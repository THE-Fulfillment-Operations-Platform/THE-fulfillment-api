package services

import (
	"bytes"
	"context"
	"testing"

	"the-fulfillment/backend/internal/models"
)

// brokenDesignURL is refused by the SSRF guard before any dial, so the tests
// exercise "every link failed" without touching the network.
const brokenDesignURL = "http://127.0.0.1/design.png"

// TestStreamBatchAssetsZip_NothingDownloadedWritesNoBytes khoá lại đúng con bug
// batch #101068: mọi link design đều tải hỏng, service trả lỗi — nhưng một
// `defer zw.Close()` vẫn ghi 22 byte "cuối archive" ra response. Handler thấy
// body đã bắt đầu nên không đổi sang JSON lỗi được, người dùng nhận một
// Batch_<code>.zip rỗng mà Finder báo "không chứa mục nào". Không có file nào
// tải được thì KHÔNG được ghi byte nào, để lý do lỗi tới được người dùng.
func TestStreamBatchAssetsZip_NothingDownloadedWritesNoBytes(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 2)
	if err := db.Model(&models.OrderItem{}).Where("id IN ?", ids).
		Update("design_url", brokenDesignURL).Error; err != nil {
		t.Fatalf("set design urls: %v", err)
	}
	batch, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}

	for _, designOnly := range []bool{true, false} {
		var buf bytes.Buffer
		err := svc.StreamBatchAssetsZip(context.Background(), &buf, batch.ID, designOnly)
		if err == nil {
			t.Fatalf("designOnly=%v: want an error when no file downloaded", designOnly)
		}
		if buf.Len() != 0 {
			t.Fatalf("designOnly=%v: wrote %d bytes, want 0 — an empty ZIP hides the error", designOnly, buf.Len())
		}
	}
}

// TestStreamDesignAssetsZip_NothingDownloadedWritesNoBytes: cùng lời hứa cho nút
// tải design ở màn "Chờ thiết kế".
func TestStreamDesignAssetsZip_NothingDownloadedWritesNoBytes(t *testing.T) {
	db := newDownloadableDB(t)
	svc := newOrderService(db)
	it := seedDownloadable(t, db, "100001_1/1", "SKU-A", models.DesignPending, brokenDesignURL, true)

	var buf bytes.Buffer
	err := svc.StreamDesignAssetsZip(context.Background(), &buf, DesignZipQuery{IDs: []uint{it.ID}}, "Design_2026-09-18")
	if err == nil {
		t.Fatal("want an error when no file downloaded")
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes, want 0 — an empty ZIP hides the error", buf.Len())
	}
}
