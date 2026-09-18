package services

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
)

// designAsset is a single design file to place into a download ZIP, tagged with
// which physical side it represents.
type designAsset struct {
	url  string
	side models.DesignSide
}

// designAssetsForItem returns the original design file(s) for an item — and ONLY
// the design files, never the mockup or production print/cut files. A two-sided
// item (BackDesignURL present) yields a FRONT + BACK pair; a one-sided item yields
// a single SINGLE-side design. Empty URLs are skipped.
func designAssetsForItem(it *models.OrderItem) []designAsset {
	front := strings.TrimSpace(it.DesignURL)
	back := strings.TrimSpace(it.BackDesignURL)
	var out []designAsset
	if back != "" {
		if front != "" {
			out = append(out, designAsset{url: front, side: models.DesignSideFront})
		}
		out = append(out, designAsset{url: back, side: models.DesignSideBack})
		return out
	}
	if front != "" {
		out = append(out, designAsset{url: front, side: models.DesignSideSingle})
	}
	return out
}

// sanitizeFileToken strips any character that could break a file name (or split
// it across folders, e.g. the "/" in an internal code like 100001_1/3), keeping
// only ASCII letters, digits, dash and underscore. Everything else becomes a
// dash. Guarantees a non-empty token. Used for both the SKU and the internal code
// that make up a design file's name.
func sanitizeFileToken(token string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(token) {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "NA"
	}
	return out
}

// designFileBase builds the extension-LESS "SKU_INTERNALCODE_QUANTITY[_SIDE]"
// name required for design downloads (e.g. WDHWB-10IN_100001_1-3_2_FRONT), where
// INTERNALCODE is the item's internal (QR) code and QUANTITY is the item's ordered
// quantity — so a designer opening a file sees exactly which order/SKU it belongs
// to. The SKU leads the name on purpose: a file explorer sorts the extracted folder
// by name, so every file of the same SKU lands together instead of being scattered
// across orders. Both sides of one item share the same prefix; SINGLE-side files
// carry no side suffix. The optional folder prefixes the name.
//
// The extension is deliberately NOT part of this: it is only known once the asset
// has been fetched and its real type resolved (see resolveAssetExt), so the caller
// appends it with reserveZipName.
func designFileBase(folder, internalCode, sku string, qty int, side models.DesignSide) string {
	if qty < 1 {
		qty = 1
	}
	base := fmt.Sprintf("%s_%s_%d", sanitizeFileToken(sku), sanitizeFileToken(internalCode), qty)
	switch side {
	case models.DesignSideFront:
		base += "_FRONT"
	case models.DesignSideBack:
		base += "_BACK"
	}
	if folder != "" {
		base = folder + "/" + base
	}
	return base
}

// reserveZipName joins an extension-less entry base with its resolved extension
// and guards against overwriting when two files would otherwise collide,
// appending -2, -3, … Names are reserved only for assets that actually
// downloaded, so a broken URL no longer burns a suffix.
func reserveZipName(usedNames map[string]int, base, ext string) string {
	name := base + ext
	if count, ok := usedNames[name]; ok {
		count++
		usedNames[name] = count
		return fmt.Sprintf("%s-%d%s", base, count, ext)
	}
	usedNames[name] = 1
	return name
}

// assetFailure is one design file that could not be downloaded. It is a line of
// the ZIP's error note when other files did come through, and a row of the web
// app's error dialog when none did — hence the JSON tags.
type assetFailure struct {
	OrderID      uint   `json:"order_id"` // where the link is fixed (order detail page)
	InternalCode string `json:"internal_code"`
	SKU          string `json:"sku"`
	Side         string `json:"side,omitempty"` // FRONT / BACK; empty for a one-sided item
	Code         string `json:"code"`           // see the assetReason* constants
	Reason       string `json:"reason"`
	URL          string `json:"url"`
}

// codeDesignDownloadFailed marks a design download where not one file came
// through; its details (designDownloadFailure) list every failed link.
const codeDesignDownloadFailed = "DESIGN_DOWNLOAD_FAILED"

type designDownloadFailure struct {
	Failed []assetFailure `json:"failed"`
}

// assetFailReason turns a download error into the code + sentence a person gets:
// the user-facing message only, never the wrapped internals. A write to the ZIP
// itself failing is not the link's fault, and says so generically.
func assetFailReason(err error) (code, message string) {
	var ae *apperr.Error
	if errors.As(err, &ae) && ae.Message != "" && ae.Code != "INTERNAL" {
		return ae.Code, ae.Message
	}
	return assetReasonDownloadFailed, "Tải file bị lỗi giữa chừng — thử tải lại"
}

