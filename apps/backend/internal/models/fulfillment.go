package models

import "time"

// Package is a physical parcel for an order, built at the packing station. The
// MVP uses one package per order; package items track expected vs scanned.
type Package struct {
	Base
	Code        string        `json:"code" gorm:"uniqueIndex;size:32;not null"`
	OrderID     uint          `json:"order_id" gorm:"index;not null"`
	Order       *Order        `json:"order,omitempty" gorm:"foreignKey:OrderID"`
	Status      PackageStatus `json:"status" gorm:"size:20;not null;index;default:'OPEN'"`
	BoxType     string        `json:"box_type" gorm:"size:60"`
	WeightGrams int           `json:"weight_grams"`
	LengthCm    float64       `json:"length_cm"`
	WidthCm     float64       `json:"width_cm"`
	HeightCm    float64       `json:"height_cm"`
	PackingNote string        `json:"packing_note" gorm:"size:500"`
	PhotoURL    string        `json:"photo_url" gorm:"size:500"`
	PackedByID  *uint         `json:"packed_by_id"`
	PackedAt    *time.Time    `json:"packed_at"`

	Items []PackageItem `json:"items,omitempty" gorm:"foreignKey:PackageID"`
}

func (Package) TableName() string { return "packages" }

// PackageItem records the expected vs scanned quantity of an order item in a
// package. Packing is blocked from handoff while any scanned < expected.
type PackageItem struct {
	Base
	PackageID   uint       `json:"package_id" gorm:"index;not null;uniqueIndex:idx_pkg_item"`
	OrderItemID uint       `json:"order_item_id" gorm:"index;not null;uniqueIndex:idx_pkg_item"`
	OrderItem   *OrderItem `json:"order_item,omitempty" gorm:"foreignKey:OrderItemID"`
	ExpectedQty int        `json:"expected_qty" gorm:"not null;default:0"`
	ScannedQty  int        `json:"scanned_qty" gorm:"not null;default:0"`
}

func (PackageItem) TableName() string { return "package_items" }

// Handoff records the bàn giao of a package/order to THE. No real carrier API is
// called in the MVP; tracking/label fields are reserved for the later phase and
// populated by the shipping adapter when it becomes available.
type Handoff struct {
	Base
	Code           string        `json:"code" gorm:"uniqueIndex;size:40;not null"`
	OrderID        *uint         `json:"order_id" gorm:"index"`
	PackageID      *uint         `json:"package_id" gorm:"index"`
	Carrier        string        `json:"carrier" gorm:"size:40;not null;default:'THE'"`
	Status         HandoffStatus `json:"status" gorm:"size:20;not null;index;default:'HANDED_OFF'"`
	TrackingNumber string        `json:"tracking_number" gorm:"size:120"` // phase 2
	LabelURL       string        `json:"label_url" gorm:"size:500"`       // phase 2
	Note           string        `json:"note" gorm:"size:500"`
	HandedOffByID  *uint         `json:"handed_off_by_id"`
	HandedOffAt    time.Time     `json:"handed_off_at"`
}

func (Handoff) TableName() string { return "handoffs" }

// Note is an internal note / required-attention task. It is the single inbox for
// blocking issues (missing mockup, wrong SKU, QC fail, packing shortage) and
// carries an owner, severity, status and a link to the entity it concerns.
type Note struct {
	Base
	Title               string       `json:"title" gorm:"size:200;not null"`
	Body                string       `json:"body" gorm:"size:2000"`
	ReasonCode          string       `json:"reason_code" gorm:"size:60;index"` // from the exception catalog
	Severity            NoteSeverity `json:"severity" gorm:"size:20;not null;index;default:'NORMAL'"`
	Status              NoteStatus   `json:"status" gorm:"size:20;not null;index;default:'OPEN'"`
	IsRequiredAttention bool         `json:"is_required_attention" gorm:"not null;default:false;index"`

	EntityType EntityType `json:"entity_type" gorm:"size:20;index:idx_note_entity"`
	EntityID   *uint      `json:"entity_id" gorm:"index:idx_note_entity"`

	OwnerRole    Role       `json:"owner_role" gorm:"size:32"`
	AssignedToID *uint      `json:"assigned_to_id"`
	DueDate      *time.Time `json:"due_date"`
	Resolution   string     `json:"resolution" gorm:"size:1000"`
	CreatedByID  *uint      `json:"created_by_id"`
	ResolvedByID *uint      `json:"resolved_by_id"`
	ResolvedAt   *time.Time `json:"resolved_at"`
}

func (Note) TableName() string { return "notes" }

// AuditLog is a coarse audit trail of important actions (login, import commit,
// status changes, QC, handoff, ...). Metadata holds a small JSON snapshot.
type AuditLog struct {
	Base
	ActorID    *uint  `json:"actor_id" gorm:"index"`
	ActorEmail string `json:"actor_email" gorm:"size:160"`
	Action     string `json:"action" gorm:"size:80;not null;index"`
	EntityType string `json:"entity_type" gorm:"size:40;index"`
	EntityID   *uint  `json:"entity_id" gorm:"index"`
	Summary    string `json:"summary" gorm:"size:500"`
	Metadata   JSONB  `json:"metadata" gorm:"type:jsonb"`
	IP         string `json:"ip" gorm:"size:60"`
}

func (AuditLog) TableName() string { return "audit_logs" }
