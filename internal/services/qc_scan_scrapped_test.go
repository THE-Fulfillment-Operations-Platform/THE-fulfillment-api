package services

import (
	"testing"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// TestQCScan_FlagsScrappedParts: sau QC fail, lần quét kế tiếp phải nói ngay
// phần nào đã huỷ (kèm lý do và lần sản xuất) — để trạm khoá PASS từ lúc quét,
// thay vì để thợ bấm PASS rồi mới văng lỗi "phần sản xuất đã bị huỷ". Phần đã
// huỷ vẫn phải nằm trong danh sách: màn QC hiện lịch sử tấm đã vứt đi.
func TestQCScan_FlagsScrappedParts(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	qc := newQCService(db)
	actor := Actor{ID: 4, Role: models.RoleQC}
	item, parts := seedQCItem(t, db, 1)

	before, err := qc.Scan(actor, ScanRef{Code: item.InternalCode})
	if err != nil {
		t.Fatalf("scan trước khi fail: %v", err)
	}
	if len(before.Batches) != 1 {
		t.Fatalf("want 1 phần sản xuất, got %d", len(before.Batches))
	}
	if b := before.Batches[0]; b.Scrapped || b.ScrapReason != "" || b.Attempt != 1 {
		t.Fatalf("phần còn sống không được mang cờ huỷ, got %+v", b)
	}

	if _, err := qc.Fail(actor, QCDecisionInput{
		ScanRef: ScanRef{Code: item.InternalCode}, DefectCode: "PRINT_WRONG",
	}); err != nil {
		t.Fatalf("QC fail: %v", err)
	}

	after, err := qc.Scan(actor, ScanRef{Code: item.InternalCode})
	if err != nil {
		t.Fatalf("scan sau khi fail: %v", err)
	}
	if len(after.Batches) != 1 {
		t.Fatalf("phần đã huỷ vẫn phải được trả về (lịch sử), got %d phần", len(after.Batches))
	}
	b := after.Batches[0]
	if b.BatchItemID != parts[0].ID || !b.Scrapped || b.ScrapReason != "PRINT_WRONG" || b.Attempt != 1 {
		t.Fatalf("phần hỏng phải mang cờ huỷ + lý do + lần sản xuất, got %+v", b)
	}

	// Làm lại ở batch mới: quét ra hai phần — lần 1 đã huỷ, lần 2 còn sống.
	next, _ := repo.Batch.NextAttempts(1, []uint{item.ID})
	newBatch := &models.Batch{Code: "#101777", MaterialID: 1, Status: models.StatusCut}
	db.Create(newBatch)
	db.Create(&models.BatchItem{
		BatchID: newBatch.ID, OrderItemID: item.ID, MaterialID: 1,
		Status: models.StatusCut, Attempt: next[item.ID],
	})
	rescan, err := qc.Scan(actor, ScanRef{Code: item.InternalCode})
	if err != nil {
		t.Fatalf("scan sau khi gom batch mới: %v", err)
	}
	if len(rescan.Batches) != 2 {
		t.Fatalf("want 2 phần (1 huỷ + 1 làm lại), got %d", len(rescan.Batches))
	}
	var scrapped, live int
	for _, p := range rescan.Batches {
		if p.Scrapped {
			scrapped++
			continue
		}
		live++
		if p.Attempt != 2 || p.BatchCode != newBatch.Code || p.Status != models.StatusCut {
			t.Fatalf("phần làm lại phải là lần 2 ở batch mới, đã cắt, got %+v", p)
		}
	}
	if scrapped != 1 || live != 1 {
		t.Fatalf("want đúng 1 phần huỷ và 1 phần sống, got huỷ=%d sống=%d", scrapped, live)
	}
}
