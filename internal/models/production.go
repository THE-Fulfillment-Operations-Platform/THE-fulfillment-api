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
	Links []BatchLink `json:"links,omitempty" gorm:"foreignKey:BatchID"`

	// ---- Parent/child batches (split by a material's production quota) ----
	// A "parent" batch groups several "child" batches; each child holds at most
	// Material.ProductsPerUnit products. A flat (un-split) batch leaves all of these
	// at their zero value. Children carry ParentBatchID + Sequence and hold the
	// items; the parent holds no items and only aggregates the children.
	ParentBatchID *uint   `json:"parent_batch_id" gorm:"index"`
	IsParent      bool    `json:"is_parent" gorm:"not null;default:false"`
	Sequence      int     `json:"sequence" gorm:"not null;default:0"`    // 1..k position of a child within its parent
	ChildCount    int     `json:"child_count" gorm:"not null;default:0"` // number of children (on the parent)
	ChildBatches  []Batch `json:"child_batches,omitempty" gorm:"foreignKey:ParentBatchID"`
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

// BatchLink is a production link (print or cut) attached to a whole batch, so a
// print/cut URL is entered once per batch rather than repeated on every item's
// design. Kind distinguishes PRINT vs CUT; the partial unique index
// idx_batch_link_kind on (batch_id, kind) WHERE deleted_at IS NULL prevents
// duplicate links for the same (batch, kind) — re-adding updates the existing row
// instead of creating a second one. Modelled as a row-per-kind (not two columns on
// batches) so a batch can carry additional link groups later without a schema
// change. UpdatedByID/LinkUpdatedAt record who last set the link and when.
type BatchLink struct {
	Base
	BatchID      uint          `json:"batch_id" gorm:"not null;uniqueIndex:idx_batch_link_kind,where:deleted_at IS NULL"`
	Batch        *Batch        `json:"batch,omitempty" gorm:"foreignKey:BatchID"`
	Kind         BatchLinkKind `json:"kind" gorm:"size:10;not null;uniqueIndex:idx_batch_link_kind,where:deleted_at IS NULL"`
	URL          string        `json:"url" gorm:"size:500;not null"`
	UpdatedByID  *uint         `json:"updated_by_id"`
	UpdatedBy    *User         `json:"updated_by,omitempty" gorm:"foreignKey:UpdatedByID"`
	LinkUpdatedAt time.Time    `json:"link_updated_at"`
}

func (BatchLink) TableName() string { return "batch_links" }

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
