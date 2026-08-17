package services

import (
	"sync"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"the-fulfillment/backend/internal/models"
)

// TestInsertBatchSize_StaysInsideParamBudget guards the shape of the import
// commit's INSERTs, which is what actually made a commit take a minute against
// the remote database — not the number of statements, which was already batched.
//
// A parameterised statement costs the database roughly in proportion to its
// parameter count, and orders are a 51-column table: the old flat batch of 200
// rows meant a ~10,000-placeholder INSERT. Every model the commit writes must
// stay inside the budget, and must keep doing so when someone adds a column.
func TestInsertBatchSize_StaysInsideParamBudget(t *testing.T) {
	db := newImportDB(t)

	for _, tc := range []struct {
		name  string
		model any
	}{
		{"orders", &models.Order{}},
		{"order_items", &models.OrderItem{}},
		{"item_assets", &models.ItemAsset{}},
		{"notes", &models.Note{}},
	} {
		size := insertBatchSize(db, tc.model)
		if size < 1 {
			t.Fatalf("%s: batch size %d, want at least 1", tc.name, size)
		}
		cols := columnCount(t, db, tc.model)
		if params := size * cols; params > maxStmtParams {
			t.Fatalf("%s: %d rows x %d cols = %d params, over the %d budget",
				tc.name, size, cols, params, maxStmtParams)
		}
		// A budget that collapses to a handful of rows per statement would trade the
		// parse cost back for round trips, which is the failure this fix came from.
		if size < 20 {
			t.Fatalf("%s: batch size %d is too small — %d columns would cost a round trip per few rows",
				tc.name, size, cols)
		}
	}
}

// columnCount reports how many columns a model writes, via the same schema parse
// insertBatchSize uses.
func columnCount(t *testing.T, db *gorm.DB, model any) int {
	t.Helper()
	s, err := schema.Parse(model, &sync.Map{}, db.NamingStrategy)
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return len(s.DBNames)
}
