package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/tracking24h"
)

// TrackingSyncService keeps an order's shipment state in step with the 24hTrack
// provider. It does three things, all of which hang off ONE identifier — the
// description we write on the provider, built from the store order id:
//
//	push    — when an order gets a tracking number, tell the provider to watch it
//	          and tag it with the store order id (see descriptionFor);
//	resolve — for an order with no tracking number, ask the provider whether a
//	          parcel was registered under that store order id;
//	sync    — refresh status + timeline for parcels that are still moving.
//
// Every method is a no-op when the integration is disabled, so callers never
// need to check first.
type TrackingSyncService struct {
	repo   *repositories.Repositories
	audit  *AuditService
	client *tracking24h.Client
	// tag prefixes every description we write, e.g. "FFM:SO-1234". The provider
	// account is shared with other tools, so the prefix is what tells our parcels
	// apart and keeps the search from matching unrelated text.
	tag     string
	resolve bool

	// runMu makes a full pass single-flight: the scheduler and a manual "sync
	// now" click must not run two passes at once and double the provider load.
	runMu sync.Mutex
}

// NewTrackingSyncService wires the service. Pass a nil client to keep the
// integration disabled (tracking then stays a purely manual field).
func NewTrackingSyncService(repo *repositories.Repositories, audit *AuditService, client *tracking24h.Client, tag string, resolve bool) *TrackingSyncService {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "FFM"
	}
	return &TrackingSyncService{repo: repo, audit: audit, client: client, tag: tag, resolve: resolve}
}

// Enabled reports whether a provider is configured.
func (s *TrackingSyncService) Enabled() bool { return s != nil && s.client != nil }

// trackable is the single gate on talking to the provider about an order.
//
// It lives here, not at each call site, because every path into the provider has
// to honour it: the scheduler, the manual button, and the fire-and-forget push
// that runs when a tracking number is recorded. An order still in production has
// no parcel with the carrier, so any call about it returns nothing useful and
// would write a misleading state onto the order.
func (s *TrackingSyncService) trackable(o *models.Order) bool {
	return s.Enabled() && o != nil && o.SellerStatus.HandedOver()
}

// descriptionFor is the provider-side identity of one of our shipments. It is
// the ONLY link between a store order id and a tracking number, so its format
// must stay stable: changing it orphans every parcel already tagged.
func (s *TrackingSyncService) descriptionFor(o *models.Order) string {
	return s.tag + ":" + strings.TrimSpace(o.StoreOrderID)
}

// ---------- push ----------

// RegisterOrder asks the provider to start tracking an order's parcel and tags
// it with the store order id. Re-registering a number the account already owns
// costs no quota — it only rewrites the description — so this is safe to call
// again whenever the tracking number changes.
//
// Nothing happens before the order is handed over to the carrier: a tracking
// number typed in while the order is still in production is ours to keep, not
// yet a parcel to watch. Handing over is what starts the journey (see
// PackingService.CreateHandoff), and registering a not-yet-shipped number would
// also spend provider quota on a parcel that resolves to nothing.
func (s *TrackingSyncService) RegisterOrder(ctx context.Context, o *models.Order) error {
	if !s.trackable(o) || strings.TrimSpace(o.TrackingNumber) == "" {
		return nil
	}
	// No carrier hint: we do not store one. The provider detects it from the
	// number itself, which is what it did for every parcel we never hinted at.
	res, err := s.client.Register(ctx, []tracking24h.RegisterItem{{
		Number:      strings.TrimSpace(o.TrackingNumber),
		Description: s.descriptionFor(o),
	}})
	if err != nil {
		return err
	}
	// A rejection is not a transport error — the number may simply be malformed —
	// but it means this parcel will never sync, so it must be visible.
	for _, r := range res.Rejected {
		log.Printf("tracking24h: provider rejected %s for order %s: %s", r.Number, o.InternalCode, r.Reason)
	}
	if res.QuotaMessage != "" {
		log.Printf("tracking24h: quota warning while registering order %s: %s", o.InternalCode, res.QuotaMessage)
	}
	return nil
}

