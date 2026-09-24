package models

import (
	"math/big"
	"time"
)

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

	// ClosedAt marks a production run that has nothing left to produce because
	// every piece it made was scrapped at QC. Without it such a batch sits on the
	// board forever showing zero items and whatever status it had reached — it can
	// never advance (no live parts) and never disappear. Re-making the product
	// happens in a NEW batch, so this one is simply finished.
	ClosedAt    *time.Time `json:"closed_at,omitempty" gorm:"index"`
	CloseReason string     `json:"close_reason,omitempty" gorm:"size:120"`

	// ScrappedCount is not stored: list/detail queries fill it so the UI can say
	// "0 còn lại · 1 đã huỷ" instead of showing a batch that looks empty.
	ScrappedCount int `json:"scrapped_count" gorm:"-"`
	// MaterialUnits is how many sheets of the material this batch needs:
	// ⌈Σ quantity / quota⌉ over its live parts, quota per product from the SKU's
	// and the material's sizes. A parent reports its child count (each child is
	// at most one sheet). nil when any part has no quota (a size is missing) —
	// the list then shows "—" instead of a number nobody can vouch for. Not
	// stored; list/detail queries fill it.
	MaterialUnits *int `json:"material_units" gorm:"-"`

	// ---- Parent/child batches (split by a material's production quota) ----
	// A "parent" batch groups several "child" batches; each child fills at most
	// ONE sheet of the material (see ProductionQuota). A flat (un-split) batch leaves all of these
	// at their zero value. Children carry ParentBatchID + Sequence and hold the
	// items; the parent holds no items and only aggregates the children.
	ParentBatchID *uint   `json:"parent_batch_id" gorm:"index"`
	IsParent      bool    `json:"is_parent" gorm:"not null;default:false"`
	Sequence      int     `json:"sequence" gorm:"not null;default:0"`    // 1..k position of a child within its parent
	ChildCount    int     `json:"child_count" gorm:"not null;default:0"` // number of children (on the parent)
	ChildBatches  []Batch `json:"child_batches,omitempty" gorm:"foreignKey:ParentBatchID"`
}

func (Batch) TableName() string { return "batches" }

// FillMaterialUnits computes MaterialUnits from the batch's loaded live parts:
// ⌈Σ quantity / quota⌉ with each part's quota derived from its SKU's size and
// `material` (the batch's own — passed in because a child batch loaded under
// its parent carries no Material of its own). Items must be loaded with their
// OrderItem and its SKU. A parent reports its child count (each child is one
// SKU within its quota, i.e. one sheet — the detail query replaces this with
// the children's exact sum, which only differs when a single order line
// exceeds its quota). nil when any part has no quota (a size is missing on
// either side) — the screen then shows "—" instead of a number nobody can
// vouch for. The sum is added as exact fractions: at the boundary where a
// sheet is precisely full, float rounding would turn "one sheet" into two.
func (b *Batch) FillMaterialUnits(material *Material) {
	b.MaterialUnits = nil
	if b.IsParent {
		n := b.ChildCount
		b.MaterialUnits = &n
		return
	}
	sum := new(big.Rat)
	for i := range b.Items {
		it := b.Items[i].OrderItem
		if it == nil {
			return
		}
		quota := ProductionQuota(it.SKU, material)
		if quota == 0 {
			return
		}
		qty := it.Quantity
		if qty < 1 {
			qty = 1
		}
		sum.Add(sum, big.NewRat(int64(qty), int64(quota)))
	}
	// ⌈num/den⌉ in integers.
	num, den := new(big.Int).Set(sum.Num()), sum.Denom()
	num.Add(num, den).Sub(num, big.NewInt(1)).Quo(num, den)
	n := int(num.Int64())
	b.MaterialUnits = &n
}

// BatchItem is one (order item, material) production unit inside a batch. The
// unique index on (order_item_id, material_id) prevents the same item-material
// from being scheduled into two batches at once.
type BatchItem struct {
	Base
	BatchID     uint           `json:"batch_id" gorm:"index;not null"`
	Batch       *Batch         `json:"batch,omitempty" gorm:"foreignKey:BatchID"`
	OrderItemID uint           `json:"order_item_id" gorm:"index;not null;uniqueIndex:idx_item_material_attempt"`
	OrderItem   *OrderItem     `json:"order_item,omitempty" gorm:"foreignKey:OrderItemID"`
	MaterialID  uint           `json:"material_id" gorm:"index;not null;uniqueIndex:idx_item_material_attempt"`
	Material    *Material      `json:"material,omitempty" gorm:"foreignKey:MaterialID"`
	Status      InternalStatus `json:"status" gorm:"size:20;not null;index;default:'PENDING'"`

	// Attempt is which production run of this (item, material) the row is: 1 for
	// the first, 2 after a QC fail sent it back to be re-made, and so on. It is
	// part of the unique key because a product CAN legitimately be produced more
	// than once — the original schema assumed once, which is exactly why a failed
	// item could never re-enter batching.
	Attempt int `json:"attempt" gorm:"not null;default:1;uniqueIndex:idx_item_material_attempt"`

	// ScrappedAt marks a part that failed QC and was written off. The row stays in
	// its batch — that is where the defective piece was actually produced, and the
	// QC record points at it — but it no longer counts anywhere: not in the item's
	// or batch's status roll-up (else the old batch could never close), and not in
	// the "already batched" test (else the item could never be re-batched).
	ScrappedAt   *time.Time `json:"scrapped_at,omitempty" gorm:"index"`
	ScrapReason  string     `json:"scrap_reason,omitempty" gorm:"size:60"`
	ScrappedByID *uint      `json:"scrapped_by_id,omitempty"`
}

// Scrapped reports whether this production part was written off after a QC fail.
func (b BatchItem) Scrapped() bool { return b.ScrappedAt != nil }

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
	BatchID       uint          `json:"batch_id" gorm:"not null;uniqueIndex:idx_batch_link_kind,where:deleted_at IS NULL"`
	Batch         *Batch        `json:"batch,omitempty" gorm:"foreignKey:BatchID"`
	Kind          BatchLinkKind `json:"kind" gorm:"size:10;not null;uniqueIndex:idx_batch_link_kind,where:deleted_at IS NULL"`
	URL           string        `json:"url" gorm:"size:500;not null"`
	UpdatedByID   *uint         `json:"updated_by_id"`
	UpdatedBy     *User         `json:"updated_by,omitempty" gorm:"foreignKey:UpdatedByID"`
	LinkUpdatedAt time.Time     `json:"link_updated_at"`
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