// describeAssetFailure records which product, which side, why it failed and the
// link to fix — everything needed to repair it without hunting through the app.
func describeAssetFailure(it *models.OrderItem, a designAsset, err error) assetFailure {
	f := assetFailure{OrderID: it.OrderID, InternalCode: it.InternalCode, SKU: it.SKUCode, URL: a.url}
	if a.side == models.DesignSideFront || a.side == models.DesignSideBack {
		f.Side = string(a.side)
	}
	f.Code, f.Reason = assetFailReason(err)
	return f
}

func (f assetFailure) noteLine() string {
	side := ""
	switch f.Side {
	case string(models.DesignSideFront):
		side = " (mặt trước)"
	case string(models.DesignSideBack):
		side = " (mặt sau)"
	}
	return fmt.Sprintf("- %s · %s%s: %s\n  link: %s", f.InternalCode, f.SKU, side, f.Reason, f.URL)
}

// noDesignFilesError is the answer to a download that produced nothing. The
// message is one sentence for a toast; the details carry every failed link for a
// UI that can show them. When every link failed for the same reason, the sentence
// names it — that reason is usually the whole fix.
func noDesignFilesError(failed []assetFailure) error {
	base := "Không tải được file design nào"
	if len(failed) == 0 {
		return apperr.Unprocessable(base + " — các sản phẩm chưa có link design")
	}
	msg := fmt.Sprintf("%s — %d link lỗi", base, len(failed))
	if sameReason(failed) {
		msg += ": " + failed[0].Reason
	}
	return apperr.New(http.StatusUnprocessableEntity, codeDesignDownloadFailed, msg).
		WithDetails(designDownloadFailure{Failed: failed})
}

func sameReason(failed []assetFailure) bool {
	for _, f := range failed[1:] {
		if f.Reason != failed[0].Reason {
			return false
		}
	}
	return true
}

// zipErrorNoteName is the note listing the design files that could not be
// downloaded. It leads with "_" so a file explorer sorts it to the top of the
// extracted folder — a designer must see it before starting work, otherwise a
// silently missing file looks like an item that simply was not in the batch.
const zipErrorNoteName = "_FILE-LOI.txt"

// writeZipErrorNote adds the note to the archive. Called only when at least one
// file DID download: with nothing to deliver the caller returns a real error
// instead, so the user gets a message rather than a ZIP holding just a complaint.
func writeZipErrorNote(zw *zip.Writer, folder string, failed []assetFailure) error {
	if len(failed) == 0 {
		return nil
	}
	name := zipErrorNoteName
	if folder != "" {
		name = folder + "/" + name
	}
	w, err := zw.Create(name)
	if err != nil {
		return apperr.Internal("could not write ZIP error note").Wrap(err)
	}
	lines := make([]string, len(failed))
	for i, f := range failed {
		lines[i] = f.noteLine()
	}
	body := fmt.Sprintf("KHÔNG TẢI ĐƯỢC %d FILE DESIGN\n\n%s\n\n"+
		"Cách xử lý: mở đúng sản phẩm trong màn \"Chờ thiết kế\" và sửa lại link design.\n"+
		"Link phải trỏ tới ĐÚNG MỘT FILE (không phải thư mục) và được chia sẻ ở chế độ\n"+
		"\"Bất kỳ ai có đường liên kết\" thì máy chủ mới tải được.\n",
		len(failed), strings.Join(lines, "\n"))
	if _, err := io.WriteString(w, body); err != nil {
		return apperr.Internal("could not write ZIP error note").Wrap(err)
	}
	return nil
}

// sortDesignItemsBySKU orders the items of a design ZIP the same way a file
// explorer will order the extracted folder — by the name tokens designFileBase
// builds, SKU first then internal code. Without it the ZIP's entry order is the
// query order (newest item first), so a designer browsing the archive before
// extracting sees the files scattered across SKUs.
func sortDesignItemsBySKU(items []*models.OrderItem) {
	sort.SliceStable(items, func(i, j int) bool {
		si, sj := sanitizeFileToken(items[i].SKUCode), sanitizeFileToken(items[j].SKUCode)
		if si != sj {
			return si < sj
		}
		return sanitizeFileToken(items[i].InternalCode) < sanitizeFileToken(items[j].InternalCode)
	})
}
