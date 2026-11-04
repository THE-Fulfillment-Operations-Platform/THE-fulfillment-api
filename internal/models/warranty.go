package models

// WarrantyReason is why a delivered (or carrier-returned) order is sent again.
type WarrantyReason string

const (
	WarrantyReasonShippingDamage WarrantyReason = "SHIPPING_DAMAGE" // vỡ do vận chuyển
	WarrantyReasonWrongItem      WarrantyReason = "WRONG_ITEM"      // sai hàng
	WarrantyReasonMissingItem    WarrantyReason = "MISSING_ITEM"    // thiếu hàng
	WarrantyReasonOther          WarrantyReason = "OTHER"           // khác (ghi chú)
)

// Valid reports whether r is a known reason.
func (r WarrantyReason) Valid() bool {
	switch r {
	case WarrantyReasonShippingDamage, WarrantyReasonWrongItem, WarrantyReasonMissingItem, WarrantyReasonOther:
		return true
	}
	return false
}

// WarrantyScope: re-ship everything, or only the chosen products.
type WarrantyScope string

const (
	WarrantyScopeFull    WarrantyScope = "FULL"
	WarrantyScopePartial WarrantyScope = "PARTIAL"
)

// OrderWarranty is the record of one warranty re-ship, one row per warranty order.
// It is what the "tháng này hãng X làm vỡ bao nhiêu đơn" report is built from, so
// every answer the form asks for is a column here, not text in a note.
type OrderWarranty struct {
	Base
	// OrderID is the NEW warranty order (unique: one record per warranty order).
	OrderID       uint `json:"order_id" gorm:"uniqueIndex;not null"`
	ParentOrderID uint `json:"parent_order_id" gorm:"index;not null"`
	RootOrderID   uint `json:"root_order_id" gorm:"index;not null"`
	Seq           int  `json:"seq" gorm:"not null"`

	Reason     WarrantyReason `json:"reason" gorm:"size:20;not null;index"`
	ReasonNote string         `json:"reason_note" gorm:"size:1000"`
	Scope      WarrantyScope  `json:"scope" gorm:"size:10;not null"`
	// AddressChanged: the form's "giữ địa chỉ cũ hay sửa" answer.
	AddressChanged bool `json:"address_changed" gorm:"not null;default:false"`
	// BillSeller: whether the seller is charged for this re-ship. Copied from
	// WarrantyPolicy.BillSeller at creation, so changing the policy later never
	// rewrites what was agreed for an order already sent.
	BillSeller bool `json:"bill_seller" gorm:"not null;default:false"`

	// Snapshot of the parent when the button was pressed — the evidence for a claim
	// against the carrier (the parent itself keeps moving afterwards).
	SourceSellerStatus   SellerStatus   `json:"source_seller_status" gorm:"size:20"`
	SourceTrackingStatus TrackingStatus `json:"source_tracking_status" gorm:"size:20"`
	SourceTrackingNumber string         `json:"source_tracking_number" gorm:"size:120"`

	CreatedByID *uint `json:"created_by_id"`
}

func (OrderWarranty) TableName() string { return "order_warranties" }