// RegisterOrderAsync runs RegisterOrder in the background. Recording a tracking
// number at the shipping desk must not block on (or fail because of) a
// third-party HTTP call; the periodic sync picks the parcel up regardless.
func (s *TrackingSyncService) RegisterOrderAsync(o *models.Order) {
	// Note the gate here as well as inside RegisterOrder: this goroutine does two
	// things, and a silent no-op from the first would otherwise let the second
	// (syncOrder) reach the provider anyway.
	if !s.trackable(o) || strings.TrimSpace(o.TrackingNumber) == "" {
		return
	}
	snapshot := *o
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.RegisterOrder(ctx, &snapshot); err != nil {
			log.Printf("tracking24h: register order %s failed: %v", snapshot.InternalCode, err)
			return
		}
		if _, err := s.syncOrder(ctx, &snapshot); err != nil && !errors.Is(err, tracking24h.ErrNotFound) {
			log.Printf("tracking24h: first sync of order %s failed: %v", snapshot.InternalCode, err)
		}
	}()
}

// ReleaseNumberAsync clears this order's tag from a parcel it no longer uses.
//
// Without it, a corrected tracking number leaves the old parcel still answering
// to this store order id on the provider. The reverse lookup would then find two
// parcels for one order and — correctly — refuse to guess, so the order silently
// stops resolving. Releasing keeps exactly one parcel tagged per order.
//
// The read-before-write matters: if CS pasted a number belonging to ANOTHER
// order and then fixed the typo, blanking it unconditionally would strip that
// other order's tag. So the description is only cleared when it is still ours.
func (s *TrackingSyncService) ReleaseNumberAsync(o *models.Order, number string) {
	number = strings.TrimSpace(number)
	if !s.Enabled() || o == nil || number == "" {
		return
	}
	snapshot := *o
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		detail, err := s.client.Get(ctx, number)
		if err != nil {
			if !errors.Is(err, tracking24h.ErrNotFound) {
				log.Printf("tracking24h: cannot inspect released number %s: %v", number, err)
			}
			return
		}
		if !strings.EqualFold(strings.TrimSpace(detail.Description), s.descriptionFor(&snapshot)) {
			// Tagged for someone else (or already untagged) — leave it alone.
			return
		}
		if _, err := s.client.Register(ctx, []tracking24h.RegisterItem{{Number: number, Description: ""}}); err != nil {
			log.Printf("tracking24h: cannot release number %s from order %s: %v", number, snapshot.InternalCode, err)
		}
	}()
}

// ---------- resolve ----------

