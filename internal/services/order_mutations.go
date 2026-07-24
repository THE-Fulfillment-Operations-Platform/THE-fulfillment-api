package services

import (
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// Order edit / cancel / delete. The permission matrix is enforced HERE (server
// side), never only by hiding a button:
//
//	Action  | SELLER (own order)                       | OPS            | ADMIN / OWNER
//	--------|------------------------------------------|----------------|--------------------------------
//	Edit    | only while PENDING_REVIEW/NEEDS_CORRECTION| pre-production | pre-production; OWNER may override
//	Cancel  | via seller cancel/request (review_service)| pre-shipping   | any (override) with reason
//	Delete  | never                                     | never          | soft delete (ADMIN pre-production, OWNER any)
//
// "pre-production" = no item batched or advanced past PENDING; "pre-shipping" =
// not packed/handed-off/shipped. Item-level field edits (SKU/qty/design) are
// blocked once an order is in production unless the actor is OWNER.

func isInternalManager(role models.Role) bool {
	return role == models.RoleOwner || role == models.RoleAdmin || role == models.RoleOps
}

// EditOrderItemInput edits one existing line item. Nil fields are left unchanged.
type EditOrderItemInput struct {
	ID            uint    `json:"id" binding:"required"`
	SKUCode       *string `json:"sku_code"`
	Quantity      *int    `json:"quantity"`
	ProductName   *string `json:"product_name"`
	VariantCode   *string `json:"variant_code"`
	DesignURL     *string `json:"design_url"`
	BackDesignURL *string `json:"back_design_url"`
	EngraveText   *string `json:"engrave_text"`
	Note          *string `json:"note"` // maps to qc_description (production/QC note)
}

// UpdateOrderInput edits an order's shipping data, note and (optionally) items.
// Every field is a pointer so "not sent" is distinct from "cleared".
type UpdateOrderInput struct {
	StoreOrderID     *string              `json:"store_order_id"`
	StoreName        *string              `json:"store_name"`
	ShippingMethod   *string              `json:"shipping_method"`
	ShippingName     *string              `json:"shipping_name"`
	ShippingAddress1 *string              `json:"shipping_address1"`
	ShippingAddress2 *string              `json:"shipping_address2"`
	ShippingCity     *string              `json:"shipping_city"`
	ShippingZip      *string              `json:"shipping_zip"`
	ShippingProvince *string              `json:"shipping_province"`
	ShippingCountry  *string              `json:"shipping_country"`
	ShippingPhone    *string              `json:"shipping_phone"`
	ShippingEmail    *string              `json:"shipping_email"`
	IOSS             *string              `json:"ioss"`
	Note             *string              `json:"note"`
	Items            []EditOrderItemInput `json:"items"`
}

// applyStr trims and applies a *string field, recording old→new in changes when it
// actually changes. Returns whether it changed.
func applyStr(changes models.JSONMap, name string, cur *string, in *string) bool {
	if in == nil {
		return false
	}
	v := strings.TrimSpace(*in)
	if v == *cur {
		return false
	}
	changes[name] = []string{*cur, v}
	*cur = v
	return true
}

// updateOrderCore applies an UpdateOrderInput to an order (and its items) inside a
// transaction and writes an audit entry. editItems controls whether line-item
// edits are permitted (callers decide based on role + production state). It assumes
// the caller has already authorized the actor and checked order state.
func (s *OrderService) updateOrderCore(actor Actor, order *models.Order, in UpdateOrderInput, editItems bool, auditAction string) (*models.Order, error) {
	changes := models.JSONMap{}

	// Validate item edits up front (before opening the transaction).
	if len(in.Items) > 0 {
		if !editItems {
			return nil, apperr.Conflict("Đơn đã vào sản xuất/batch — không thể sửa sản phẩm (chỉ OWNER được sửa).")
		}
		byID := map[uint]*models.OrderItem{}
		for i := range order.Items {
			byID[order.Items[i].ID] = &order.Items[i]
		}
		for _, iu := range in.Items {
			item, ok := byID[iu.ID]
			if !ok {
				return nil, apperr.BadRequest("Sản phẩm không thuộc đơn này")
			}
			if itemCancelled(item.CancellationStatus) {
				return nil, apperr.Conflict("Không thể sửa sản phẩm đã huỷ")
			}
			if iu.Quantity != nil && *iu.Quantity < 1 {
				return nil, apperr.Unprocessable("Số lượng phải >= 1")
			}
		}
	}

	err := s.repo.DB.Transaction(func(tx *gorm.DB) error {
		txRepo := repositories.New(tx)

		applyStr(changes, "store_order_id", &order.StoreOrderID, in.StoreOrderID)
		if in.StoreOrderID != nil {
			order.StoreOrderRef = order.StoreOrderID
		}
		applyStr(changes, "store_name", &order.StoreName, in.StoreName)
		applyStr(changes, "shipping_method", &order.ShippingMethod, in.ShippingMethod)
		applyStr(changes, "shipping_name", &order.ShippingName, in.ShippingName)
		applyStr(changes, "shipping_address1", &order.ShippingAddress1, in.ShippingAddress1)
		applyStr(changes, "shipping_address2", &order.ShippingAddress2, in.ShippingAddress2)
		applyStr(changes, "shipping_city", &order.ShippingCity, in.ShippingCity)
		applyStr(changes, "shipping_zip", &order.ShippingZip, in.ShippingZip)
		applyStr(changes, "shipping_province", &order.ShippingProvince, in.ShippingProvince)
		applyStr(changes, "shipping_country", &order.ShippingCountry, in.ShippingCountry)
		applyStr(changes, "shipping_phone", &order.ShippingPhone, in.ShippingPhone)
		applyStr(changes, "shipping_email", &order.ShippingEmail, in.ShippingEmail)
		applyStr(changes, "ioss", &order.IOSS, in.IOSS)
		applyStr(changes, "note", &order.Note, in.Note)
		if err := txRepo.Order.Update(order); err != nil {
			return err
		}

		byID := map[uint]*models.OrderItem{}
		for i := range order.Items {
			byID[order.Items[i].ID] = &order.Items[i]
		}
		for _, iu := range in.Items {
			item := byID[iu.ID]
			itemChanges := models.JSONMap{}
			if iu.SKUCode != nil {
				code := models.NormalizeCode(*iu.SKUCode)
				if code != item.SKUCode {
					itemChanges["sku_code"] = []string{item.SKUCode, code}
					item.SKUCode = code
					// Re-resolve the SKU FK so downstream material/mapping is correct.
					if sku, _ := txRepo.SKU.FindByCode(code); sku != nil {
						item.SKUID = &sku.ID
					} else {
						item.SKUID = nil
					}
				}
			}
			if iu.Quantity != nil && *iu.Quantity != item.Quantity {
				itemChanges["quantity"] = []int{item.Quantity, *iu.Quantity}
				item.Quantity = *iu.Quantity
			}
			applyStr(itemChanges, "product_name", &item.ProductName, iu.ProductName)
			applyStr(itemChanges, "variant_code", &item.VariantCode, iu.VariantCode)
			applyStr(itemChanges, "design_url", &item.DesignURL, iu.DesignURL)
			applyStr(itemChanges, "back_design_url", &item.BackDesignURL, iu.BackDesignURL)
			applyStr(itemChanges, "engrave_text", &item.EngraveText, iu.EngraveText)
			applyStr(itemChanges, "qc_description", &item.QCDescription, iu.Note)
			if len(itemChanges) > 0 {
				if err := txRepo.OrderItem.Update(item); err != nil {
					return err
				}
				changes["item_"+itoa(int(item.ID))] = itemChanges
			}
		}
		return nil
	})
	if err != nil {
		if ae, ok := apperr.As(err); ok {
			return nil, ae
		}
		return nil, apperr.Internal("could not update order").Wrap(err)
	}

	if len(changes) == 0 {
		return s.GetOrder(order.ID)
	}
	_ = recordStatus(s.repo, models.EntityOrder, order.ID, string(order.ReviewStatus), string(order.ReviewStatus), actor, "order edited")
	s.audit.Log(actor, auditAction, "order", &order.ID, "Edited order "+order.InternalCode, changes)
	return s.GetOrder(order.ID)
}

// UpdateOrder edits an order as an internal manager (OWNER/ADMIN/OPS). Item edits
// are blocked once the order is in production, except for OWNER.
func (s *OrderService) UpdateOrder(actor Actor, id uint, in UpdateOrderInput) (*models.Order, error) {
	if !isInternalManager(actor.Role) {
		return nil, apperr.Forbidden("Bạn không có quyền sửa đơn")
	}
	order, err := s.GetOrder(id)
	if err != nil {
		return nil, err
	}
	if order.ReviewStatus == models.ReviewRejected || order.ReviewStatus == models.ReviewCancelled {
		return nil, apperr.Conflict("Đơn đã huỷ/từ chối, không thể sửa")
	}
	if orderPacked(order.SellerStatus) {
		return nil, apperr.Conflict("Đơn đã đóng gói/bàn giao/gửi đi, không thể sửa")
	}
	editItems := !orderInProduction(order) || actor.Role == models.RoleOwner
	return s.updateOrderCore(actor, order, in, editItems, "ORDER_EDIT")
}

// SellerUpdateOrder lets a seller edit their own order only while it is still in
// review (PENDING_REVIEW / NEEDS_CORRECTION). Ownership and state are enforced
// server-side regardless of what the client sends.
func (s *OrderService) SellerUpdateOrder(actor Actor, sellerID, id uint, in UpdateOrderInput) (*models.Order, error) {
	order, err := s.GetOrder(id)
	if err != nil {
		return nil, err
	}
	if order.SellerID != sellerID {
		return nil, apperr.Forbidden("Đơn không thuộc tài khoản seller của bạn")
	}
	if order.ReviewStatus != models.ReviewPending && order.ReviewStatus != models.ReviewNeedsFix {
		return nil, apperr.Conflict("Chỉ sửa được đơn đang chờ duyệt hoặc cần chỉnh sửa")
	}
	return s.updateOrderCore(actor, order, in, true, "ORDER_EDIT_SELLER")
}

// CancelOrder is an internal (OPS/ADMIN/OWNER) manual cancellation. It records who
// cancelled, when and why, and removes the order from the production flow without
// deleting data. A packed/handed-off/shipped order can only be cancelled by
// ADMIN/OWNER (the "special permission" case).
func (s *OrderService) CancelOrder(actor Actor, id uint, reason string) (*models.Order, error) {
	if !isInternalManager(actor.Role) {
		return nil, apperr.Forbidden("Bạn không có quyền huỷ đơn")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, apperr.BadRequest("Cần nhập lý do huỷ")
	}
	order, err := s.GetOrder(id)
	if err != nil {
		return nil, err
	}
	if order.ReviewStatus == models.ReviewCancelled || itemCancelled(order.CancellationStatus) {
		return nil, apperr.Conflict("Đơn đã huỷ trước đó")
	}
	if orderPacked(order.SellerStatus) && actor.Role != models.RoleOwner && actor.Role != models.RoleAdmin {
		return nil, apperr.Conflict("Đơn đã đóng gói/gửi đi — chỉ Admin/Owner được huỷ")
	}
	now := time.Now()
	from := order.ReviewStatus
	order.ReviewStatus = models.ReviewCancelled
	order.CancellationStatus = models.CancellationApproved
	order.CancellationRequestedByID = actor.IDPtr()
	order.CancellationRequestedAt = &now
	order.CancellationReason = reason
	order.CancellationResolvedByID = actor.IDPtr()
	order.CancellationResolvedAt = &now
	order.CancellationResolutionNote = "Huỷ bởi vận hành"
	if err := s.repo.Order.Update(order); err != nil {
		return nil, apperr.Internal("could not cancel order").Wrap(err)
	}
	_ = recordStatus(s.repo, models.EntityOrder, order.ID, string(from), string(models.ReviewCancelled), actor, "ops cancelled: "+reason)
	s.audit.Log(actor, "ORDER_OPS_CANCEL", "order", &order.ID, "Cancelled order "+order.InternalCode,
		models.JSONMap{"reason": reason, "from": string(from)})
	return s.GetOrder(order.ID)
}

// DeleteOrder soft-deletes an order. Only ADMIN/OWNER may delete; ADMIN cannot
// delete an order already in production (OWNER may). Data is never hard-deleted
// (soft delete via deleted_at) so linked history is preserved; the order simply
// disappears from every operational list.
func (s *OrderService) DeleteOrder(actor Actor, id uint) error {
	if actor.Role != models.RoleOwner && actor.Role != models.RoleAdmin {
		return apperr.Forbidden("Chỉ Admin/Owner được xoá đơn")
	}
	order, err := s.GetOrder(id)
	if err != nil {
		return err
	}
	if orderInProduction(order) && actor.Role != models.RoleOwner {
		return apperr.Conflict("Đơn đang sản xuất — chỉ OWNER được xoá")
	}
	if err := s.repo.Order.SoftDelete(order.ID); err != nil {
		return apperr.Internal("could not delete order").Wrap(err)
	}
	s.audit.Log(actor, "ORDER_DELETE", "order", &order.ID, "Soft-deleted order "+order.InternalCode,
		models.JSONMap{"review_status": string(order.ReviewStatus), "seller_status": string(order.SellerStatus)})
	return nil
}
