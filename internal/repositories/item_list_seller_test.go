package repositories

import (
	"testing"

	"the-fulfillment/backend/internal/models"
)

// Màn Đơn hàng lọc theo seller để trả lời "đơn nào của seller nào". Bộ lọc nằm
// trên bảng orders chứ không trên order_items, nên nó chỉ đúng khi câu truy vấn
// đi qua join sang orders — khai báo trường mà quên vế WHERE thì bộ lọc lặng lẽ
// trả về mọi thứ, và người dùng tin là seller đó có toàn bộ số đơn ấy.
func TestItemList_FiltersBySeller(t *testing.T) {
	db := newQueueTestDB(t)
	one := &models.Seller{Code: "S1", Name: "Seller Một"}
	two := &models.Seller{Code: "S2", Name: "Seller Hai"}
	if err := db.Create(one).Error; err != nil {
		t.Fatalf("seed seller 1: %v", err)
	}
	if err := db.Create(two).Error; err != nil {
		t.Fatalf("seed seller 2: %v", err)
	}

	// seedQueueItem gắn mọi đơn vào seller 1; đẩy riêng một đơn sang seller 2.
	mine := seedQueueItem(t, db, "100001_1/1", models.DesignReady)
	other := seedQueueItem(t, db, "100002_1/1", models.DesignReady)
	if err := db.Model(&models.Order{}).Where("id = ?", other.OrderID).
		Update("seller_id", two.ID).Error; err != nil {
		t.Fatalf("chuyển đơn sang seller 2: %v", err)
	}

	repo := &OrderItemRepository{db: db}
	rows, total, err := repo.List(ItemFilter{
		Page:     Page{Page: 1, PageSize: 20},
		SellerID: &one.ID,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("lọc theo seller 1 phải ra đúng 1 dòng, got total=%d len=%d", total, len(rows))
	}
	if rows[0].ID != mine.ID {
		t.Fatalf("trả về sản phẩm của seller khác: item %d", rows[0].ID)
	}

	// Không lọc thì thấy cả hai — nếu không, cái test trên "đạt" chỉ vì dữ liệu rỗng.
	_, all, err := repo.List(ItemFilter{Page: Page{Page: 1, PageSize: 20}})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if all != 2 {
		t.Fatalf("không lọc phải thấy 2 dòng, got %d", all)
	}
}

// Cột Seller trên màn Đơn hàng đọc order.seller, mà preload đó là opt-in: bật cờ
// mà không có dữ liệu thì cột hiện toàn dấu gạch dù seller vẫn ở đó.
func TestItemList_WithSellerFillsTheColumn(t *testing.T) {
	db := newQueueTestDB(t)
	seller := &models.Seller{Code: "S1", Name: "Seller Một"}
	if err := db.Create(seller).Error; err != nil {
		t.Fatalf("seed seller: %v", err)
	}
	seedQueueItem(t, db, "100001_1/1", models.DesignReady) // SellerID = 1

	repo := &OrderItemRepository{db: db}
	rows, _, err := repo.List(ItemFilter{
		Page: Page{Page: 1, PageSize: 20}, WithSeller: true,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Order == nil {
		t.Fatalf("want 1 dòng kèm đơn, got %d", len(rows))
	}
	if rows[0].Order.Seller.Name != "Seller Một" {
		t.Fatalf("cột Seller không có dữ liệu: %+v", rows[0].Order.Seller)
	}
}
