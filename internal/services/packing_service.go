package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/shipping"
)

// PackingService handles the packing station (scan QC-passed items into a
// package) and the THE handoff. No real carrier API is called in the MVP.
type PackingService struct {
	repo    *repositories.Repositories
	audit   *AuditService
	carrier shipping.Carrier
	// tracking registers a dispatched parcel with the 24hTrack provider the moment
	// the shipping desk records its tracking number.
	tracking *TrackingSyncService
}

// getOrCreateOpenPackage returns the order's open package, creating one (with an
// expected line per order item) on first use. A partial unique index
// (uniq_packages_open_order) guarantees at most one OPEN package per order; if
// two stations race on the first scan, the loser's INSERT fails and it retries
// the lookup, landing on the winner's package instead of creating a duplicate.
func (s *PackingService) getOrCreateOpenPackage(tx *gorm.DB, order *models.Order, actor Actor) (*models.Package, error) {
	txRepo := repositories.New(tx)
	if pkg, err := txRepo.Package.FindOpenByOrder(order.ID); err == nil {
		return pkg, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	pkg := &models.Package{OrderID: order.ID, Status: models.PackageOpen}
	if err := txRepo.Package.Create(pkg); err != nil {
		// Unique-index collision: another station created the open package first.
		if existing, ferr := txRepo.Package.FindOpenByOrder(order.ID); ferr == nil {
			return existing, nil
		}
		return nil, err
	}
	pkg.Code = fmt.Sprintf("PKG-%06d", pkg.ID)
	if err := txRepo.Package.Update(pkg); err != nil {
		return nil, err
	}

	var items []models.PackageItem
	for _, it := range order.Items {
		if itemCancelled(it.CancellationStatus) {
			continue
		}
		items = append(items, models.PackageItem{
			PackageID: pkg.ID, OrderItemID: it.ID, ExpectedQty: it.Quantity, ScannedQty: 0,
		})
	}
	if err := txRepo.Package.CreateItems(items); err != nil {
		return nil, err
	}
	return txRepo.Package.FindByID(pkg.ID)
}

// PackingScanInput scans one physical unit of an item into its order's package.
type PackingScanInput struct {
	OrderID *uint  `json:"order_id"`
	Code    string `json:"code"`
	ItemID  *uint  `json:"item_id"`
}

// PackingResult is the expected-vs-scanned summary returned after each scan.
type PackingResult struct {
	PackageID   uint          `json:"package_id"`
	PackageCode string        `json:"package_code"`
	OrderID     uint          `json:"order_id"`
	OrderCode   string        `json:"order_code"`
	FullyPacked bool          `json:"fully_packed"`
	Lines       []PackingLine `json:"lines"`
}

// PackingLine is one item's expected vs scanned count.
type PackingLine struct {
	OrderItemID uint   `json:"order_item_id"`
	ItemCode    string `json:"item_code"`
	SKUCode     string `json:"sku_code"`
	Expected    int    `json:"expected"`
	Scanned     int    `json:"scanned"`
}

func (s *PackingService) resolveItem(orderID *uint, code string, itemID *uint) (*models.OrderItem, error) {
	if itemID != nil {
		it, err := s.repo.OrderItem.FindByID(*itemID)
		if err != nil {
			return nil, apperr.NotFound("Item not found")
		}
		return it, nil
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, apperr.BadRequest("Provide an item code or item_id")
	}
	it, err := s.repo.OrderItem.FindByCode(code)
	if err != nil {
		return nil, apperr.NotFound("No item matches that scan code")
	}
	if orderID != nil && it.OrderID != *orderID {
		return nil, apperr.BadRequest("Scanned item does not belong to the given order")
	}
	return it, nil
}

// Scan validates the item is QC-passed and increments its scanned count, blocking
// over-scans. When all lines are complete the order moves to seller status PACKED.
func (s *PackingService) Scan(actor Actor, in PackingScanInput) (*PackingResult, error) {
	item, err := s.resolveItem(in.OrderID, in.Code, in.ItemID)
	if err != nil {
		return nil, err
	}
	order, err := s.repo.Order.FindByID(item.OrderID)
	if err != nil {
		return nil, apperr.Internal("could not load order").Wrap(err)
	}
	if order.ReviewStatus != models.ReviewApproved {
		return nil, apperr.Unprocessable("Order is not approved for production; cannot pack")
	}

	var result *PackingResult
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)
		// Cửa "đã QC chưa / đã huỷ chưa" phải đọc TRONG transaction, sau khi khoá
		// dòng sản phẩm. Đọc ngoài rồi mới mở transaction thì một lần huỷ batch
		// hoặc hạ QC chạy song song vẫn kịp lọt: lần quét này mở kiện cho CẢ đơn
		// và từ đó không luồng nào kéo hàng về "chờ làm lại" được nữa, nên nó phải
		// nhìn thấy đúng trạng thái tại thời điểm ghi.
		if err := txRepo.OrderItem.LockForUpdate([]uint{item.ID}); err != nil {
			return apperr.Internal("Không khoá được sản phẩm để đóng gói").Wrap(err)
		}
		status, cancellation, err := txRepo.OrderItem.GateStateByID(item.ID)
		if err != nil {
			return apperr.Internal("could not re-read item").Wrap(err)
		}
		if status != models.StatusQCPassed {
			return apperr.Unprocessable("Item is not QC-passed yet; cannot pack (BLOCK)")
		}
		if itemCancelled(cancellation) {
			return apperr.Conflict("Sản phẩm đã huỷ, không thể đóng gói")
		}
		pkg, err := s.getOrCreateOpenPackage(tx, order, actor)
		if err != nil {
			return err
		}
		var line *models.PackageItem
		for i := range pkg.Items {
			if pkg.Items[i].OrderItemID == item.ID {
				line = &pkg.Items[i]
				break
			}
		}
		if line == nil {
			return apperr.BadRequest("Item is not part of this package")
		}
		// Atomic, guarded increment: two stations scanning the same line at once
		// can never double-count a slot or push scanned past expected — the losing
		// UPDATE simply matches zero rows and is rejected as an over-scan.
		bumped, err := txRepo.Package.IncrementScanned(line.ID)
		if err != nil {
			return err
		}
		if !bumped {
			return apperr.Conflict("Item already fully scanned (over-scan blocked)")
		}

		// Recompute fully-packed from the fresh counts.
		full := true
		fresh, err := txRepo.Package.FindByID(pkg.ID)
		if err != nil {
			return err
		}
		for _, l := range fresh.Items {
			if l.ScannedQty < l.ExpectedQty {
				full = false
				break
			}
		}
		if full && order.SellerStatus == models.SellerStatusProduction {
			// Guarded transition (WHERE seller_status = PRODUCTION): concurrent
			// completions record the PACKED move exactly once, and the update can't
			// overwrite a status another flow advanced in the meantime.
			changed, err := txRepo.Order.UpdateSellerStatusIf(order.ID, models.SellerStatusProduction, models.SellerStatusPacked)
			if err != nil {
				return err
			}
			if changed {
				order.SellerStatus = models.SellerStatusPacked
				_ = recordStatus(txRepo, models.EntityOrder, order.ID,
					string(models.SellerStatusProduction), string(models.SellerStatusPacked), actor, "all items packed")
			}
		}

		result = buildPackingResult(fresh, order, full)
		return nil
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("could not record packing scan").Wrap(err)
	}
	s.audit.Log(actor, "PACKING_SCAN", "order_item", &item.ID, "Packed item "+item.InternalCode, nil)
	return result, nil
}

