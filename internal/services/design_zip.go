package services

import (
	"archive/zip"
	"context"
	"io"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/repositories"
)

// DesignZipResult reports which design files could not be downloaded so the
// caller can surface a partial-success message ("N file lỗi") instead of failing
// the whole archive.
type DesignZipResult struct {
	Written int      `json:"written"`
	Failed  []string `json:"failed"`
}

// DesignAssetsFolder returns the single in-ZIP folder that groups a design
// download so a designer can tell one pull from another. When a batch filter is
// active the folder is "Batch_<code>"; otherwise it is "Design_<YYYY-MM-DD>" of
// the download day. The folder is what keeps pulls apart: STT counts position
// within one archive, so EVERY download starts again at 001 and two pulls
// extracted into the same place would otherwise overwrite each other. now is
// passed in so the handler's ZIP filename and the folder inside always agree on
// the same instant.
func DesignAssetsFolder(batch string, now time.Time) string {
	if b := sanitizeZipComponent(batch); b != "" {
		return "Batch_" + b
	}
	return "Design_" + now.Format("2006-01-02")
}

// StreamDesignAssetsZip streams ONLY the original design files (front + back) of
// design-queue items into a ZIP — never the mockup and never the production
// print/cut files. Every file goes into a single folder (see DesignAssetsFolder)
// and is named "STT_SKU_QUANTITY[_SIDE].EXT" (see designFileName). ids restricts
// the export to the given item ids; a nil/empty slice covers the whole approved
// design queue (optionally narrowed to a batch). A single file that fails to
// download is skipped (not fatal) so one broken URL doesn't abandon the rest of
// the archive. It fails only when no design file could be written.
func (s *OrderService) StreamDesignAssetsZip(ctx context.Context, w io.Writer, ids []uint, batch, folder string) error {
	f := repositories.ItemFilter{ReviewApproved: true, IDs: ids}
	// With no explicit selection, cover the design queue (items still needing
	// design), optionally narrowed to the batch the designer is filtering on. With
	// an explicit selection, download exactly those items' designs regardless of
	// design status (a designer may re-pull a finished item) — the batch here only
	// names the folder, it does not further filter the ticked rows.
	if len(ids) == 0 {
		f.NeedsDesign = true
		if batch != "" {
			f.BatchCode = batch
		}
	}
	items, err := s.repo.OrderItem.ListAll(f)
	if err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	client := newSafeAssetClient(30 * time.Second)
	usedNames := map[string]int{}
	written := 0

	var seq designSeq
	for i := range items {
		it := &items[i]
		wroteForItem := false
		for _, a := range designAssetsForItem(it) {
			entryName := designFileName(folder, seq.next(), it.SKUCode, it.Quantity, a.side, a.url, usedNames)
			if err := writeURLToZipEntry(ctx, client, zw, a.url, entryName); err != nil {
				// Skip a single broken/blocked asset rather than aborting the whole ZIP.
				continue
			}
			written++
			wroteForItem = true
		}
		if wroteForItem {
			seq.commit()
		}
	}

	if written == 0 {
		return apperr.Unprocessable("Không có file design nào để tải (kiểm tra link design của các đơn đã chọn)")
	}
	return zw.Close()
}