// ResolveOrder looks a parcel up by the order's store order id and adopts it
// when exactly one match is found. It answers the question "this order was
// uploaded — which tracking number does it belong to?".
//
// Ambiguity is left alone on purpose: if a store order id matches several
// parcels, guessing one would silently attach the wrong journey to the order.
func (s *TrackingSyncService) ResolveOrder(ctx context.Context, o *models.Order) (string, error) {
	// Same boundary as RegisterOrder: with nothing handed to a carrier there is no
	// parcel out there to match this store order id against.
	if !s.trackable(o) || strings.TrimSpace(o.StoreOrderID) == "" {
		return "", nil
	}
	if strings.TrimSpace(o.TrackingNumber) != "" {
		return o.TrackingNumber, nil
	}

	want := s.descriptionFor(o)
	// The provider's description filter is a LIKE, so "FFM:SO-12" also returns
	// "FFM:SO-123". Ask for the page, then keep only exact descriptions.
	page, err := s.client.SearchByDescription(ctx, want, 1, 100)
	if err != nil {
		return "", err
	}
	var matches []tracking24h.Detail
	for _, d := range page.Items {
		if strings.EqualFold(strings.TrimSpace(d.Description), want) && strings.TrimSpace(d.TrackingNumber) != "" {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		// Nothing yet: stamp the attempt so the scheduler moves on to other orders
		// instead of re-asking about this one on every pass.
		_ = s.repo.Order.UpdateTracking(o.ID, map[string]interface{}{"tracking_synced_at": time.Now()})
		return "", nil
	case 1:
		// fallthrough below
	default:
		log.Printf("tracking24h: store order %s matches %d parcels; leaving it for a human", o.StoreOrderID, len(matches))
		_ = s.repo.Order.UpdateTracking(o.ID, map[string]interface{}{
			"tracking_synced_at":  time.Now(),
			"tracking_sync_error": fmt.Sprintf("%d mã vận đơn cùng khớp store order này", len(matches)),
		})
		return "", nil
	}

	found := matches[0]
	o.TrackingNumber = strings.TrimSpace(found.TrackingNumber)
	// found.Carrier is deliberately dropped: the parcel's transport partner is
	// not something this system records.
	if err := s.repo.Order.UpdateTracking(o.ID, map[string]interface{}{
		"tracking_number":     o.TrackingNumber,
		"tracking_sync_error": "",
	}); err != nil {
		return "", err
	}
	s.audit.Log(Actor{}, "ORDER_TRACKING_RESOLVED", "order", &o.ID,
		fmt.Sprintf("24hTrack: đơn %s (store order %s) khớp mã vận đơn %s", o.InternalCode, o.StoreOrderID, o.TrackingNumber),
		map[string]interface{}{"tracking_number": []string{"", o.TrackingNumber}})

	// The parcel is ours now — pull its state in the same pass so the screen shows
	// a journey immediately rather than after the next tick.
	if _, err := s.applyDetail(ctx, o, &found); err != nil {
		return o.TrackingNumber, err
	}
	return o.TrackingNumber, nil
}

// ---------- sync ----------

// SyncOrderByID refreshes one order on demand (the "Đồng bộ" button). Unlike the
// scheduler it reports its error to the caller, and it resolves the tracking
// number first when the order does not have one yet.
func (s *TrackingSyncService) SyncOrderByID(ctx context.Context, actor Actor, orderID uint) (*models.Order, error) {
	if !s.Enabled() {
		return nil, apperr.Unprocessable("Tích hợp 24hTrack chưa được bật")
	}
	if !canEditTracking(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền đồng bộ tracking")
	}
	order, err := s.repo.Order.FindByID(orderID)
	if err != nil {
		return nil, apperr.NotFound("Order not found")
	}
	// The manual button must say WHY nothing happened. Silently returning an
	// unchanged order would read as "24hTrack has no data" when the real answer
	// is "this parcel has not left the factory yet".
	if !order.SellerStatus.HandedOver() {
		return nil, apperr.Unprocessable(
			"Đơn chưa bàn giao cho THE nên chưa có hành trình. Vào màn Đóng gói bấm “Bàn giao cho THE” trước.")
	}

	if strings.TrimSpace(order.TrackingNumber) == "" {
		number, rerr := s.ResolveOrder(ctx, order)
		if rerr != nil {
			return nil, apperr.Unprocessable("Không tra được 24hTrack: " + rerr.Error())
		}
		if number == "" {
			return nil, apperr.NotFound("24hTrack chưa có mã vận đơn nào gắn với store order " + order.StoreOrderID)
		}
	} else {
		// Make sure the provider knows this parcel (and its store order id) before
		// asking about it — a manually typed number may never have been registered.
		if err := s.RegisterOrder(ctx, order); err != nil {
			log.Printf("tracking24h: register during manual sync of %s failed: %v", order.InternalCode, err)
		}
		if _, err := s.syncOrder(ctx, order); err != nil {
			if errors.Is(err, tracking24h.ErrNotFound) {
				return nil, apperr.NotFound("24hTrack chưa có dữ liệu cho mã vận đơn " + order.TrackingNumber)
			}
			return nil, apperr.Unprocessable("Không đồng bộ được 24hTrack: " + err.Error())
		}
	}

	fresh, err := s.repo.Order.FindByID(orderID)
	if err != nil {
		return nil, apperr.Internal("could not reload order").Wrap(err)
	}
	return fresh, nil
}

// syncOrder pulls the provider's current view of one parcel and writes it back.
// It returns whether the normalized status changed.
func (s *TrackingSyncService) syncOrder(ctx context.Context, o *models.Order) (bool, error) {
	// Last line of defence. Every provider call funnels through here, so even a
	// caller that forgets the gate cannot pull a carrier state onto an order whose
	// parcel has not been handed over.
	if !s.trackable(o) {
		return false, nil
	}
	detail, err := s.client.Get(ctx, strings.TrimSpace(o.TrackingNumber))
	if err != nil {
		s.recordSyncError(o, err)
		return false, err
	}
	return s.applyDetail(ctx, o, detail)
}

// applyDetail writes a provider snapshot onto the order and stores its timeline.
func (s *TrackingSyncService) applyDetail(ctx context.Context, o *models.Order, d *tracking24h.Detail) (bool, error) {
	now := time.Now()
	status := normalizeProviderStatus(d.Status)
	changed := status != o.TrackingStatus

	fields := map[string]interface{}{
		"tracking_status":     status,
		"tracking_detail":     truncate(d.Detail, 500),
		"tracking_location":   truncate(d.Location, 255),
		"tracking_raw_status": truncate(firstNonEmpty(d.RawStatus, d.Status), 120),
		"tracking_synced_at":  now,
		"tracking_sync_error": "",
	}
	// d.Carrier is read and thrown away on purpose — see the model.
	if o.TrackingURL == "" {
		fields["tracking_url"] = publicTrackingURL(o.TrackingNumber)
	}
	if d.DeliveredAt != nil {
		fields["tracking_delivered_at"] = *d.DeliveredAt
	}
	// tracking_updated_at is the business-visible "khi nào trạng thái đổi", not
	// "khi nào ta gọi provider" — only move it when the status actually changed.
	if changed {
		fields["tracking_updated_at"] = now
	}
	if err := s.repo.Order.UpdateTracking(o.ID, fields); err != nil {
		return false, err
	}
	o.TrackingStatus = status

	// The carrier's word is also news about the ORDER, not just about the parcel.
	// Skipping this was the bug behind an order sitting at "Đã bàn giao" while the
	// tracking badge beside it already read "Đã giao".
	s.advanceSellerStatus(o, status)

	if err := s.saveTimeline(ctx, o); err != nil {
		// A missing timeline is not worth failing the status update over.
		log.Printf("tracking24h: timeline of order %s failed: %v", o.InternalCode, err)
	}
	return changed, nil
}

// sellerStatusFor maps a carrier state onto the order lifecycle, or returns ""
// when the state says nothing about how far the order has got.
//
// Deliberately not exhaustive over TrackingStatus: UNDELIVERED / EXCEPTION /
// EXPIRED / CANCELLED are all trouble with a parcel that is already in the
// carrier's hands. None of them means the order moved forward, and none of them
// should quietly move it backward either — they are shown through the tracking
// badge, which is the field that exists to describe the parcel's own problems.
func sellerStatusFor(t models.TrackingStatus) models.SellerStatus {
	switch t {
	case models.TrackingDelivered:
		return models.SellerStatusDelivered
	case models.TrackingInTransit, models.TrackingOutForDelivery, models.TrackingPickUp:
		// The parcel is demonstrably moving, so it has shipped — whatever the
		// shipping desk did or did not click.
		return models.SellerStatusShipped
	}
	// PENDING / PRE_TRANSIT: the number is registered but nobody has scanned the
	// parcel yet. HANDED_OFF already says exactly that.
	return ""
}

// advanceSellerStatus moves the order forward when the carrier says so. Failures
// are logged, not returned: the tracking snapshot itself is already saved, and
// losing the whole sync over a bookkeeping write would be worse than a status
// that catches up on the next pass.
func (s *TrackingSyncService) advanceSellerStatus(o *models.Order, t models.TrackingStatus) {
	next := sellerStatusFor(t)
	if next == "" {
		return
	}
	// Forward only. A return-leg scan on a delivered parcel, or a provider that
	// replays an older state, must not walk the order back down the timeline.
	if next.Rank() <= o.SellerStatus.Rank() {
		return
	}
	// A cancelled order keeps the state it was cancelled in: its paper trail says
	// how far it got before being pulled, and a late carrier scan must not rewrite
	// that into a delivery the seller was told was cancelled.
	if o.ReviewStatus == models.ReviewCancelled {
		return
	}
	from := o.SellerStatus
	// Compare-and-set on the old value: the scheduled sweep, a manual sync and the
	// shipping desk can all touch the same order, and a plain UPDATE would let a
	// slow worker overwrite a newer status with an older one.
	ok, err := s.repo.Order.UpdateSellerStatusIf(o.ID, from, next)
	if err != nil {
		log.Printf("tracking24h: advance %s %s→%s failed: %v", o.InternalCode, from, next, err)
		return
	}
	if !ok {
		// Someone else moved it first; their value is at least as new as ours.
		return
	}
	o.SellerStatus = next
	// The carrier moving the parcel proves it left the factory; stamp the
	// factory-exit moment for orders that never got it from a handoff (the
	// write is a no-op when handed_over_at is already set).
	if o.HandedOverAt == nil && next.HandedOver() {
		handedAt := time.Now()
		if err := s.repo.Order.StampHandedOverAt(o.ID, handedAt); err != nil {
			log.Printf("tracking24h: stamp handed_over_at for %s failed: %v", o.InternalCode, err)
		} else {
			o.HandedOverAt = &handedAt
		}
	}
	// Actor is zero (system): no human witnessed this, the carrier reported it.
	// The seller's order history renders that as "Hệ thống".
	_ = recordStatus(s.repo, models.EntityOrder, o.ID, string(from), string(next), Actor{},
		"theo trạng thái vận chuyển")
}

// saveTimeline mirrors the provider's event list for the order's current parcel.
func (s *TrackingSyncService) saveTimeline(ctx context.Context, o *models.Order) error {
	number := strings.TrimSpace(o.TrackingNumber)
	events, err := s.client.Events(ctx, number)
	if err != nil {
		if errors.Is(err, tracking24h.ErrNotFound) {
			return nil
		}
		return err
	}
	rows := make([]models.OrderTrackingEvent, 0, len(events))
	for _, e := range events {
		desc := strings.TrimSpace(e.Description)
		raw := strings.TrimSpace(e.EventDate)
		if desc == "" && raw == "" {
			continue
		}
		rows = append(rows, models.OrderTrackingEvent{
			OrderID:        o.ID,
			TrackingNumber: number,
			EventAt:        parseProviderTime(raw),
			RawDate:        truncate(raw, 120),
			Location:       truncate(strings.TrimSpace(e.Location), 255),
			Description:    truncate(desc, 500),
			StatusHint:     truncate(strings.TrimSpace(e.StatusHint), 120),
			Fingerprint:    models.EventFingerprint(number, raw, desc, e.Location),
		})
	}
	return s.repo.Tracking.SaveEvents(rows)
}

// recordSyncError stamps the failure on the order so an operator can see WHY a
// parcel stopped updating, instead of silently stale data.
func (s *TrackingSyncService) recordSyncError(o *models.Order, err error) {
	msg := err.Error()
	if errors.Is(err, tracking24h.ErrNotFound) {
		msg = "24hTrack chưa có dữ liệu cho mã vận đơn này"
	}
	_ = s.repo.Order.UpdateTracking(o.ID, map[string]interface{}{
		"tracking_synced_at":  time.Now(),
		"tracking_sync_error": truncate(msg, 255),
	})
}

// ---------- batch pass ----------

// SyncStats reports what one pass did, for the log line and the manual-run
// response.
type SyncStats struct {
	Resolved int `json:"resolved"`
	Synced   int `json:"synced"`
	Changed  int `json:"changed"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
	// Reconciled: orders whose seller status was pulled back in line with a
	// tracking status the provider had already reported. No provider call involved.
	Reconciled int `json:"reconciled"`
}

// RunOnce performs a full pass: adopt parcels for untracked orders, then refresh
// the ones already tracked. batchSize caps the refresh, which is what keeps the
// pass inside the provider's per-minute rate limit.
//
// It never returns an error: a pass is best-effort by nature (a provider outage
// must not stop the scheduler), and per-order failures are recorded on the
// orders themselves.
func (s *TrackingSyncService) RunOnce(ctx context.Context, batchSize int) SyncStats {
	var stats SyncStats
	if !s.Enabled() {
		return stats
	}
	// A pass that is still running when the next tick arrives must be skipped, not
	// queued: overlapping passes would double the provider load for no gain.
	if !s.runMu.TryLock() {
		log.Println("tracking24h: previous sync pass still running; skipping this tick")
		return stats
	}
	defer s.runMu.Unlock()

	if s.resolve {
		// Only orders from the last 60 days: an order that never grew a tracking
		// number in two months never will, and asking forever would crowd out the
		// fresh ones this pass is really for.
		cutoff := time.Now().AddDate(0, 0, -60)
		pending, err := s.repo.Tracking.AwaitingTrackingNumber(resolveBudget(batchSize), cutoff)
		if err != nil {
			log.Printf("tracking24h: cannot list orders awaiting tracking: %v", err)
		}
		for i := range pending {
			if ctx.Err() != nil {
				return stats
			}
			number, err := s.ResolveOrder(ctx, &pending[i])
			switch {
			case err != nil:
				stats.Failed++
				log.Printf("tracking24h: resolve order %s failed: %v", pending[i].InternalCode, err)
			case number != "":
				stats.Resolved++
			default:
				stats.Skipped++
			}
		}
	}

	// Re-check a parcel at most once per hour even when the batch has room: the
	// carrier scans it a handful of times a day, so anything faster is waste.
	due, err := s.repo.Tracking.DueForSync(batchSize, time.Now().Add(-time.Hour))
	if err != nil {
		log.Printf("tracking24h: cannot list orders due for sync: %v", err)
		return stats
	}
	for i := range due {
		if ctx.Err() != nil {
			return stats
		}
		changed, err := s.syncOrder(ctx, &due[i])
		if err != nil {
			stats.Failed++
			continue
		}
		stats.Synced++
		if changed {
			stats.Changed++
		}
	}

	// Catch up any order the provider has already finished with. These never come
	// back through DueForSync (a delivered parcel is excluded from it for good), so
	// without this pass an order that reached DELIVERED without its seller status
	// following would stay wrong forever. Costs no provider call.
	stats.Reconciled = s.reconcileSellerStatus(batchSize)
	return stats
}

// reconcileSellerStatus advances orders whose seller status trails the tracking
// status already stored on them, and reports how many moved.
//
// Pure bookkeeping over our own columns — no provider call, so it is safe to run
// on every pass and cannot spend quota. advanceSellerStatus applies the same
// guards as the live path (forward-only, skip cancelled, compare-and-set, write
// history), so an order fixed here is indistinguishable from one that got the
// status at the moment the carrier reported it.
func (s *TrackingSyncService) reconcileSellerStatus(limit int) int {
	lagging, err := s.repo.Tracking.SellerStatusLagging(limit)
	if err != nil {
		log.Printf("tracking24h: cannot list orders with lagging seller status: %v", err)
		return 0
	}
	fixed := 0
	for i := range lagging {
		o := &lagging[i]
		before := o.SellerStatus
		s.advanceSellerStatus(o, o.TrackingStatus)
		if o.SellerStatus != before {
			fixed++
		}
	}
	if fixed > 0 {
		log.Printf("tracking24h: reconciled seller status on %d order(s)", fixed)
	}
	return fixed
}

// RunOnceForActor is the manual trigger behind POST /api/tracking/sync.
func (s *TrackingSyncService) RunOnceForActor(ctx context.Context, actor Actor, batchSize int) (SyncStats, error) {
	if !s.Enabled() {
		return SyncStats{}, apperr.Unprocessable("Tích hợp 24hTrack chưa được bật")
	}
	if !canEditTracking(actor) {
		return SyncStats{}, apperr.Forbidden("Bạn không có quyền đồng bộ tracking")
	}
	return s.RunOnce(ctx, batchSize), nil
}

// Timeline returns the journey of the parcel the order is CURRENTLY tracking.
//
// The order's own tracking number decides what comes back. Scans collected under
// a previous number stay in the table (they are the evidence for a claim against
// the carrier) but never appear beside the parcel now in flight — splicing two
// shipments into one history is worse than showing none.
func (s *TrackingSyncService) Timeline(orderID uint) ([]models.OrderTrackingEvent, error) {
	order, err := s.repo.Order.FindByID(orderID)
	if err != nil {
		return nil, apperr.NotFound("Order not found")
	}
	return s.timelineFor(order)
}

func (s *TrackingSyncService) timelineFor(order *models.Order) ([]models.OrderTrackingEvent, error) {
	rows, err := s.repo.Tracking.EventsByOrder(order.ID, order.TrackingNumber)
	if err != nil {
		return nil, apperr.Internal("could not load tracking events").Wrap(err)
	}
	return rows, nil
}

// SellerTimeline is the same journey, but only for an order the seller owns.
// The ownership check lives here rather than in the handler because a tracking
// event names the destination city — enumerating order ids would otherwise leak
// where other sellers' customers live.
func (s *TrackingSyncService) SellerTimeline(sellerID, orderID uint) ([]models.OrderTrackingEvent, error) {
	order, err := s.repo.Order.FindByID(orderID)
	if err != nil {
		return nil, apperr.NotFound("Order not found")
	}
	if order.SellerID != sellerID {
		return nil, apperr.Forbidden("This order does not belong to your seller account")
	}
	// Reuses the already-loaded order rather than re-reading it through Timeline.
	events, err := s.timelineFor(order)
	if err != nil {
		return nil, err
	}
	// Scan text is free-form and some transport partners stamp their own name into
	// it. Ops reads the provider's original wording; the seller does not.
	return redactPartnerEvents(events), nil
}

// ---------- helpers ----------

// resolveBudget splits the pass budget: refreshing live parcels is the primary
// job, so the reverse lookup gets a quarter of it (at least one order).
func resolveBudget(batchSize int) int {
	n := batchSize / 4
	if n < 1 {
		n = 1
	}
	return n
}

// normalizeProviderStatus maps 24hTrack's normalized status names onto our own
// enum. Unknown/blank values become PENDING rather than an invalid status, so a
// new provider value can never write garbage into the column.
func normalizeProviderStatus(s string) models.TrackingStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "delivered":
		return models.TrackingDelivered
	case "in transit":
		return models.TrackingInTransit
	case "out for delivery":
		return models.TrackingOutForDelivery
	case "pick up", "pickup", "available for pickup":
		return models.TrackingPickUp
	case "info received":
		return models.TrackingPreTransit
	case "undelivered":
		return models.TrackingUndelivered
	case "alert", "exception":
		return models.TrackingException
	case "expired":
		return models.TrackingExpired
	default:
		// "Not Found", "Queued", "Pending" and anything new: the parcel exists on
		// the provider but has no carrier state yet.
		return models.TrackingPending
	}
}

// publicTrackingURL is the customer-facing page for a parcel on 24hTrack. The
// site takes the number as a query parameter (?ids=), not a path segment.
func publicTrackingURL(number string) string {
	number = strings.TrimSpace(number)
	if number == "" {
		return ""
	}
	return "https://www.24htrack.com/track?ids=" + url.QueryEscape(number)
}

// providerTimeRe matches the human date strings carriers hand back, e.g.
// "July 27, 2026 9:53 PM", "March 17, 2026, 2:15 pm" or a bare "July 27, 2026".
// Anything trailing (some carriers append "Shipping Partner: …") is ignored.
var providerTimeRe = regexp.MustCompile(`(?i)^([A-Za-z]+)\s+(\d{1,2}),\s*(\d{4})(?:,)?(?:\s+(\d{1,2}):(\d{2})(?::(\d{2}))?\s*([AP]M)?)?`)

var monthByName = map[string]time.Month{
	"january": time.January, "february": time.February, "march": time.March,
	"april": time.April, "may": time.May, "june": time.June, "july": time.July,
	"august": time.August, "september": time.September, "october": time.October,
	"november": time.November, "december": time.December,
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"jun": time.June, "jul": time.July, "aug": time.August, "sep": time.September,
	"sept": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}

// parseProviderTime turns a carrier scan date into a sortable timestamp, or nil
// when it cannot be understood.
//
// The value is deliberately built in UTC and NOT converted: carriers report
// local facility time with no zone, so any conversion would invent an offset.
// The UI shows RawDate; this timestamp exists only to order the timeline.
func parseProviderTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// ISO first — some provider fields (and any future API change) use it.
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return &t
		}
	}

	m := providerTimeRe.FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	month, ok := monthByName[strings.ToLower(m[1])]
	if !ok {
		return nil
	}
	day, err := strconv.Atoi(m[2])
	if err != nil {
		return nil
	}
	year, err := strconv.Atoi(m[3])
	if err != nil {
		return nil
	}
	hour, minute, sec := 0, 0, 0
	if m[4] != "" {
		hour, _ = strconv.Atoi(m[4])
		minute, _ = strconv.Atoi(m[5])
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
		switch strings.ToUpper(m[7]) {
		case "PM":
			if hour < 12 {
				hour += 12
			}
		case "AM":
			if hour == 12 {
				hour = 0
			}
		}
	}
	t := time.Date(year, month, day, hour, minute, sec, 0, time.UTC)
	return &t
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary so the column never stores a broken UTF-8 sequence.
	for max > 0 && !utf8ValidCut(s, max) {
		max--
	}
	return s[:max]
}

// utf8ValidCut reports whether s[:i] ends on a rune boundary.
func utf8ValidCut(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
