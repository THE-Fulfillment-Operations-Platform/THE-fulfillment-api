package services

import (
	"errors"
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
	// SellerActionCancel: order is still in review — seller may cancel directly.
	SellerActionCancel SellerCancelAction = "CANCEL"
	// SellerActionRequest: order is approved and waiting for design (not yet in
	// production) — seller may submit a cancellation request for ops to resolve.
	SellerActionRequest SellerCancelAction = "REQUEST"
	// SellerActionOpsOnly: order is already in production (printed/cut/QC or
	// batched) — only OPS/ADMIN can cancel it manually; the seller cannot.
	SellerActionOpsOnly SellerCancelAction = "OPS_ONLY"
	// SellerActionRefundClaim: order is packed/handed off/shipped — direct
	// cancellation is not allowed; it is handled later as a refund/claim.
	SellerActionRefundClaim SellerCancelAction = "REFUND_CLAIM"
	// SellerActionNone: no action available (already cancelled/rejected, or a
	// cancellation request is already pending).
	SellerActionNone SellerCancelAction = "NONE"
)

// orderPacked reports whether an order has advanced to packing or beyond.
func orderPacked(seller models.SellerStatus) bool {
	switch seller {
	case models.SellerStatusPacked, models.SellerStatusHandedOff, models.SellerStatusShipped:
		return true
	}
	return false
}

// orderInProduction reports whether an approved order already has production
// work in flight: any item scheduled into a batch or advanced past PENDING.
// Requires the order's Items (and, for precision, their BatchItems) preloaded.
func orderInProduction(o *models.Order) bool {
	for _, it := range o.Items {
		if it.InternalStatus != models.StatusPending {
			return true
		}
		if len(it.BatchItems) > 0 {
			return true
		}
	}
	return false
}

// sellerCancelAction computes the cancellation action available to the seller
// from the order's review/cancellation state and how far it has progressed.
func sellerCancelAction(review models.ReviewStatus, cancellation models.CancellationStatus, packed, inProduction bool) SellerCancelAction {
	// A request already in flight leaves the seller nothing to do but wait.
	if cancellation == models.CancellationRequested {
		return SellerActionNone
	}
	switch review {
	case models.ReviewPending, models.ReviewNeedsFix:
		return SellerActionCancel
	case models.ReviewApproved:
		if packed {
			return SellerActionRefundClaim
		}
		if inProduction {
			return SellerActionOpsOnly
		}
		return SellerActionRequest
	default: // REJECTED, CANCELLED
		return SellerActionNone
	}
}

// sellerCancelActionForOrder is the order-level convenience wrapper.
func sellerCancelActionForOrder(o *models.Order) SellerCancelAction {
	return sellerCancelAction(o.ReviewStatus, o.CancellationStatus, orderPacked(o.SellerStatus), orderInProduction(o))
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

	// One query for every item's mapped-material count (instead of a COUNT per
	// item) — the loop below is then database-free.
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

// ---------- Cancellation (SELLER) ----------

// cancelActionError maps an unavailable action into a helpful, guiding error.
func cancelActionError(a SellerCancelAction) error {
	switch a {
	case SellerActionCancel:
		return apperr.Conflict("This order is still in review; cancel it directly instead")
	case SellerActionRequest:
		return apperr.Conflict("This order is approved; submit a cancellation request instead")
	case SellerActionOpsOnly:
		return apperr.Conflict("This order is already in production; please contact Ops to cancel")
	case SellerActionRefundClaim:
		return apperr.Conflict("This order is already packed/shipped; cancellation is not allowed (handle as refund/claim)")
	default:
		return apperr.Conflict("This order can no longer be cancelled")
	}
}

// SellerCancel lets a seller directly cancel an order that is still in review.
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
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not cancel order").Wrap(err)
	}
	_ = recordStatus(s.repo, models.EntityOrder, order.ID, string(from), string(models.ReviewCancelled), actor, "seller cancelled")
	s.audit.Log(actor, "ORDER_SELLER_CANCEL", "order", &order.ID, "Seller cancelled order "+order.InternalCode, nil)
	return s.getOrder(order.ID)
}