func buildPackingResult(pkg *models.Package, order *models.Order, full bool) *PackingResult {
	res := &PackingResult{
		PackageID: pkg.ID, PackageCode: pkg.Code, OrderID: order.ID,
		OrderCode: order.InternalCode, FullyPacked: full,
	}
	for _, l := range pkg.Items {
		line := PackingLine{OrderItemID: l.OrderItemID, Expected: l.ExpectedQty, Scanned: l.ScannedQty}
		if l.OrderItem != nil {
			line.ItemCode = l.OrderItem.InternalCode
			line.SKUCode = l.OrderItem.SKUCode
		}
		res.Lines = append(res.Lines, line)
	}
	return res
}

// GetPackageForOrder returns the order's current package (open or packed).
func (s *PackingService) GetPackageForOrder(orderID uint) (*models.Package, error) {
	pkg, err := s.repo.Package.FindOpenByOrder(orderID)
	if err == nil {
		return pkg, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	// fall back to any package for the order
	pkgs, _, lerr := s.repo.Package.List(repositories.Page{Page: 1, PageSize: 1}, &orderID)
	if lerr != nil || len(pkgs) == 0 {
		return nil, apperr.NotFound("No package for this order yet")
	}
	return s.repo.Package.FindByID(pkgs[0].ID)
}

// ---------- Handoff ----------

// HandoffInput creates a THE handoff for a fully-packed order/package.
type HandoffInput struct {
	OrderID     *uint   `json:"order_id"`
	PackageID   *uint   `json:"package_id"`
	BoxType     string  `json:"box_type"`
	WeightGrams int     `json:"weight_grams"`
	LengthCm    float64 `json:"length_cm"`
	WidthCm     float64 `json:"width_cm"`
	HeightCm    float64 `json:"height_cm"`
	PackingNote string  `json:"packing_note"`
	PhotoURL    string  `json:"photo_url"`
	Note        string  `json:"note"`
}

func (s *PackingService) resolvePackage(in HandoffInput) (*models.Package, error) {
	if in.PackageID != nil {
		pkg, err := s.repo.Package.FindByID(*in.PackageID)
		if err != nil {
			return nil, apperr.NotFound("Package not found")
		}
		return pkg, nil
	}
	if in.OrderID != nil {
		return s.GetPackageForOrder(*in.OrderID)
	}
	return nil, apperr.BadRequest("Provide order_id or package_id")
}

// CreateHandoff validates the package is complete, records the bàn giao to THE
// and advances the order to seller status HANDED_OFF. The shipping adapter is
// consulted for a label/tracking number; in the MVP it reports unsupported and
// the handoff is recorded as a manual handover.
func (s *PackingService) CreateHandoff(actor Actor, in HandoffInput) (*models.Handoff, error) {
	pkg, err := s.resolvePackage(in)
	if err != nil {
		return nil, err
	}
	for _, l := range pkg.Items {
		if l.ScannedQty < l.ExpectedQty {
			return nil, apperr.Unprocessable("Package is not fully packed; handoff blocked (thiếu item)")
		}
	}
	order, err := s.repo.Order.FindByID(pkg.OrderID)
	if err != nil {
		return nil, apperr.Internal("could not load order").Wrap(err)
	}
	if order.ReviewStatus != models.ReviewApproved {
		return nil, apperr.Unprocessable("Order is not approved for production; cannot hand off")
	}

	// Consult the carrier adapter (no-op in the MVP).
	label, _ := s.carrier.CreateLabel(context.Background(), shipping.LabelRequest{
		OrderCode: order.InternalCode, RecipientName: order.ShippingName,
		Address1: order.ShippingAddress1, Address2: order.ShippingAddress2, City: order.ShippingCity,
		Province: order.ShippingProvince, Zip: order.ShippingZip, Country: order.ShippingCountry,
		Phone: order.ShippingPhone, WeightGrams: in.WeightGrams,
	})

	var handoff *models.Handoff
	now := time.Now()
	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		// Update package config + mark packed.
		pkg.BoxType = in.BoxType
		pkg.WeightGrams = in.WeightGrams
		pkg.LengthCm = in.LengthCm
		pkg.WidthCm = in.WidthCm
		pkg.HeightCm = in.HeightCm
		pkg.PackingNote = in.PackingNote
		pkg.PhotoURL = in.PhotoURL
		pkg.Status = models.PackagePacked
		pkg.PackedByID = actor.IDPtr()
		pkg.PackedAt = &now
		if err := txRepo.Package.Update(pkg); err != nil {
			return err
		}

		handoff = &models.Handoff{
			OrderID: &order.ID, PackageID: &pkg.ID, Carrier: s.carrier.Name(),
			Status: models.HandoffHandedOff, Note: in.Note, HandedOffByID: actor.IDPtr(), HandedOffAt: now,
		}
		if label.Supported {
			handoff.TrackingNumber = label.TrackingNumber
			handoff.LabelURL = label.LabelURL
		}
		if err := txRepo.Handoff.Create(handoff); err != nil {
			return err
		}
		handoff.Code = fmt.Sprintf("THE-HO-%06d", handoff.ID)
		if err := txRepo.Handoff.Update(handoff); err != nil {
			return err
		}

		old := string(order.SellerStatus)
		order.SellerStatus = models.SellerStatusHandedOff
		if order.HandedOverAt == nil {
			order.HandedOverAt = &now
		}
		if err := txRepo.Order.Update(order); err != nil {
			return err
		}
		_ = recordStatus(txRepo, models.EntityOrder, order.ID, old, string(models.SellerStatusHandedOff), actor, "handed off to "+s.carrier.Name())
		return nil
	})
	if err != nil {
		return nil, apperr.Internal("could not create handoff").Wrap(err)
	}
	s.audit.Log(actor, "HANDOFF_CREATE", "handoff", &handoff.ID,
		fmt.Sprintf("Handoff %s for order %s to %s", handoff.Code, order.InternalCode, s.carrier.Name()), nil)

	// Handing over is the moment the shipping half of the order's life begins:
	// from here the parcel is the carrier's, so this is when tracking starts.
	// Orders that already carry a tracking number (CS attached it while the order
	// was still in production) are registered with the provider right now; the
	// rest are picked up by the periodic pass, which only looks at handed-over
	// orders. Fire-and-forget — the packing station must not wait on a third party.
	// (order.SellerStatus is already HANDED_OFF: the transaction above set it on
	// this same struct, which is what makes the guard inside RegisterOrder pass.)
	s.tracking.RegisterOrderAsync(order)
	return handoff, nil
}

