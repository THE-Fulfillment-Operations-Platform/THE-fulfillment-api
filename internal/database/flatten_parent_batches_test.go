package database

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// legacyBatch is the batches table as it stood BEFORE the parent/child layer was
// retired: the four extra columns the live database still carries. Migrating
// with it reproduces a production table; the model itself no longer has them.
type legacyBatch struct {
	models.Batch
	ParentBatchID *uint `gorm:"index"`
	IsParent      bool  `gorm:"not null;default:false"`
	Sequence      int   `gorm:"not null;default:0"`
	ChildCount    int   `gorm:"not null;default:0"`
}

func (legacyBatch) TableName() string { return "batches" }

// TestFlattenParentBatches: a parent with two children becomes two ordinary
// batches (codes, items and status untouched) and the parent disappears; a
// second run changes nothing; a fresh schema without the columns is a no-op.
func TestFlattenParentBatches(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Material{}, &legacyBatch{}, &models.BatchItem{}); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	parent := &legacyBatch{Batch: models.Batch{Code: "#101034", MaterialID: 1, Status: models.StatusPending}, IsParent: true, ChildCount: 2}
	if err := db.Create(parent).Error; err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	pid := parent.ID
	for i, code := range []string{"#101034-1", "#101034-2"} {
		c := &legacyBatch{Batch: models.Batch{Code: code, MaterialID: 1, Status: models.StatusPrinted}, ParentBatchID: &pid, Sequence: i + 1}
		if err := db.Create(c).Error; err != nil {
			t.Fatalf("seed child: %v", err)
		}
	}
	flat := &legacyBatch{Batch: models.Batch{Code: "#101040", MaterialID: 1, Status: models.StatusPending}}
	if err := db.Create(flat).Error; err != nil {
		t.Fatalf("seed flat: %v", err)
	}

	for run := 1; run <= 2; run++ {
		if err := flattenParentBatches(db); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		var live []legacyBatch
		if err := db.Order("id").Find(&live).Error; err != nil {
			t.Fatalf("list: %v", err)
		}
		codes := make([]string, 0, len(live))
		for _, b := range live {
			codes = append(codes, b.Code)
			if b.ParentBatchID != nil || b.Sequence != 0 {
				t.Fatalf("run %d: %s still filed under a parent: %+v", run, b.Code, b)
			}
		}
		if len(live) != 3 || codes[0] != "#101034-1" || codes[1] != "#101034-2" || codes[2] != "#101040" {
			t.Fatalf("run %d: live batches = %v, want the two former children + the flat one", run, codes)
		}
		if live[0].Status != models.StatusPrinted {
			t.Fatalf("run %d: child status must survive, got %s", run, live[0].Status)
		}
		var gone legacyBatch
		if err := db.Unscoped().First(&gone, pid).Error; err != nil {
			t.Fatalf("parent row must remain (soft-deleted): %v", err)
		}
		if gone.DeletedAt.Time.IsZero() || gone.CloseReason == "" {
			t.Fatalf("parent must be soft-deleted with a reason, got %+v", gone)
		}
	}

	// A fresh database has no such column: nothing to do, no error.
	fresh, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err := fresh.AutoMigrate(&models.Batch{}); err != nil {
		t.Fatalf("migrate fresh: %v", err)
	}
	if err := flattenParentBatches(fresh); err != nil {
		t.Fatalf("fresh schema: %v", err)
	}
}
