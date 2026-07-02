package models

import "time"

// Batch is a production order grouped by a single material. A combo item that
// uses several materials is split across several batches (one batch line per
// material), which is why batches are material-scoped.
type Batch struct {
	Base
	Code        string         `json:"code" gorm:"uniqueIndex;size:32;not null"`
	MaterialID  uint           `json:"material_id" gorm:"index;not null"`
	Material    Material       `json:"material,omitempty" gorm:"foreignKey:MaterialID"`
	Status      InternalStatus `json:"status" gorm:"size:20;not null;index;default:'PENDING'"`
	Priority    Priority       `json:"priority" gorm:"size:20;not null;default:'NORMAL'"`
	DueDate     *time.Time     `json:"due_date"`
	Note        string         `json:"note" gorm:"size:500"`
	CreatedByID *uint          `json:"created_by_id"` // the Designer who created the batch
	CreatedBy   *User          `json:"created_by,omitempty" gorm:"foreignKey:CreatedByID"`

	Items []BatchItem `json:"items,omitempty" gorm:"foreignKey:BatchID"`
}

func (Batch) TableName() string { return "batches" }

// BatchItem is one (order item, material) production unit inside a batch. The
// unique index on (order_item_id, material_id) prevents the same item-material
// from being scheduled into two batches at once.
type BatchItem struct {
	Base
	BatchID     uint           `json:"batch_id" gorm:"index;not null"`
	Batch       *Batch         `json:"batch,omitempty" gorm:"foreignKey:BatchID"`
	OrderItemID uint           `json:"order_item_id" gorm:"index;not null;uniqueIndex:idx_item_material"`
	OrderItem   *OrderItem     `json:"order_item,omitempty" gorm:"foreignKey:OrderItemID"`
	MaterialID  uint           `json:"material_id" gorm:"index;not null;uniqueIndex:idx_item_material"`
	Material    *Material      `json:"material,omitempty" gorm:"foreignKey:MaterialID"`
	Status      InternalStatus `json:"status" gorm:"size:20;not null;index;default:'PENDING'"`
}

func (BatchItem) TableName() string { return "batch_items" }

// StatusHistory is an append-only audit of every status transition on an order,
// item, batch or batch item.
type StatusHistory struct {
	Base
	EntityType  EntityType `json:"entity_type" gorm:"size:20;not null;index:idx_status_hist_entity"`
	EntityID    uint       `json:"entity_id" gorm:"not null;index:idx_status_hist_entity"`
	FromStatus  string     `json:"from_status" gorm:"size:20"`
	ToStatus    string     `json:"to_status" gorm:"size:20"`
	ChangedByID *uint      `json:"changed_by_id"`
	Note        string     `json:"note" gorm:"size:255"`
}

func (StatusHistory) TableName() string { return "status_histories" }

// QCRecord is one quality-control comparison of a produced item against the
// seller's mockup. A PASS releases the item toward packing; a FAIL spawns a
// required-attention note for rework.
type QCRecord struct {
	Base
	OrderItemID uint     `json:"order_item_id" gorm:"index;not null"`
	BatchItemID *uint    `json:"batch_item_id" gorm:"index"`
	Result      QCResult `json:"result" gorm:"size:10;not null;index"`
	MockupURL   string   `json:"mockup_url" gorm:"size:500"` // snapshot of the reference compared against
	DefectCode  string   `json:"defect_code" gorm:"size:60"`
	Note        string   `json:"note" gorm:"size:500"`
	CheckedByID *uint    `json:"checked_by_id"`
}

func (QCRecord) TableName() string { return "qc_records" }