// ListHandoffs returns handoffs.
func (s *PackingService) ListHandoffs(page repositories.Page) ([]models.Handoff, int64, error) {
	return s.repo.Handoff.List(page.Normalize())
}

// MarkShippedInput records the tracking number for a dispatched handoff.
// Tracking is required — it is the whole point of the dispatch step.
//
// There is no carrier field: the handoff already carries our own name, and the
// transport partner behind it is not something the shipping desk types in.
type MarkShippedInput struct {
	TrackingNumber string `json:"tracking_number"`
	LabelURL       string `json:"label_url"`
}

// MarkShipped completes the final leg: a handed-off parcel becomes SHIPPED,
// carrying the tracking number, and its order advances to seller
// status SHIPPED. This is the counterpart of CreateHandoff — where CreateHandoff
// stops at HANDED_OFF (the MVP had no dispatch step), MarkShipped closes the
// order lifecycle so sellers see "Đã gửi đi" and can follow the tracking.
func (s *PackingService) MarkShipped(actor Actor, handoffID uint, in MarkShippedInput) (*models.Handoff, error) {
	in.TrackingNumber = strings.TrimSpace(in.TrackingNumber)
	in.LabelURL = strings.TrimSpace(in.LabelURL)
	if in.TrackingNumber == "" {
		return nil, apperr.BadRequest("Mã vận đơn (tracking_number) là bắt buộc")
	}

	handoff, err := s.repo.Handoff.FindByID(handoffID)
	if err != nil {
		return nil, apperr.NotFound("Handoff not found")
	}
	if handoff.Status == models.HandoffShipped {
		return nil, apperr.Conflict("Handoff đã ở trạng thái đã gửi")
	}
	if handoff.Status != models.HandoffHandedOff {
		return nil, apperr.Unprocessable("Chỉ đánh dấu gửi được cho handoff đã bàn giao")
	}

	// Captured inside the transaction, used after it commits: the provider must
	// only ever be told about a shipment that actually persisted.
	var shippedOrder *models.Order

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		handoff.TrackingNumber = in.TrackingNumber
		if in.LabelURL != "" {
			handoff.LabelURL = in.LabelURL
		}
		handoff.Status = models.HandoffShipped
		if err := txRepo.Handoff.Update(handoff); err != nil {
			return err
		}
		_ = recordStatus(txRepo, models.EntityHandoff, handoff.ID,
			string(models.HandoffHandedOff), string(models.HandoffShipped), actor, "marked shipped")

		// Advance the order to seller status SHIPPED (the visible end state).
		if handoff.OrderID != nil {
			order, oerr := txRepo.Order.FindByID(*handoff.OrderID)
			if oerr == nil {
				if order.SellerStatus != models.SellerStatusShipped {
					old := string(order.SellerStatus)
					order.SellerStatus = models.SellerStatusShipped
					// A dispatch implies the parcel left the factory; only legacy
					// orders that predate HandedOverAt still miss the stamp here.
					if order.HandedOverAt == nil {
						handedAt := time.Now()
						order.HandedOverAt = &handedAt
					}
					if err := txRepo.Order.Update(order); err != nil {
						return err
					}
					_ = recordStatus(txRepo, models.EntityOrder, order.ID, old, string(models.SellerStatusShipped), actor, "shipped")
				}
				// Mirror the dispatch's tracking number onto the order. The handoff is
				// where it is captured, but the order is what every screen (and the
				// provider sync) reads — without this the parcel would ship without the
				// order ever knowing its own tracking number.
				if order.TrackingNumber != handoff.TrackingNumber {
					fields := map[string]interface{}{
						"tracking_number":     handoff.TrackingNumber,
						"tracking_updated_at": time.Now(),
					}
					// PENDING, not IN_TRANSIT: the parcel has been handed over but no
					// scan has confirmed it yet. The provider sync overwrites this with
					// the real shipment state on its first pass.
					if order.TrackingStatus == models.TrackingNone || order.TrackingStatus == "" {
						fields["tracking_status"] = models.TrackingPending
					}
					if err := txRepo.Order.UpdateTracking(order.ID, fields); err != nil {
						return err
					}
					order.TrackingNumber = handoff.TrackingNumber
				}
				shippedOrder = order
			}
		}
		return nil
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("could not mark handoff shipped").Wrap(err)
	}

	s.audit.Log(actor, "HANDOFF_SHIP", "handoff", &handoff.ID,
		fmt.Sprintf("Handoff %s shipped (%s)", handoff.Code, handoff.TrackingNumber), nil)

	// Hand the parcel to the tracking provider so its journey starts being
	// collected. Fire-and-forget: the shipping desk must not wait on a third
	// party, and the periodic sync would pick the parcel up anyway.
	s.tracking.RegisterOrderAsync(shippedOrder)
	return handoff, nil
}
