package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
)

// Printing a label bought outside FFM (orders.external_label_url, set by the
// Excel import). The warehouse's browser cannot fetch the carrier's PDF itself
// (cross-origin), so the server downloads it and hands the bytes back — through
// the SSRF-guarded client, with a hard timeout and size cap, so a dead or
// hostile link can neither hang the request nor fill memory.

const (
	// externalLabelTimeout bounds the whole download of one label.
	externalLabelTimeout = 20 * time.Second
	// maxExternalLabelBytes: a 4x6 label PDF is tens of KB; 15 MB is generous.
	maxExternalLabelBytes int64 = 15 << 20
)

// ExternalLabel downloads the order's outside label for printing.
func (s *OrderService) ExternalLabel(ctx context.Context, actor Actor, orderID uint) (*LabelFile, error) {
	if !canSeeShipments(actor) {
		return nil, apperr.Forbidden("Bạn không có quyền in label")
	}
	order, err := s.repo.Order.FindByID(orderID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Order not found")
		}
		return nil, apperr.Internal("lookup failed").Wrap(err)
	}
	link := strings.TrimSpace(order.ExternalLabelURL)
	if link == "" {
		return nil, apperr.NotFound("Đơn này chưa có label ngoài — nhập link label bằng Excel ở màn Hành trình đơn hàng")
	}

	client := s.extLabelClient
	if client == nil {
		if _, err := validatePublicHTTPURL(link); err != nil {
			return nil, apperr.BadRequest("Link label không dùng được: " + err.Error())
		}
		client = newSafeAssetClient(externalLabelTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, externalLabelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, apperr.BadRequest("Link label không hợp lệ")
	}
	req.Header.Set("Accept", "application/pdf,image/*;q=0.9,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return nil, apperr.BadRequest("Không tải được label từ link đã nhập (hết hạn / chặn / chậm quá " +
			fmt.Sprint(int(externalLabelTimeout.Seconds())) + "s). Mở link gốc để kiểm tra.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.BadRequest(fmt.Sprintf("Link label trả về lỗi %d — link có thể đã hết hạn", resp.StatusCode))
	}
	if resp.ContentLength > maxExternalLabelBytes {
		return nil, apperr.BadRequest("File label quá lớn (> 15 MB)")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxExternalLabelBytes+1))
	if err != nil {
		return nil, apperr.BadRequest("Tải label bị ngắt giữa chừng, thử lại")
	}
	if int64(len(data)) > maxExternalLabelBytes {
		return nil, apperr.BadRequest("File label quá lớn (> 15 MB)")
	}

	ctype, ext := sniffLabel(data)
	if ctype == "" {
		return nil, apperr.BadRequest("Link không trả về file label (PDF / ảnh) — kiểm tra lại link")
	}
	return &LabelFile{Data: data, ContentType: ctype, Filename: "label-" + order.InternalCode + "." + ext}, nil
}

// sniffLabel recognises what the carrier sent by its first bytes (the server's
// Content-Type is often octet-stream): PDF, PNG, JPEG or GIF; "" for anything
// else (an HTML login page, a JSON error…).
func sniffLabel(data []byte) (ctype, ext string) {
	if len(data) >= 5 && string(data[:5]) == "%PDF-" {
		return "application/pdf", "pdf"
	}
	switch t := http.DetectContentType(data); t {
	case "image/png":
		return t, "png"
	case "image/jpeg":
		return t, "jpg"
	case "image/gif":
		return t, "gif"
	}
	return "", ""
}