// SellerRequestCancellation submits a cancellation request for an approved order
// that is not yet in production. It raises a required-attention note for ops.
func (s *ReviewService) SellerRequestCancellation(actor Actor, sellerID, orderID uint, reason string) (*models.Order, error) {
	order, err := s.getSellerOrder(sellerID, orderID)
	if err != nil {
		return nil, err
	}
	if order.CancellationStatus == models.CancellationRequested {
		return nil, apperr.Conflict("A cancellation request is already pending for this order")
	}
	if sellerCancelActionForOrder(order) != SellerActionRequest {
		return nil, cancelActionError(sellerCancelActionForOrder(order))
	}
	now := time.Now()
	order.CancellationStatus = models.CancellationRequested
	order.CancellationRequestedByID = actor.IDPtr()
	order.CancellationRequestedAt = &now
	order.CancellationReason = strings.TrimSpace(reason)
	// Clear any previous resolution so a re-request starts clean.
	order.CancellationResolvedByID = nil
	order.CancellationResolvedAt = nil
	order.CancellationResolutionNote = ""
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not submit cancellation request").Wrap(err)
	}
	// Surface the request as a required-attention note in the ops inbox.
	_ = s.repo.Note.Create(&models.Note{
		Title:               "Yêu cầu huỷ đơn " + order.InternalCode,
		Body:                "Seller yêu cầu huỷ đơn. Lý do: " + order.CancellationReason,
		ReasonCode:          "CANCEL_REQUEST",
		Severity:            models.SeverityHigh,
		Status:              models.NoteOpen,
		IsRequiredAttention: true,
		EntityType:          models.EntityOrder,
		EntityID:            &order.ID,
		OwnerRole:           models.RoleOps,
		CreatedByID:         actor.IDPtr(),
	})
	s.audit.Log(actor, "ORDER_CANCEL_REQUEST", "order", &order.ID, "Seller requested cancellation of "+order.InternalCode, nil)
	return s.getOrder(order.ID)
}

func itemCancelled(status models.CancellationStatus) bool {
	return status == models.CancellationSeller || status == models.CancellationApproved
}

// SellerCancelItem cancels exactly one line item while its parent order is still
// in review. The parent order is cancelled only after its last active item goes.
func (s *ReviewService) SellerCancelItem(actor Actor, sellerID, orderID, itemID uint, reason string) (*models.Order, error) {
	order, err := s.getSellerOrder(sellerID, orderID)
	if err != nil {
		return nil, err
	}
	if sellerCancelActionForOrder(order) != SellerActionCancel {
		return nil, cancelActionError(sellerCancelActionForOrder(order))
	}
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
		return nil, apperr.Conflict("This order item already has a cancellation")
	}
	now := time.Now()
	item.CancellationStatus = models.CancellationSeller
	item.CancellationRequestedByID, item.CancellationResolvedByID = actor.IDPtr(), actor.IDPtr()
	item.CancellationRequestedAt, item.CancellationResolvedAt = &now, &now
	item.CancellationReason = strings.TrimSpace(reason)
	if err := s.repo.OrderItem.Update(item); err != nil {
		return nil, apperr.Internal("could not cancel order item").Wrap(err)
	}

	refreshed, err := s.getOrder(orderID)
	if err != nil {
		return nil, err
	}
	allCancelled := len(refreshed.Items) > 0
	for i := range refreshed.Items {
		if !itemCancelled(refreshed.Items[i].CancellationStatus) {
			allCancelled = false
			break
		}
	}
	if allCancelled {
		refreshed.ReviewStatus = models.ReviewCancelled
		refreshed.CancellationStatus = models.CancellationSeller
		refreshed.CancellationResolvedAt = &now
		if err := s.repo.Order.Update(refreshed); err != nil {
			return nil, apperr.Internal("could not close empty order").Wrap(err)
		}
	}
	s.audit.Log(actor, "ORDER_ITEM_SELLER_CANCEL", "order_item", &item.ID, "Seller cancelled item "+item.InternalCode, nil)
	return s.getOrder(orderID)
}

// SellerRequestItemCancellation requests removal of one item without changing
// the parent order or its sibling items until Ops resolves the request.
func (s *ReviewService) SellerRequestItemCancellation(actor Actor, sellerID, orderID, itemID uint, reason string) (*models.Order, error) {
	order, err := s.getSellerOrder(sellerID, orderID)
	if err != nil {
		return nil, err
	}
	if sellerCancelActionForOrder(order) != SellerActionRequest {
		return nil, cancelActionError(sellerCancelActionForOrder(order))
	}
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
		return nil, apperr.Conflict("This order item already has a cancellation")
	}
	now := time.Now()
	item.CancellationStatus = models.CancellationRequested
	item.CancellationRequestedByID, item.CancellationRequestedAt = actor.IDPtr(), &now
	item.CancellationReason = strings.TrimSpace(reason)
	item.CancellationResolvedByID, item.CancellationResolvedAt, item.CancellationResolutionNote = nil, nil, ""
	if err := s.repo.OrderItem.Update(item); err != nil {
		return nil, apperr.Internal("could not request order item cancellation").Wrap(err)
	}
	_ = s.repo.Note.Create(&models.Note{Title: "Yêu cầu huỷ sản phẩm " + item.InternalCode, Body: "Seller yêu cầu huỷ sản phẩm. Lý do: " + item.CancellationReason, ReasonCode: "ITEM_CANCEL_REQUEST", Severity: models.SeverityHigh, Status: models.NoteOpen, IsRequiredAttention: true, EntityType: models.EntityOrderItem, EntityID: &item.ID, OwnerRole: models.RoleOps, CreatedByID: actor.IDPtr()})
	s.audit.Log(actor, "ORDER_ITEM_CANCEL_REQUEST", "order_item", &item.ID, "Seller requested cancellation of "+item.InternalCode, nil)
	return s.getOrder(orderID)
}

