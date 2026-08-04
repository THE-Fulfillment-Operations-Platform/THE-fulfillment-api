package services

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// ReviewService implements the operational intake review of newly uploaded /
// imported orders (approve / reject / request correction) and the cancellation
// workflow (seller direct cancel, seller cancellation request, and the OPS/ADMIN
// resolution of such requests). It never touches the production status machine;
// it only decides whether an order is allowed to reach it.
type ReviewService struct {
	repo  *repositories.Repositories
	audit *AuditService
}

// ---------- Cancellation rule engine (pure, unit-testable) ----------

// SellerCancelAction classifies what a seller may do with one of their orders.
// It is the single source of truth behind both the API guards and the buttons
// the seller UI shows, so the two can never disagree.
type SellerCancelAction string

const (
	// SellerActionCancel: nothing has been produced yet — the seller cancels
	// outright, it takes effect immediately and nothing is charged.
	SellerActionCancel SellerCancelAction = "CANCEL"
	// SellerActionRequest: production has already started (or finished) — the
	// seller may only ASK to cancel. Ops/Admin decide, and the order stays
	// billable because the work was already done.
	SellerActionRequest SellerCancelAction = "REQUEST"
	// SellerActionNone: no action available (already cancelled/rejected, or a
	// cancellation request is already pending).
	SellerActionNone SellerCancelAction = "NONE"
)

// orderPacked reports whether an order has advanced to packing or beyond.
// Rank-based rather than an explicit list: a new late-lifecycle status (DELIVERED
// was one) is "packed or beyond" by definition, and a list would have silently
// answered false for it.
func orderPacked(seller models.SellerStatus) bool {
	return seller.Rank() >= models.SellerStatusPacked.Rank()
}

// orderInProduction reports whether an approved order already has production
// work in flight: any live item scheduled into a batch or advanced past PENDING.
// Already-cancelled lines don't count — they are history, not work. Requires the
// order's Items (and, for precision, their BatchItems) preloaded; the list path
// uses OrderRepository.InProductionIDs instead of preloading a page of them.
func orderInProduction(o *models.Order) bool {
	for _, it := range o.Items {
		if itemCancelled(it.CancellationStatus) {
			continue
		}
		if it.InternalStatus != models.StatusPending {
			return true
		}
		if len(it.BatchItems) > 0 {
			return true
		}
	}
	return false
}

// orderCancelStage snapshots how far an order has progressed. Everything about a
// cancellation follows from it: whether the seller may cancel outright or has to
// ask, and whether the customer is charged anyway.
func orderCancelStage(seller models.SellerStatus, inProduction bool) models.CancelStage {
	switch seller {
	// Delivered is past shipped, not a separate cancellation stage — there is no
	// "more cancelled than shipped". Both bill in full.
	case models.SellerStatusShipped, models.SellerStatusDelivered:
		return models.CancelStageShipped
	case models.SellerStatusPacked, models.SellerStatusHandedOff:
		return models.CancelStagePacked
	}
	if inProduction {
		return models.CancelStageInProduction
	}
	return models.CancelStagePreProduction
}

// orderCancelStageFor is the order-level convenience wrapper (items preloaded).
func orderCancelStageFor(o *models.Order) models.CancelStage {
	return orderCancelStage(o.SellerStatus, orderInProduction(o))
}

// sellerCancelAction computes the cancellation action available to the seller.
//
// The whole policy is two lines: before production the seller owns the order and
// cancels it themselves; from the first print onward the factory has spent
// material and machine time, so a human approves the cancellation and the order
// is still invoiced. Cancellation therefore stays available across the ENTIRE
// lifecycle — a packed or shipped order can still be requested (Ops/Admin resolve
// it as a claim) instead of leaving the seller with no button and a phone call.
func sellerCancelAction(review models.ReviewStatus, cancellation models.CancellationStatus, stage models.CancelStage) SellerCancelAction {
	// A request already in flight leaves the seller nothing to do but wait.
	if cancellation == models.CancellationRequested {
		return SellerActionNone
	}
	switch review {
	case models.ReviewPending, models.ReviewNeedsFix, models.ReviewApproved:
		if stage.NeedsApproval() {
			return SellerActionRequest
		}
		return SellerActionCancel
	default: // REJECTED, CANCELLED
		return SellerActionNone
	}
}

// sellerCancelActionForOrder is the order-level convenience wrapper.
func sellerCancelActionForOrder(o *models.Order) SellerCancelAction {
	return sellerCancelAction(o.ReviewStatus, o.CancellationStatus, orderCancelStageFor(o))
}

// isReviewable reports whether an order is in a state a reviewer can act on.
func isReviewable(status models.ReviewStatus) bool {
	return status == models.ReviewPending || status == models.ReviewNeedsFix
}

// ---------- Loading ----------

func (s *ReviewService) getOrder(id uint) (*models.Order, error) {
	o, err := s.repo.Order.FindByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	return o, nil
}

func (s *ReviewService) getSellerOrder(sellerID, orderID uint) (*models.Order, error) {
	order, err := s.getOrder(orderID)
	if err != nil {
		return nil, err
	}
	if order.SellerID != sellerID {
		return nil, apperr.Forbidden("This order does not belong to your seller account")
	}
	return order, nil
}

// ---------- Review queue ----------

