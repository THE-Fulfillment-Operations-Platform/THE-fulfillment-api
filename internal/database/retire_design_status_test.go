package database

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
)

// Items left at PENDING / IN_PROGRESS / MISSING from before the design step was
// retired are set READY once; a later run (data_migrations row) changes nothing.
func TestRetireDesignStatus_RunsOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.OrderItem{}, &models.DataMigration{}); err != nil {
		t.Fatal(err)
	}
	for i, st := range []models.DesignStatus{models.DesignPending, models.DesignInProgress, models.DesignMissing, models.DesignReady} {
		if err := db.Create(&models.OrderItem{OrderID: 1, InternalCode: "X" + string(rune('A'+i)), SKUCode: "S", Quantity: 1,
			InternalStatus: models.StatusPending, DesignStatus: st}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := retireDesignStatus(db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	var notReady int64
	db.Model(&models.OrderItem{}).Where("design_status <> ?", models.DesignReady).Count(&notReady)
	if notReady != 0 {
		t.Fatalf("%d item(s) still not READY", notReady)
	}
	// Second run is a no-op even if something is PENDING again.
	db.Model(&models.OrderItem{}).Where("internal_code = ?", "XA").Update("design_status", models.DesignPending)
	if err := retireDesignStatus(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	db.Model(&models.OrderItem{}).Where("design_status <> ?", models.DesignReady).Count(&notReady)
	if notReady != 1 {
		t.Fatalf("the migration must run only once, changed %d", 1-notReady)
	}
}