// ---------- Cancellation resolution (OPS / ADMIN) ----------

func (s *ReviewService) ListItemCancellationRequests(p repositories.Page) ([]models.OrderItem, int64, error) {
	return s.repo.OrderItem.ListCancellationRequests(p.Normalize())
}

func (s *ReviewService) ResolveItemCancellation(actor Actor, itemID uint, approve bool, note string) (*models.OrderItem, error) {
	item, err := s.repo.OrderItem.FindByID(itemID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order item not found")
		}
		return nil, apperr.Internal("lookup item failed").Wrap(err)
	}
	if item.CancellationStatus != models.CancellationRequested {
		return nil, apperr.Conflict("No pending cancellation request for this order item")
	}
	now := time.Now()
	if approve {
		item.CancellationStatus = models.CancellationApproved
	} else {
		item.CancellationStatus = models.CancellationRejected
	}
	item.CancellationResolvedByID, item.CancellationResolvedAt = actor.IDPtr(), &now
	item.CancellationResolutionNote = strings.TrimSpace(note)
	if err := s.repo.OrderItem.Update(item); err != nil {
		return nil, apperr.Internal("could not resolve order item cancellation").Wrap(err)
	}
	if approve {
		order, loadErr := s.getOrder(item.OrderID)
		if loadErr != nil {
			return nil, loadErr
		}
		allCancelled := len(order.Items) > 0
		for i := range order.Items {
			if !itemCancelled(order.Items[i].CancellationStatus) {
				allCancelled = false
				break
			}
		}
		if allCancelled {
			order.ReviewStatus, order.CancellationStatus = models.ReviewCancelled, models.CancellationApproved
			order.CancellationResolvedAt = &now
			if err := s.repo.Order.Update(order); err != nil {
				return nil, apperr.Internal("could not close empty order").Wrap(err)
			}
		}
	}
	action := "CANCEL_ITEM_REJECT"
	if approve {
		action = "CANCEL_ITEM_APPROVE"
	}
	s.audit.Log(actor, action, "order_item", &item.ID, "Resolved cancellation of "+item.InternalCode, nil)
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

// ApproveCancellation approves a pending cancellation request and cancels the
// order (removing it from the production flow).
func (s *ReviewService) ApproveCancellation(actor Actor, orderID uint, note string) (*models.Order, error) {
	order, err := s.getOrder(orderID)
	if err != nil {
		return nil, err
	}
	if order.CancellationStatus != models.CancellationRequested {
		return nil, apperr.Conflict("No pending cancellation request for this order")
	}
	now := time.Now()
	from := order.ReviewStatus
	order.CancellationStatus = models.CancellationApproved
	order.CancellationResolvedByID = actor.IDPtr()
	order.CancellationResolvedAt = &now
	order.CancellationResolutionNote = strings.TrimSpace(note)
	order.ReviewStatus = models.ReviewCancelled
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not approve cancellation").Wrap(err)
	}
	_ = recordStatus(s.repo, models.EntityOrder, order.ID, string(from), string(models.ReviewCancelled), actor, "cancellation approved")
	s.audit.Log(actor, "CANCEL_APPROVE", "order", &order.ID, "Approved cancellation of "+order.InternalCode, nil)
	return s.getOrder(order.ID)
}

// RejectCancellation denies a pending cancellation request; the order continues
// on its normal flow.
func (s *ReviewService) RejectCancellation(actor Actor, orderID uint, note string) (*models.Order, error) {
	order, err := s.getOrder(orderID)
	if err != nil {
		return nil, err
	}
	if order.CancellationStatus != models.CancellationRequested {
		return nil, apperr.Conflict("No pending cancellation request for this order")
	}
	now := time.Now()
	order.CancellationStatus = models.CancellationRejected
	order.CancellationResolvedByID = actor.IDPtr()
	order.CancellationResolvedAt = &now
	order.CancellationResolutionNote = strings.TrimSpace(note)
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not reject cancellation").Wrap(err)
	}
	s.audit.Log(actor, "CANCEL_REJECT", "order", &order.ID, "Rejected cancellation of "+order.InternalCode, nil)
	return s.getOrder(order.ID)
}
