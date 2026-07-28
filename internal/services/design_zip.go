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
// the download day. The folder keeps one pull's files together and separate from
// another's, so re-pulling the same item on a different day doesn't overwrite the
// earlier copy. now is passed in so the handler's ZIP filename and the folder
// inside always agree on the same instant.
func DesignAssetsFolder(batch string, now time.Time) string {
	if b := sanitizeZipComponent(batch); b != "" {
		return "Batch_" + b
	}
	return "Design_" + now.Format("2006-01-02")
}

// DesignZipQuery selects which design-queue items a ZIP export covers. When IDs is
// non-empty the export is exactly those items (an explicit tick selection); a nil/
// empty slice covers the whole approved design queue, narrowed server-side by the
// same filters the pick-list uses — Batch, MaterialID and a Search over internal_code
// /sku_code — so "download everything matching this filter" never depends on which
// page the client had loaded.
type DesignZipQuery struct {
	IDs        []uint
	Batch      string
	Search     string
	MaterialID *uint
}

// StreamDesignAssetsZip streams ONLY the original design files (front + back) of
// design-queue items into a ZIP — never the mockup and never the production
// print/cut files. Every file goes into a single folder (see DesignAssetsFolder)
// and is named "INTERNALCODE_SKU_QUANTITY[_SIDE].EXT" (see designFileName). See
// DesignZipQuery for how the item set is chosen. A single file that fails to
// download is skipped (not fatal) so one broken URL doesn't abandon the rest of
// the archive. It fails only when no design file could be written.
func (s *OrderService) StreamDesignAssetsZip(ctx context.Context, w io.Writer, q DesignZipQuery, folder string) error {
	f := repositories.ItemFilter{ReviewApproved: true, IDs: q.IDs}
	// With no explicit selection, cover the design queue (items still needing design
	// that already have a file), narrowed by the caller's batch/NVL/search filters.
	// With an explicit selection, download exactly those items' designs regardless of
	// design status (a designer may re-pull a finished item) — the filters here only
	// name the folder / are ignored, they do not further narrow the ticked rows.
	if len(q.IDs) == 0 {
		f.NeedsDesign = true
		f.HasDesignFile = true
		f.BatchCode = q.Batch
		f.Search = q.Search
		f.MaterialID = q.MaterialID
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

	for i := range items {
		it := &items[i]
		for _, a := range designAssetsForItem(it) {
			entryName := designFileName(folder, it.InternalCode, it.SKUCode, it.Quantity, a.side, a.url, usedNames)
			if err := writeURLToZipEntry(ctx, client, zw, a.url, entryName); err != nil {
				// Skip a single broken/blocked asset rather than aborting the whole ZIP.
				continue
			}
			written++
		}
	}

	if written == 0 {
		return apperr.Unprocessable("Không có file design nào để tải (kiểm tra link design của các đơn đã chọn)")
	}
	return zw.Close()
}