// ListReviewOrders lists orders in the review queue. With no explicit review
// status filter it returns PENDING_REVIEW + NEEDS_CORRECTION.
func (s *ReviewService) ListReviewOrders(f repositories.OrderFilter) ([]models.Order, int64, error) {
	f.Page = f.Page.Normalize()
	if f.ReviewStatus == "" && len(f.ReviewStatuses) == 0 {
		f.ReviewStatuses = []string{string(models.ReviewPending), string(models.ReviewNeedsFix)}
	}
	rows, total, err := s.repo.Order.List(f)
	if err != nil {
		return rows, total, err
	}
	if err := annotateStoreOrderDupSlice(s.repo, rows); err != nil {
		return rows, total, err
	}
	return rows, total, nil
}

// ReviewIssue is a single validation finding surfaced to the reviewer so they can
// judge SKU/material mapping, mockup/design links, quantity and shipping data.
type ReviewIssue struct {
	ItemID   uint   `json:"item_id,omitempty"`
	ItemCode string `json:"item_code,omitempty"`
	SKUCode  string `json:"sku_code,omitempty"`
	Field    string `json:"field"`
	Severity string `json:"severity"` // BLOCKER | WARNING
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// ReviewOrderDetail bundles an order with its computed validation issues.
type ReviewOrderDetail struct {
	Order  *models.Order `json:"order"`
	Issues []ReviewIssue `json:"issues"`
}

// GetReviewOrder returns a full order plus a list of validation issues (SKU
// mapping, material mapping, mockup/design links, quantity, shipping) so the
// reviewer can approve, reject or request a correction with full context.
func (s *ReviewService) GetReviewOrder(id uint) (*ReviewOrderDetail, error) {
	order, err := s.getOrder(id)
	if err != nil {
		return nil, err
	}
	issues := s.reviewIssues(order)
	// The review screen is an operational queue, not an audit history. Exclude
	// cancelled line items so Ops only reviews products that can still proceed.
	activeItems := make([]models.OrderItem, 0, len(order.Items))
	for _, item := range order.Items {
		if !itemCancelled(item.CancellationStatus) {
			activeItems = append(activeItems, item)
		}
	}
	order.Items = activeItems
	return &ReviewOrderDetail{Order: order, Issues: issues}, nil
}

func (s *ReviewService) reviewIssues(order *models.Order) []ReviewIssue {
	// One query for every item's mapped-material count (instead of a COUNT per
	// item); the pure validation below is then database-free.
	skuIDs := make([]uint, 0, len(order.Items))
	for _, it := range order.Items {
		if it.SKUID != nil && !itemCancelled(it.CancellationStatus) {
			skuIDs = append(skuIDs, *it.SKUID)
		}
	}
	materialCounts, err := s.repo.SKU.MaterialCounts(skuIDs)
	if err != nil {
		// Degrade the same way the old per-item probe did on error: skip the
		// material-mapping check rather than fail the whole review screen.
		materialCounts = nil
	}
	return reviewIssuesWith(order, materialCounts)
}

// reviewIssuesWith is the pure (no-DB) review validation given a precomputed
// SKU->material-count map. Splitting the DB lookup out lets the bulk-approve path
// compute material counts ONCE for every order's items and then validate each
// order in memory. A nil materialCounts skips the material-mapping check.
func reviewIssuesWith(order *models.Order, materialCounts map[uint]int64) []ReviewIssue {
	issues := make([]ReviewIssue, 0)

	// Order-level: shipping data.
	if strings.TrimSpace(order.ShippingName) == "" ||
		strings.TrimSpace(order.ShippingAddress1) == "" ||
		strings.TrimSpace(order.ShippingCountry) == "" {
		issues = append(issues, ReviewIssue{
			Field: "shipping", Severity: "BLOCKER", Code: "ADDR_INVALID",
			Message: "Thiếu thông tin giao hàng (tên, địa chỉ 1 hoặc quốc gia).",
		})
	}

	// Item-level: SKU/material mapping, mockup, design, quantity.
	for _, it := range order.Items {
		if itemCancelled(it.CancellationStatus) {
			continue
		}
		base := ReviewIssue{ItemID: it.ID, ItemCode: it.InternalCode, SKUCode: it.SKUCode}
		if it.Quantity < 1 {
			iss := base
			iss.Field, iss.Severity, iss.Code = "quantity", "BLOCKER", "QTY_INVALID"
			iss.Message = "Số lượng phải >= 1."
			issues = append(issues, iss)
		}
		if it.SKUID == nil {
			iss := base
			iss.Field, iss.Severity, iss.Code = "sku", "BLOCKER", "SKU_UNMAPPED"
			iss.Message = "SKU chưa có trong master data (chưa được setup nguyên vật liệu)."
			issues = append(issues, iss)
		} else if materialCounts != nil && materialCounts[*it.SKUID] == 0 {
			iss := base
			iss.Field, iss.Severity, iss.Code = "material", "BLOCKER", "SKU_NO_MATERIAL"
			iss.Message = "SKU chưa được gán nguyên vật liệu (Loại VL)."
			issues = append(issues, iss)
		}
		if strings.TrimSpace(it.MockupURL) == "" {
			iss := base
			iss.Field, iss.Severity, iss.Code = "mockup", "WARNING", "MOCKUP_MISSING"
			iss.Message = "Thiếu mockup để QC đối chiếu."
			issues = append(issues, iss)
		}
		if strings.TrimSpace(it.DesignURL) == "" {
			iss := base
			iss.Field, iss.Severity, iss.Code = "design", "WARNING", "DESIGN_MISSING"
			iss.Message = "Thiếu link design/print file."
			issues = append(issues, iss)
		}
	}
	return issues
}

// ---------- Review transitions (OPS / ADMIN / DESIGNER) ----------

// transitionReview moves an order to a new review status, stamps the reviewer
// metadata, records status history and writes an audit entry.
func (s *ReviewService) transitionReview(actor Actor, order *models.Order, to models.ReviewStatus, note, action, summary string) (*models.Order, error) {
	from := order.ReviewStatus
	now := time.Now()
	order.ReviewStatus = to
	order.ReviewedByID = actor.IDPtr()
	order.ReviewedAt = &now
	order.ReviewNote = strings.TrimSpace(note)
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not update order review status").Wrap(err)
	}
	_ = recordStatus(s.repo, models.EntityOrder, order.ID, string(from), string(to), actor, note)
	s.audit.Log(actor, action, "order", &order.ID, summary+" "+order.InternalCode,
		models.JSONMap{"from": string(from), "to": string(to), "note": note})
	return s.getOrder(order.ID)
}

