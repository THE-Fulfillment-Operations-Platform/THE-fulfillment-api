package repositories

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// Batch trả kèm danh sách seller (chủ xưởng 25/11: "hiện tên seller ở batch SX"):
// một batch nhiều seller thì mỗi seller một lần, không seller thì [] chứ không
// null, phần đã huỷ ở QC không tính.
func TestBatchList_Sellers(t *testing.T) {
	db := newQueueTestDB(t)
	s1, s2, _ := multiSeedSellers(t, db, "BS")
	mat := &models.Material{Code: "BS-MAT", Name: "Gỗ BS"}
	if err := db.Create(mat).Error; err != nil {
		t.Fatal(err)
	}
	mkBatch := func(code string, items ...*models.OrderItem) *models.Batch {
		b := &models.Batch{Code: code, MaterialID: mat.ID, Status: models.StatusPending}
		if err := db.Create(b).Error; err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if err := db.Create(&models.BatchItem{BatchID: b.ID, OrderItemID: it.ID, MaterialID: mat.ID, Status: models.StatusPending}).Error; err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	prod := models.SellerStatusProduction
	a1 := multiSeedItem(t, db, "510001", s2, prod, models.StatusPending)
	a2 := multiSeedItem(t, db, "510002", s1, prod, models.StatusPending)
	a3 := multiSeedItem(t, db, "510003", s1, prod, models.StatusPending)
	bMulti := mkBatch("#BS-MULTI", a1, a2, a3)
	bEmpty := mkBatch("#BS-EMPTY")

	repo := &BatchRepository{db: db}
	rows, _, err := repo.List(BatchFilter{Page: Page{Page: 1, PageSize: 20}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint][]models.BatchSeller{}
	for _, b := range rows {
		got[b.ID] = b.Sellers
		if b.Sellers == nil {
			t.Fatalf("%s: Sellers nil, cần []", b.Code)
		}
	}
	if m := got[bMulti.ID]; len(m) != 2 || m[0].ID != s1.ID || m[1].ID != s2.ID {
		t.Fatalf("batch nhiều seller: %+v (cần s1,s2 theo tên, không trùng)", m)
	}
	if len(got[bEmpty.ID]) != 0 {
		t.Fatalf("batch rỗng: %+v", got[bEmpty.ID])
	}
	// Huỷ phần của s2 ở QC: s2 rơi khỏi danh sách.
	if err := db.Model(&models.BatchItem{}).Where("order_item_id = ?", a1.ID).Update("scrapped_at", "2026-11-01 00:00:00").Error; err != nil {
		t.Fatal(err)
	}
	m, err := repo.SellersByBatch([]uint{bMulti.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(m[bMulti.ID]) != 1 || m[bMulti.ID][0].ID != s1.ID {
		t.Fatalf("sau khi huỷ: %+v", m[bMulti.ID])
	}
}
