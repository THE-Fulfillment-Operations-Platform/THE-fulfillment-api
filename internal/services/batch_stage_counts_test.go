package services

import (
	"reflect"
	"testing"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// GET /api/batches/stage-counts: luôn có ĐỦ mọi chặng đã biết (0 nếu không có) để
// ô Trạng thái hiện đủ lựa chọn kèm số; by_status tách theo trạng thái lưu (cột
// mặc định của bảng); trạng thái lạ trong DB được nối vào cuối `order`.
func TestBatchStageCounts_Shape(t *testing.T) {
	db := newQCDB(t)
	if err := db.AutoMigrate(&models.BatchLink{}); err != nil {
		t.Fatal(err)
	}
	repo := repositories.New(db)
	svc := &BatchService{repo: repo}

	empty, err := svc.StageCounts(repositories.BatchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Stages) != len(repositories.AllBatchStages) || !reflect.DeepEqual(empty.Order, repositories.AllBatchStages) || empty.Rows == nil {
		t.Fatalf("DB rỗng: phải đủ %d chặng = 0, order chuẩn, rows []: %+v", len(repositories.AllBatchStages), empty)
	}
	for _, st := range repositories.AllBatchStages {
		if v, ok := empty.Stages[st]; !ok || v != 0 {
			t.Errorf("DB rỗng: stage %s = %v (có=%v)", st, v, ok)
		}
	}

	mat := &models.Material{Code: "SC-M", Name: "SC"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatal(err)
	}
	// Ba batch không có phần nào (NO_ITEMS) ở PENDING / CUT, và một batch trạng thái lạ.
	for i, st := range []models.InternalStatus{models.StatusPending, models.StatusCut, models.StatusCut, "LEGACY_X"} {
		b := &models.Batch{Code: "#SC-" + string(rune('A'+i)), MaterialID: mat.ID, Status: st}
		if err := db.Create(b).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.StageCounts(repositories.BatchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stages[repositories.BatchStageNoItems] != 3 || got.Stages["LEGACY_X"] != 1 {
		t.Errorf("stages: %+v", got.Stages)
	}
	if got.ByStatus["CUT"][repositories.BatchStageNoItems] != 2 || got.ByStatus["PENDING"][repositories.BatchStageNoItems] != 1 {
		t.Errorf("by_status: %+v", got.ByStatus)
	}
	if last := got.Order[len(got.Order)-1]; last != "LEGACY_X" || len(got.Order) != len(repositories.AllBatchStages)+1 {
		t.Errorf("order phải nối LEGACY_X vào cuối: %v", got.Order)
	}
}