// Approve releases an order into the design/production flow. It enforces the
// same blocking validation the review UI shows (missing shipping/quantity,
// unmapped SKU, SKU without material) server-side, so an order can never be
// approved with an unresolved blocker via a direct API call.
func (s *ReviewService) Approve(actor Actor, id uint, note string) (*models.Order, error) {
	order, err := s.getOrder(id)
	if err != nil {
		return nil, err
	}
	if !isReviewable(order.ReviewStatus) {
		return nil, apperr.Conflict("Only orders pending review or needing correction can be approved")
	}
	for _, iss := range s.reviewIssues(order) {
		if iss.Severity == "BLOCKER" {
			return nil, apperr.Unprocessable("Không thể duyệt: còn lỗi chặn cần xử lý — " + iss.Message)
		}
	}
	return s.transitionReview(actor, order, models.ReviewApproved, note, "REVIEW_APPROVE", "Approved order")
}

// Reject marks an order as rejected; it will never be produced.
func (s *ReviewService) Reject(actor Actor, id uint, note string) (*models.Order, error) {
	order, err := s.getOrder(id)
	if err != nil {
		return nil, err
	}
	if !isReviewable(order.ReviewStatus) {
		return nil, apperr.Conflict("Only orders pending review or needing correction can be rejected")
	}
	return s.transitionReview(actor, order, models.ReviewRejected, note, "REVIEW_REJECT", "Rejected order")
}

// RequestCorrection sends an order back to the seller for correction.
func (s *ReviewService) RequestCorrection(actor Actor, id uint, note string) (*models.Order, error) {
	order, err := s.getOrder(id)
	if err != nil {
		return nil, err
	}
	if !isReviewable(order.ReviewStatus) {
		return nil, apperr.Conflict("Only orders pending review or needing correction can be sent back")
	}
	return s.transitionReview(actor, order, models.ReviewNeedsFix, note, "REVIEW_REQUEST_CORRECTION", "Requested correction on order")
}

// ---------- Bulk approve (OPS / ADMIN / DESIGNER) ----------

// BulkApproveInput is the body for approving several orders at once.
type BulkApproveInput struct {
	OrderIDs []uint `json:"order_ids" binding:"required,min=1"`
	Note     string `json:"note"`
}

