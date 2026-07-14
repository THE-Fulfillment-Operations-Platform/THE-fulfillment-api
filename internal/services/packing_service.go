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
}

// getOrCreateOpenPackage returns the order's open package, creating one (with an
// expected line per order item) on first use.
func (s *PackingService) getOrCreateOpenPackage(tx *gorm.DB, order *models.Order, actor Actor) (*models.Package, error) {
	txRepo := repositories.New(tx)
	if pkg, err := txRepo.Package.FindOpenByOrder(order.ID); err == nil {
		return pkg, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	pkg := &models.Package{OrderID: order.ID, Status: models.PackageOpen}
	if err := txRepo.Package.Create(pkg); err != nil {
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
	if item.InternalStatus != models.StatusQCPassed {
		return nil, apperr.Unprocessable("Item is not QC-passed yet; cannot pack (BLOCK)")
	}
	if itemCancelled(item.CancellationStatus) {
		return nil, apperr.Conflict("Sản phẩm đã huỷ, không thể đóng gói")
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
		if line.ScannedQty >= line.ExpectedQty {
			return apperr.Conflict("Item already fully scanned (over-scan blocked)")
		}
		line.ScannedQty++
		if err := txRepo.Package.UpdateItem(line); err != nil {
			return err
		}

		// Recompute fully-packed.
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
			old := string(order.SellerStatus)
			order.SellerStatus = models.SellerStatusPacked
			if err := txRepo.Order.Update(order); err != nil {
				return err
			}
			_ = recordStatus(txRepo, models.EntityOrder, order.ID, old, string(models.SellerStatusPacked), actor, "all items packed")
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
		Phone: order.ShippingPhone, WeightGrams: in.WeightGrams, ShippingMethod: order.ShippingMethod,
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
	return handoff, nil
}

// ListHandoffs returns handoffs.
func (s *PackingService) ListHandoffs(page repositories.Page) ([]models.Handoff, int64, error) {
	return s.repo.Handoff.List(page.Normalize())
}

// MarkShippedInput records the carrier + tracking number for a dispatched
// handoff. carrier is optional (falls back to the existing value); tracking is
// required — it is the whole point of the dispatch step.
type MarkShippedInput struct {
	Carrier        string `json:"carrier"`
	TrackingNumber string `json:"tracking_number"`
	LabelURL       string `json:"label_url"`
}

// MarkShipped completes the final leg: a handed-off parcel becomes SHIPPED,
// carrying the carrier + tracking number, and its order advances to seller
// status SHIPPED. This is the counterpart of CreateHandoff — where CreateHandoff
// stops at HANDED_OFF (the MVP had no dispatch step), MarkShipped closes the
// order lifecycle so sellers see "Đã gửi đi" and can follow the tracking.
func (s *PackingService) MarkShipped(actor Actor, handoffID uint, in MarkShippedInput) (*models.Handoff, error) {
	in.Carrier = strings.TrimSpace(in.Carrier)
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

	err = s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		if in.Carrier != "" {
			handoff.Carrier = in.Carrier
		}
		handoff.TrackingNumber = in.TrackingNumber
		if in.LabelURL != "" {
			handoff.LabelURL = in.LabelURL
		}
		handoff.Status = models.HandoffShipped
		if err := txRepo.Handoff.Update(handoff); err != nil {
			return err
		}
		_ = recordStatus(txRepo, models.EntityHandoff, handoff.ID,
			string(models.HandoffHandedOff), string(models.HandoffShipped), actor, "marked shipped via "+handoff.Carrier)

		// Advance the order to seller status SHIPPED (the visible end state).
		if handoff.OrderID != nil {
			order, oerr := txRepo.Order.FindByID(*handoff.OrderID)
			if oerr == nil && order.SellerStatus != models.SellerStatusShipped {
				old := string(order.SellerStatus)
				order.SellerStatus = models.SellerStatusShipped
				if err := txRepo.Order.Update(order); err != nil {
					return err
				}
				_ = recordStatus(txRepo, models.EntityOrder, order.ID, old, string(models.SellerStatusShipped), actor, "shipped")
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
		fmt.Sprintf("Handoff %s shipped via %s (%s)", handoff.Code, handoff.Carrier, handoff.TrackingNumber), nil)
	return handoff, nil
}
