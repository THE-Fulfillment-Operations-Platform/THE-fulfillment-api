package services

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"the-fulfillment/backend/internal/apperr"
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
	batchAll, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, err)
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

// TestStreamBatchAssetsZip_ReportsEveryFailedLink: batch #101068 thật — mọi item
// trỏ vào cùng một THƯ MỤC Drive dạng /drive/u/0/folders/. Lỗi trả về phải gọi
// đúng tên lý do (không phải "chưa chia sẻ") và kèm TỪNG link hỏng trong details
// để web hiện bảng lỗi, không chỉ một ví dụ trong câu toast.
func TestStreamBatchAssetsZip_ReportsEveryFailedLink(t *testing.T) {
	db := newScrapDB(t)
	svc := newBatchService(db)
	_, ids := seedSplit(t, db, 0, 3)
	const folder = "https://drive.google.com/drive/u/0/folders/1O_xR_Ovg"
	if err := db.Model(&models.OrderItem{}).Where("id IN ?", ids).
		Update("design_url", folder).Error; err != nil {
		t.Fatalf("set design urls: %v", err)
	}
	batchAll, _, err := svc.Create(Actor{ID: 1, Role: models.RoleDesigner}, CreateBatchInput{MaterialID: 1, OrderItemIDs: ids})
	batch := firstBatch(t, batchAll, err)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}

	err = svc.StreamBatchAssetsZip(context.Background(), io.Discard, batch.ID, true)
	ae, ok := apperr.As(err)
	if !ok || ae.Code != codeDesignDownloadFailed {
		t.Fatalf("err = %v, want code %s", err, codeDesignDownloadFailed)
	}
	if !strings.Contains(ae.Message, "3 link lỗi") || !strings.Contains(ae.Message, "THƯ MỤC") {
		t.Errorf("message must count the links and name the one shared reason: %q", ae.Message)
	}
	details, ok := ae.Details.(designDownloadFailure)
	if !ok || len(details.Failed) != 3 {
		t.Fatalf("details = %#v, want 3 failed links", ae.Details)
	}
	for _, f := range details.Failed {
		if f.Code != assetReasonDriveFolder || f.URL != folder || f.InternalCode == "" || f.SKU == "" || f.OrderID == 0 {
			t.Errorf("failed row incomplete: %+v", f)
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