// BulkSkip explains why one order in a bulk operation was not approved.
type BulkSkip struct {
	OrderID uint   `json:"order_id"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
}

// BulkApproveResult reports the outcome of a bulk approve: which orders were
// approved and which were skipped (with a reason), so the UI can show a clear,
// partial-success summary.
type BulkApproveResult struct {
	Approved      []uint     `json:"approved"`
	ApprovedCount int        `json:"approved_count"`
	Skipped       []BulkSkip `json:"skipped"`
	SkippedCount  int        `json:"skipped_count"`
}

// BulkApprove approves every order in ids that is still reviewable and has no
// blocking validation issue, skipping the rest with a reason. Each order's
// approval is independent (partial success is intentional): a not-found, already
// decided, or blocked order never prevents the others from being approved. The
// same server-side BLOCKER check as the single-order Approve is enforced per
// order, so a bulk call can never bypass validation or approve an order that is
// not in the caller's review scope.
func (s *ReviewService) BulkApprove(actor Actor, in BulkApproveInput) (*BulkApproveResult, error) {
	res := &BulkApproveResult{Approved: []uint{}, Skipped: []BulkSkip{}}
	note := strings.TrimSpace(in.Note)

	// De-duplicate the requested ids, preserving order and dropping zeros.
	ids := make([]uint, 0, len(in.OrderIDs))
	seen := map[uint]bool{}
	for _, id := range in.OrderIDs {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return res, nil
	}

	// 1. Load every requested order + its items in bulk (2 queries) instead of the
	//    old per-order FindByID (~6 queries each). On a remote database this is the
	//    single biggest win: query count no longer scales with the number of orders.
	orders, err := s.repo.Order.FindByIDsForReview(ids)
	if err != nil {
		return nil, apperr.Internal("could not load orders for bulk approve").Wrap(err)
	}
	byID := make(map[uint]*models.Order, len(orders))
	for i := range orders {
		byID[orders[i].ID] = &orders[i]
	}

	// 2. Material counts for every item across ALL orders in ONE query, so the
	//    blocker check below is evaluated purely in memory (no per-order DB call).
	skuSeen := map[uint]bool{}
	skuIDs := make([]uint, 0)
	for i := range orders {
		for _, it := range orders[i].Items {
			if it.SKUID != nil && !itemCancelled(it.CancellationStatus) && !skuSeen[*it.SKUID] {
				skuSeen[*it.SKUID] = true
				skuIDs = append(skuIDs, *it.SKUID)
			}
		}
	}
	materialCounts, err := s.repo.SKU.MaterialCounts(skuIDs)
	if err != nil {
		materialCounts = nil // degrade: skip the material-mapping blocker check
	}

	// 3. Decide each order in memory: approvable, or skipped with a reason. Same
	//    guards and BLOCKER validation as the single-order Approve — a bulk call
	//    can never bypass validation.
	// Just the ids: the history rows read each order's pre-approval status straight
	// out of the orders table, so there is nothing else to carry forward.
	approvedIDs := make([]uint, 0, len(ids))
	for _, id := range ids {
		order, ok := byID[id]
		if !ok {
			res.Skipped = append(res.Skipped, BulkSkip{OrderID: id, Code: "NOT_FOUND", Reason: "Đơn không tồn tại hoặc đã bị xoá"})
			continue
		}
		if !isReviewable(order.ReviewStatus) {
			res.Skipped = append(res.Skipped, BulkSkip{
				OrderID: id, Code: "NOT_REVIEWABLE",
				Reason: "Đơn không ở trạng thái chờ duyệt/cần sửa (hiện tại: " + string(order.ReviewStatus) + ")",
			})
			continue
		}
		if order.CancellationStatus == models.CancellationRequested {
			res.Skipped = append(res.Skipped, BulkSkip{OrderID: id, Code: "CANCEL_PENDING", Reason: "Đơn đang có yêu cầu huỷ chờ xử lý"})
			continue
		}
		blocked := ""
		for _, iss := range reviewIssuesWith(order, materialCounts) {
			if iss.Severity == "BLOCKER" {
				blocked = iss.Message
				break
			}
		}
		if blocked != "" {
			res.Skipped = append(res.Skipped, BulkSkip{OrderID: id, Code: "HAS_BLOCKER", Reason: "Còn lỗi chặn: " + blocked})
			continue
		}
		approvedIDs = append(approvedIDs, id)
	}

	// 4. Persist every approval in ONE transaction of three constant-size
	//    statements — history INSERT…SELECT, batch UPDATE, audit INSERT —
	//    regardless of how many orders are being approved. Round trips, not rows,
	//    are what this endpoint pays for: the database is remote, so a statement
	//    that carries a row per order (the old bulk history insert did) cost more
	//    than a second on its own once the selection grew past a couple hundred.
	if len(approvedIDs) > 0 {
		now := time.Now()
		txErr := s.repo.DB.Transaction(func(tx *gorm.DB) error {
			txRepo := repositories.New(tx)
			// History FIRST: it reads each order's pre-approval review_status out of
			// the orders table, which the UPDATE below is about to overwrite.
			if err := txRepo.Status.RecordEntityTransition(
				models.EntityOrder, string(models.ReviewApproved), actor.IDPtr(), note, now,
				txRepo.Order.ReviewApprovableSource(approvedIDs),
			); err != nil {
				return err
			}
			if err := txRepo.Order.BulkSetReviewApproved(approvedIDs, actor.IDPtr(), note, now); err != nil {
				return err
			}
			// The audit entry joins the same transaction instead of paying its own
			// round trip afterwards — and can no longer record an approval that then
			// failed to commit.
			return writeBulkApproveAudit(txRepo, actor, approvedIDs, len(res.Skipped))
		})
		if txErr != nil {
			return nil, apperr.Internal("could not approve orders").Wrap(txErr)
		}
		res.Approved = approvedIDs
		res.ApprovedCount = len(res.Approved)
		res.SkippedCount = len(res.Skipped)
		return res, nil
	}

	res.ApprovedCount = len(res.Approved)
	res.SkippedCount = len(res.Skipped)
	// Nothing was approved, so there is no transaction to ride along in; still
	// record the attempt.
	s.audit.Log(actor, "REVIEW_BULK_APPROVE", "order", nil,
		fmt.Sprintf("Bulk approve: %d duyệt, %d bỏ qua", res.ApprovedCount, res.SkippedCount),
		models.JSONMap{"approved": res.Approved, "skipped_count": res.SkippedCount})
	return res, nil
}

// writeBulkApproveAudit writes the bulk-approve audit entry on the transaction's
// own connection, mirroring AuditService.Log's row without paying a separate
// round trip for it.
func writeBulkApproveAudit(txRepo *repositories.Repositories, actor Actor, approved []uint, skipped int) error {
	meta, _ := models.ToJSONB(models.JSONMap{"approved": approved, "skipped_count": skipped})
	return txRepo.Audit.Create(&models.AuditLog{
		ActorID:    actor.IDPtr(),
		ActorEmail: actor.Email,
		Action:     "REVIEW_BULK_APPROVE",
		EntityType: "order",
		Summary:    fmt.Sprintf("Bulk approve: %d duyệt, %d bỏ qua", len(approved), skipped),
		Metadata:   meta,
		IP:         actor.IP,
	})
}

// ---------- Cancellation (SELLER) ----------

// cancelActionError maps an unavailable action into a helpful, guiding error.
func cancelActionError(a SellerCancelAction) error {
	switch a {
	case SellerActionCancel:
		return apperr.Conflict("Đơn chưa vào sản xuất — huỷ trực tiếp thay vì gửi yêu cầu")
	case SellerActionRequest:
		return apperr.Conflict("Đơn đã vào sản xuất — cần gửi yêu cầu huỷ để vận hành duyệt")
	default:
		return apperr.Conflict("Đơn này không thể huỷ nữa")
	}
}

// stageLabel is the Vietnamese wording of a cancellation stage, used in the ops
// note and audit trail so a reader sees where the order was without decoding an
// enum.
func stageLabel(s models.CancelStage) string {
	switch s {
	case models.CancelStageInProduction:
		return "đang sản xuất"
	case models.CancelStagePacked:
		return "đã đóng gói/bàn giao"
	case models.CancelStageShipped:
		return "đã gửi đi"
	default:
		return "chưa vào sản xuất"
	}
}

// cascadeOrderCancellation propagates a settled order-level cancellation down to
// the order's remaining live lines and re-derives the status of every batch that
// lost work.
//
// Without it the order is only cosmetically cancelled: design, batching, QC and
// packing all decide what is "work" from the ITEM's cancellation status, so its
// products would keep moving through the factory after the seller was told the
// order was cancelled. Call it after the order row itself has been written, with
// order.CancellationStatus already at its terminal value.
func cascadeOrderCancellation(repo *repositories.Repositories, actor Actor, order *models.Order, at time.Time, note string) error {
	// Read the affected batches BEFORE the lines are marked cancelled — the lookup
	// joins through them.
	batchIDs, err := repo.Batch.BatchIDsForOrder(order.ID)
	if err != nil {
		return err
	}
	if _, err := repo.OrderItem.CancelActiveForOrder(order.ID, repositories.ItemCancelPatch{
		Status:       order.CancellationStatus,
		Reason:       order.CancellationReason,
		ResolvedByID: actor.IDPtr(),
		ResolvedAt:   at,
		Note:         note,
		Stage:        order.CancelStage,
		Billable:     order.CancelBillable,
	}); err != nil {
		return err
	}
	// A batch that just lost parts may now be fully printed/cut/QC'd; re-derive it
	// so the production board doesn't hold it open on work nobody will do.
	for _, id := range batchIDs {
		_ = recomputeBatchStatus(repo, id, actor)
	}
	return nil
}

// SellerCancel lets a seller cancel an order outright — allowed only while
// nothing has been produced (see sellerCancelAction). It is immediate and free.
func (s *ReviewService) SellerCancel(actor Actor, sellerID, orderID uint, reason string) (*models.Order, error) {
	order, err := s.getSellerOrder(sellerID, orderID)
	if err != nil {
		return nil, err
	}
	if sellerCancelActionForOrder(order) != SellerActionCancel {
		return nil, cancelActionError(sellerCancelActionForOrder(order))
	}
	now := time.Now()
	from := order.ReviewStatus
	order.ReviewStatus = models.ReviewCancelled
	order.CancellationStatus = models.CancellationSeller
	order.CancellationRequestedByID = actor.IDPtr()
	order.CancellationRequestedAt = &now
	order.CancellationReason = strings.TrimSpace(reason)
	order.CancellationResolvedByID = actor.IDPtr()
	order.CancellationResolvedAt = &now
	// A direct seller cancel only exists before production, so it is never billed.
	order.CancelStage = models.CancelStagePreProduction
	order.CancelBillable = false
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not cancel order").Wrap(err)
	}
	if err := cascadeOrderCancellation(s.repo, actor, order, now, "Seller huỷ cả đơn"); err != nil {
		return nil, apperr.Internal("could not cancel order items").Wrap(err)
	}
	_ = recordStatus(s.repo, models.EntityOrder, order.ID, string(from), string(models.ReviewCancelled), actor, "seller cancelled")
	s.audit.Log(actor, "ORDER_SELLER_CANCEL", "order", &order.ID, "Seller cancelled order "+order.InternalCode,
		models.JSONMap{"stage": string(order.CancelStage), "billable": false})
	return s.getOrder(order.ID)
}

// SellerRequestCancellation submits a cancellation request for an order the
// seller may no longer cancel on their own — production has started, so Ops/Admin
// decide and the order stays billable. It raises a required-attention note so the
// request lands in the ops inbox rather than waiting to be noticed.
func (s *ReviewService) SellerRequestCancellation(actor Actor, sellerID, orderID uint, reason string) (*models.Order, error) {
	order, err := s.getSellerOrder(sellerID, orderID)
	if err != nil {
		return nil, err
	}
	if order.CancellationStatus == models.CancellationRequested {
		return nil, apperr.Conflict("Đơn này đã có một yêu cầu huỷ đang chờ xử lý")
	}
	if sellerCancelActionForOrder(order) != SellerActionRequest {
		return nil, cancelActionError(sellerCancelActionForOrder(order))
	}
	now := time.Now()
	stage := orderCancelStageFor(order)
	order.CancellationStatus = models.CancellationRequested
	order.CancellationRequestedByID = actor.IDPtr()
	order.CancellationRequestedAt = &now
	order.CancellationReason = strings.TrimSpace(reason)
	// The stage is frozen here, not at approval time: the seller acted on this
	// state, and production keeps moving while the request waits in the queue.
	order.CancelStage = stage
	order.CancelBillable = stage.Billable()
	// Clear any previous resolution so a re-request starts clean.
	order.CancellationResolvedByID = nil
	order.CancellationResolvedAt = nil
	order.CancellationResolutionNote = ""
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not submit cancellation request").Wrap(err)
	}
	// Surface the request as a required-attention note in the ops inbox.
	billingLine := ""
	if order.CancelBillable {
		billingLine = " ĐƠN ĐÃ VÀO SẢN XUẤT — nếu duyệt huỷ thì vẫn tính tiền khách."
	}
	_ = s.repo.Note.Create(&models.Note{
		Title:               "Yêu cầu huỷ đơn " + order.InternalCode,
		Body:                "Seller yêu cầu huỷ đơn (" + stageLabel(stage) + "). Lý do: " + order.CancellationReason + "." + billingLine,
		ReasonCode:          "CANCEL_REQUEST",
		Severity:            models.SeverityHigh,
		Status:              models.NoteOpen,
		IsRequiredAttention: true,
		EntityType:          models.EntityOrder,
		EntityID:            &order.ID,
		OwnerRole:           models.RoleOps,
		CreatedByID:         actor.IDPtr(),
	})
	s.audit.Log(actor, "ORDER_CANCEL_REQUEST", "order", &order.ID, "Seller requested cancellation of "+order.InternalCode,
		models.JSONMap{"stage": string(stage), "billable": order.CancelBillable})
	return s.getOrder(order.ID)
}

func itemCancelled(status models.CancellationStatus) bool {
	return status == models.CancellationSeller || status == models.CancellationApproved
}

// itemCancelStage snapshots how far ONE line has progressed. It is judged on the
// line's own work, not the order's: a product still sitting at PENDING outside
// every batch costs nothing to drop even when its siblings are already on the
// machine, so the seller can still pull it themselves. Packing and shipping are
// order-level facts and apply to every line in the order.
func itemCancelStage(order *models.Order, it *models.OrderItem) models.CancelStage {
	switch order.SellerStatus {
	case models.SellerStatusShipped, models.SellerStatusDelivered:
		return models.CancelStageShipped
	case models.SellerStatusPacked, models.SellerStatusHandedOff:
		return models.CancelStagePacked
	}
	if it.InternalStatus != models.StatusPending || len(it.BatchItems) > 0 {
		return models.CancelStageInProduction
	}
	return models.CancelStagePreProduction
}

// loadSellerItem loads one line and proves it belongs to the given order and that
// no cancellation is already running on it.
func (s *ReviewService) loadSellerItem(orderID, itemID uint) (*models.OrderItem, error) {
	item, err := s.repo.OrderItem.FindByID(itemID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order item not found")
		}
		return nil, apperr.Internal("lookup item failed").Wrap(err)
	}
	if item.OrderID != orderID {
		return nil, apperr.NotFound("Order item not found in this order")
	}
	if item.CancellationStatus == models.CancellationRequested || itemCancelled(item.CancellationStatus) {
		return nil, apperr.Conflict("Sản phẩm này đã có yêu cầu huỷ hoặc đã bị huỷ")
	}
	return item, nil
}

// closeOrderIfAllItemsCancelled cancels the parent order once its last live line
// is gone, so an emptied order doesn't linger as "in production" with nothing in
// it. Inherits the billing decision from the lines that were cancelled.
func (s *ReviewService) closeOrderIfAllItemsCancelled(actor Actor, orderID uint, status models.CancellationStatus, at time.Time) error {
	refreshed, err := s.getOrder(orderID)
	if err != nil {
		return err
	}
	if refreshed.ReviewStatus == models.ReviewCancelled || len(refreshed.Items) == 0 {
		return nil
	}
	billable := false
	for i := range refreshed.Items {
		if !itemCancelled(refreshed.Items[i].CancellationStatus) {
			return nil
		}
		if refreshed.Items[i].CancelBillable {
			billable = true
		}
	}
	from := refreshed.ReviewStatus
	refreshed.ReviewStatus = models.ReviewCancelled
	refreshed.CancellationStatus = status
	refreshed.CancellationResolvedByID = actor.IDPtr()
	refreshed.CancellationResolvedAt = &at
	refreshed.CancellationResolutionNote = "Tất cả sản phẩm trong đơn đã bị huỷ"
	refreshed.CancelBillable = billable
	if refreshed.CancelStage == models.CancelStageNone {
		refreshed.CancelStage = models.CancelStagePreProduction
		if billable {
			refreshed.CancelStage = models.CancelStageInProduction
		}
	}
	if err := s.repo.Order.Update(refreshed); err != nil {
		return apperr.Internal("could not close empty order").Wrap(err)
	}
	_ = recordStatus(s.repo, models.EntityOrder, refreshed.ID, string(from), string(models.ReviewCancelled), actor, "all items cancelled")
	return nil
}

// SellerCancelItem cancels exactly one line item outright — allowed only while
// that line has not been produced. The parent order is cancelled only after its
// last active item goes.
func (s *ReviewService) SellerCancelItem(actor Actor, sellerID, orderID, itemID uint, reason string) (*models.Order, error) {
	order, err := s.getSellerOrder(sellerID, orderID)
	if err != nil {
		return nil, err
	}
	item, err := s.loadSellerItem(orderID, itemID)
	if err != nil {
		return nil, err
	}
	// Judged on the line's own progress, so an untouched product in a partly
	// produced order can still be dropped without an ops round trip.
	action := sellerCancelAction(order.ReviewStatus, order.CancellationStatus, itemCancelStage(order, item))
	if action != SellerActionCancel {
		return nil, cancelActionError(action)
	}
	now := time.Now()
	item.CancellationStatus = models.CancellationSeller
	item.CancellationRequestedByID, item.CancellationResolvedByID = actor.IDPtr(), actor.IDPtr()
	item.CancellationRequestedAt, item.CancellationResolvedAt = &now, &now
	item.CancellationReason = strings.TrimSpace(reason)
	item.CancelStage, item.CancelBillable = models.CancelStagePreProduction, false
	if err := s.repo.OrderItem.Update(item); err != nil {
		return nil, apperr.Internal("could not cancel order item").Wrap(err)
	}
	if err := s.closeOrderIfAllItemsCancelled(actor, orderID, models.CancellationSeller, now); err != nil {
		return nil, err
	}
	s.audit.Log(actor, "ORDER_ITEM_SELLER_CANCEL", "order_item", &item.ID, "Seller cancelled item "+item.InternalCode, nil)
	return s.getOrder(orderID)
}

// SellerRequestItemCancellation requests removal of one already-produced item
// without changing the parent order or its sibling items until Ops resolves it.
func (s *ReviewService) SellerRequestItemCancellation(actor Actor, sellerID, orderID, itemID uint, reason string) (*models.Order, error) {
	order, err := s.getSellerOrder(sellerID, orderID)
	if err != nil {
		return nil, err
	}
	item, err := s.loadSellerItem(orderID, itemID)
	if err != nil {
		return nil, err
	}
	stage := itemCancelStage(order, item)
	action := sellerCancelAction(order.ReviewStatus, order.CancellationStatus, stage)
	if action != SellerActionRequest {
		return nil, cancelActionError(action)
	}
	now := time.Now()
	item.CancellationStatus = models.CancellationRequested
	item.CancellationRequestedByID, item.CancellationRequestedAt = actor.IDPtr(), &now
	item.CancellationReason = strings.TrimSpace(reason)
	item.CancellationResolvedByID, item.CancellationResolvedAt, item.CancellationResolutionNote = nil, nil, ""
	item.CancelStage, item.CancelBillable = stage, stage.Billable()
	if err := s.repo.OrderItem.Update(item); err != nil {
		return nil, apperr.Internal("could not request order item cancellation").Wrap(err)
	}
	billingLine := ""
	if item.CancelBillable {
		billingLine = " SẢN PHẨM ĐÃ VÀO SẢN XUẤT — nếu duyệt huỷ thì vẫn tính tiền khách."
	}
	_ = s.repo.Note.Create(&models.Note{
		Title:               "Yêu cầu huỷ sản phẩm " + item.InternalCode,
		Body:                "Seller yêu cầu huỷ sản phẩm (" + stageLabel(stage) + "). Lý do: " + item.CancellationReason + "." + billingLine,
		ReasonCode:          "ITEM_CANCEL_REQUEST",
		Severity:            models.SeverityHigh,
		Status:              models.NoteOpen,
		IsRequiredAttention: true,
		EntityType:          models.EntityOrderItem,
		EntityID:            &item.ID,
		OwnerRole:           models.RoleOps,
		CreatedByID:         actor.IDPtr(),
	})
	s.audit.Log(actor, "ORDER_ITEM_CANCEL_REQUEST", "order_item", &item.ID, "Seller requested cancellation of "+item.InternalCode,
		models.JSONMap{"stage": string(stage), "billable": item.CancelBillable})
	return s.getOrder(orderID)
}

// ---------- Cancellation resolution (OPS / ADMIN) ----------

func (s *ReviewService) ListItemCancellationRequests(p repositories.Page) ([]models.OrderItem, int64, error) {
	return s.repo.OrderItem.ListCancellationRequests(p.Normalize())
}

// ResolveItemCancellation approves or rejects a pending per-item cancellation.
// billable overrides the charge recorded when the request was made (nil = keep
// it), so Ops can waive the cost of an in-production line case by case.
func (s *ReviewService) ResolveItemCancellation(actor Actor, itemID uint, approve bool, note string, billable *bool) (*models.OrderItem, error) {
	item, err := s.repo.OrderItem.FindByID(itemID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order item not found")
		}
		return nil, apperr.Internal("lookup item failed").Wrap(err)
	}
	if item.CancellationStatus != models.CancellationRequested {
		return nil, apperr.Conflict("Sản phẩm này không có yêu cầu huỷ nào đang chờ")
	}
	if err := s.guardResolveStage(actor, item.CancelStage); err != nil {
		return nil, err
	}
	now := time.Now()
	if approve {
		item.CancellationStatus = models.CancellationApproved
	} else {
		item.CancellationStatus = models.CancellationRejected
		// A refused request leaves nothing to invoice on top of the normal order.
		item.CancelBillable = false
	}
	if approve && billable != nil {
		item.CancelBillable = *billable
	}
	item.CancellationResolvedByID, item.CancellationResolvedAt = actor.IDPtr(), &now
	item.CancellationResolutionNote = strings.TrimSpace(note)
	if err := s.repo.OrderItem.Update(item); err != nil {
		return nil, apperr.Internal("could not resolve order item cancellation").Wrap(err)
	}
	resolution := "Đã từ chối yêu cầu huỷ sản phẩm"
	if approve {
		resolution = "Đã duyệt huỷ sản phẩm"
	}
	s.closeCancelRequestNote(actor, models.EntityOrderItem, item.ID, "ITEM_CANCEL_REQUEST", resolution, now)
	if approve {
		// The line is out of production now: re-derive the batches it was in so they
		// aren't held open waiting on a part nobody will make.
		if batchIDs, bErr := s.repo.Batch.BatchIDsForOrderItem(item.ID); bErr == nil {
			for _, id := range batchIDs {
				_ = recomputeBatchStatus(s.repo, id, actor)
			}
		}
		if err := s.closeOrderIfAllItemsCancelled(actor, item.OrderID, models.CancellationApproved, now); err != nil {
			return nil, err
		}
	}
	action := "CANCEL_ITEM_REJECT"
	if approve {
		action = "CANCEL_ITEM_APPROVE"
	}
	s.audit.Log(actor, action, "order_item", &item.ID, "Resolved cancellation of "+item.InternalCode,
		models.JSONMap{"stage": string(item.CancelStage), "billable": item.CancelBillable})
	return s.repo.OrderItem.FindByID(item.ID)
}

// ListCancellationRequests lists orders with a pending cancellation request.
func (s *ReviewService) ListCancellationRequests(f repositories.OrderFilter) ([]models.Order, int64, error) {
	f.Page = f.Page.Normalize()
	f.CancellationStatus = string(models.CancellationRequested)
	rows, total, err := s.repo.Order.List(f)
	if err != nil {
		return rows, total, err
	}
	if err := annotateStoreOrderDupSlice(s.repo, rows); err != nil {
		return rows, total, err
	}
	return rows, total, nil
}

// closeCancelRequestNote clears the required-attention note the request raised.
// The inbox is only useful if settled items leave it, and a cancellation the ops
// team has already decided on is settled either way — approved or refused.
func (s *ReviewService) closeCancelRequestNote(actor Actor, entity models.EntityType, entityID uint, reasonCode, resolution string, at time.Time) {
	_, _ = s.repo.Note.ResolveOpenForEntityReason(entity, entityID, reasonCode, actor.IDPtr(), resolution, at)
}

// guardResolveStage restricts who may sign off a cancellation. Ops handle the
// everyday case (an order still on the shop floor); once the goods are packed or
// out the door, writing the order off is a commercial decision, so it mirrors the
// manual-cancel rule and asks for ADMIN/OWNER.
func (s *ReviewService) guardResolveStage(actor Actor, stage models.CancelStage) error {
	if stage != models.CancelStagePacked && stage != models.CancelStageShipped {
		return nil
	}
	if actor.Role == models.RoleOwner || actor.Role == models.RoleAdmin {
		return nil
	}
	return apperr.Forbidden("Đơn đã đóng gói/gửi đi — chỉ Admin/Owner được duyệt huỷ")
}

// ListResolvedCancellations lists cancellations that are already settled — the
// history behind the pending queue.
//
// It exists because a settled cancellation otherwise has nowhere to be seen: it
// leaves the pending queue, and cancelling an order cancels every line in it, so
// it drops out of the item-level order list too. That matters most for the ones
// that are still charged (cancel_billable), which are exactly the rows somebody
// has to invoice. billable narrows to those; nil lists everything settled.
func (s *ReviewService) ListResolvedCancellations(f repositories.OrderFilter, billable *bool) ([]models.Order, int64, error) {
	f.Page = f.Page.Normalize()
	f.CancellationStatuses = []string{
		string(models.CancellationApproved),
		string(models.CancellationSeller),
		string(models.CancellationRejected),
	}
	f.CancelBillable = billable
	rows, total, err := s.repo.Order.List(f)
	if err != nil {
		return rows, total, err
	}
	if err := annotateStoreOrderDupSlice(s.repo, rows); err != nil {
		return rows, total, err
	}
	return rows, total, nil
}

// ApproveCancellation approves a pending cancellation request and cancels the
// order, pulling it and every one of its lines out of the production flow.
// billable overrides the charge decided when the request was made (nil = keep
// it): the default is "đã sản xuất thì vẫn tính tiền", and Ops/Admin may waive it.
func (s *ReviewService) ApproveCancellation(actor Actor, orderID uint, note string, billable *bool) (*models.Order, error) {
	order, err := s.getOrder(orderID)
	if err != nil {
		return nil, err
	}
	if order.CancellationStatus != models.CancellationRequested {
		return nil, apperr.Conflict("Đơn này không có yêu cầu huỷ nào đang chờ")
	}
	if err := s.guardResolveStage(actor, order.CancelStage); err != nil {
		return nil, err
	}
	now := time.Now()
	from := order.ReviewStatus
	order.CancellationStatus = models.CancellationApproved
	order.CancellationResolvedByID = actor.IDPtr()
	order.CancellationResolvedAt = &now
	order.CancellationResolutionNote = strings.TrimSpace(note)
	order.ReviewStatus = models.ReviewCancelled
	if billable != nil {
		order.CancelBillable = *billable
	}
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not approve cancellation").Wrap(err)
	}
	if err := cascadeOrderCancellation(s.repo, actor, order, now, "Huỷ đơn được duyệt"); err != nil {
		return nil, apperr.Internal("could not cancel order items").Wrap(err)
	}
	s.closeCancelRequestNote(actor, models.EntityOrder, order.ID, "CANCEL_REQUEST", "Đã duyệt huỷ đơn", now)
	_ = recordStatus(s.repo, models.EntityOrder, order.ID, string(from), string(models.ReviewCancelled), actor, "cancellation approved")
	s.audit.Log(actor, "CANCEL_APPROVE", "order", &order.ID, "Approved cancellation of "+order.InternalCode,
		models.JSONMap{"stage": string(order.CancelStage), "billable": order.CancelBillable})
	return s.getOrder(order.ID)
}

// RejectCancellation denies a pending cancellation request; the order continues
// on its normal flow and is billed as an ordinary order.
func (s *ReviewService) RejectCancellation(actor Actor, orderID uint, note string) (*models.Order, error) {
	order, err := s.getOrder(orderID)
	if err != nil {
		return nil, err
	}
	if order.CancellationStatus != models.CancellationRequested {
		return nil, apperr.Conflict("Đơn này không có yêu cầu huỷ nào đang chờ")
	}
	now := time.Now()
	order.CancellationStatus = models.CancellationRejected
	order.CancellationResolvedByID = actor.IDPtr()
	order.CancellationResolvedAt = &now
	order.CancellationResolutionNote = strings.TrimSpace(note)
	// Nothing was cancelled, so there is no cancellation charge to carry.
	order.CancelBillable = false
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not reject cancellation").Wrap(err)
	}
	s.closeCancelRequestNote(actor, models.EntityOrder, order.ID, "CANCEL_REQUEST", "Đã từ chối yêu cầu huỷ", now)
	s.audit.Log(actor, "CANCEL_REJECT", "order", &order.ID, "Rejected cancellation of "+order.InternalCode,
		models.JSONMap{"stage": string(order.CancelStage)})
	return s.getOrder(order.ID)
}
