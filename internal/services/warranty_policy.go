package services

import (
	"os"
	"strings"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// WarrantyPolicyConfig is the ONE place that holds the choices anh Kiên has not
// settled for the "Bảo hành" button (task #3092). Change a default here (or by the
// env var named on the field) instead of hunting through the service.
type WarrantyPolicyConfig struct {
	// (a) BillSeller: does a warranty order charge the seller? Nghĩa chốt 04/11:
	// KHÔNG — không tính khoản nào, phí ship cũng không (anh Kiên tự trả THE). FFM has no per-order price today (only the CancelBillable flag),
	// so this is stored on each warranty record (order_warranties.bill_seller) and
	// whatever bills sellers later reads it from there. Env WARRANTY_BILL_SELLER.
	BillSeller bool

	// (b) AllowedRoles: who may press the button (Nghĩa chốt 04/11: Owner, KTV,
	// vận hành, sản xuất, QC). Mapped to the existing roles: OWNER, TECHNICIAN,
	// OPS (vận hành), PRODUCTION (sản xuất), QC. ADMIN (quản lý) is kept too: it
	// has every tick OPS has. Designer, Packing, Shipping, CS and Seller are out.
	// The route only needs to read orders (PRODUCTION and QC hold no orders
	// "manage" tick); this list is the real gate. Edit it to change who may.
	AllowedRoles []models.Role

	// OverrideRoles may create a warranty order even while the chain still has an
	// unfinished one (the double-click guard). Default OWNER only.
	OverrideRoles []models.Role

	// (c) SuffixPrefix: the code of a warranty order is <root StoreOrderID> +
	// SuffixPrefix + <warranty_seq>. Chốt 04/11: "-BH" (SON20721-BH1).
	// Env WARRANTY_SUFFIX_PREFIX.
	SuffixPrefix string

	// ReturnedTrackingStatuses: FFM has no RETURNED state. The closest facts it
	// can see are the carrier saying the parcel was not delivered (UNDELIVERED) or
	// reporting an exception (EXCEPTION). An order in one of these (after
	// hand-off) gets the button too, besides the delivered ones.
	ReturnedTrackingStatuses []models.TrackingStatus
}

// WarrantyPolicy is the active policy. Tests may replace it.
var WarrantyPolicy = LoadWarrantyPolicy()

// LoadWarrantyPolicy builds the defaults, then applies env overrides.
func LoadWarrantyPolicy() WarrantyPolicyConfig {
	p := WarrantyPolicyConfig{
		BillSeller: false,
		AllowedRoles: []models.Role{
			models.RoleOwner, models.RoleTechnician, models.RoleAdmin,
			models.RoleOps, models.RoleProduction, models.RoleQC,
		},
		OverrideRoles:            []models.Role{models.RoleOwner},
		SuffixPrefix:             "-BH",
		ReturnedTrackingStatuses: []models.TrackingStatus{models.TrackingUndelivered, models.TrackingException},
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("WARRANTY_BILL_SELLER"))); v == "1" || v == "true" {
		p.BillSeller = true
	}
	if v := os.Getenv("WARRANTY_SUFFIX_PREFIX"); v != "" {
		p.SuffixPrefix = v
	}
	return p
}

func roleIn(r models.Role, list []models.Role) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}

// CanCreate: may this role press the button.
func (p WarrantyPolicyConfig) CanCreate(r models.Role) bool { return roleIn(r, p.AllowedRoles) }

// CanOverride: may this role skip the double-click guard.
func (p WarrantyPolicyConfig) CanOverride(r models.Role) bool { return roleIn(r, p.OverrideRoles) }

// Code builds the store order id of the seq-th warranty order of a chain.
func (p WarrantyPolicyConfig) Code(rootStoreOrderID string, seq int) string {
	return rootStoreOrderID + p.SuffixPrefix + itoa(seq)
}

// Eligible reports whether an order may be warrantied, and if not, why.
func (p WarrantyPolicyConfig) Eligible(o *models.Order) (bool, string) {
	if o.ReviewStatus != models.ReviewApproved {
		return false, "Đơn chưa được duyệt nên chưa thể bảo hành"
	}
	switch o.CancellationStatus {
	case models.CancellationSeller, models.CancellationApproved:
		return false, "Đơn đã huỷ"
	}
	if o.SellerStatus == models.SellerStatusDelivered || o.TrackingStatus == models.TrackingDelivered {
		return true, ""
	}
	if o.SellerStatus.HandedOver() {
		for _, t := range p.ReturnedTrackingStatuses {
			if o.TrackingStatus == t {
				return true, ""
			}
		}
	}
	return false, "Chỉ bảo hành được đơn đã giao hoặc đơn hãng báo trả về / giao không thành"
}

// EligibleScope is Eligible written as a SQL condition, for the "Có thể bảo
// hành" filter of the item list. It lives HERE, next to Eligible, and reads the
// same policy fields, so a rule changed in one place cannot be forgotten in the
// other (an earlier version kept two copies and they drifted). The test
// TestWarrantyEligibleScopeMatchesEligible walks every status combination and
// fails if the two ever disagree — edit both functions together.
//
// db must be a query that already joins `orders` (the item list does).
func (p WarrantyPolicyConfig) EligibleScope(db *gorm.DB) *gorm.DB {
	returned := make([]string, 0, len(p.ReturnedTrackingStatuses))
	for _, t := range p.ReturnedTrackingStatuses {
		returned = append(returned, string(t))
	}
	// Same boundary as Eligible (SellerStatus.HandedOver), not a second hand-written list.
	handedOver := []string{}
	for _, s := range []models.SellerStatus{
		models.SellerStatusProduction, models.SellerStatusPacked, models.SellerStatusHandedOff,
		models.SellerStatusShipped, models.SellerStatusDelivered,
	} {
		if s.HandedOver() {
			handedOver = append(handedOver, string(s))
		}
	}
	if len(returned) == 0 {
		returned = []string{""} // an empty IN () is invalid SQL; "" never matches NOT NULL tracking
	}
	return db.Where(
		"(orders.review_status = ? AND COALESCE(orders.cancellation_status, '') NOT IN ? AND "+
			"(orders.seller_status = ? OR orders.tracking_status = ? OR (orders.seller_status IN ? AND orders.tracking_status IN ?)))",
		string(models.ReviewApproved),
		[]string{string(models.CancellationSeller), string(models.CancellationApproved)},
		string(models.SellerStatusDelivered), string(models.TrackingDelivered),
		handedOver, returned,
	)
}
