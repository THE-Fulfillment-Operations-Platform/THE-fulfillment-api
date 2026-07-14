package services

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/repositories"
)

// StreamDesignAssetsZip streams the design + mockup files of design-queue items
// into a ZIP, one folder per internal code (e.g. 100003_1/design.png,
// 100003_1/mockup.jpg). ids restricts the export to the given item ids; a nil or
// empty slice covers the whole approved design queue. Only design + mockup are
// included — designers don't need the print/cut production files here.
func (s *OrderService) StreamDesignAssetsZip(ctx context.Context, w io.Writer, ids []uint) error {
	items, err := s.repo.OrderItem.ListAll(repositories.ItemFilter{
		NeedsDesign:    true,
		ReviewApproved: true,
		IDs:            ids,
	})
	if err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	// SSRF-safe client: refuses to connect to non-public IPs (see safeurl.go).
	// Asset URLs come from seller import data, so a plain client would be an SSRF sink.
	client := newSafeAssetClient(30 * time.Second)
	written := 0
	usedNames := map[string]int{}

	for i := range items {
		it := &items[i]
		assets := []struct {
			url   string
			type_ string
		}{
			{it.DesignURL, "design"},
			{it.MockupURL, "mockup"},
		}
		code := sanitizeZipComponent(it.InternalCode)
		if code == "" {
			code = fmt.Sprintf("item-%d", it.ID)
		}

		for _, asset := range assets {
			if strings.TrimSpace(asset.url) == "" {
				continue
			}
			entryName := zipFolderEntryName(code, asset.type_, asset.url, usedNames)
			if err := writeURLToZipEntry(ctx, client, zw, asset.url, entryName); err != nil {
				return err
			}
			written++
		}
	}

	if written == 0 {
		return apperr.Unprocessable("Không có file design/mockup nào để tải")
	}

	return zw.Close()
}

// zipFolderEntryName builds a foldered entry path "<code>/<type><ext>" so each
// internal code becomes its own directory when the archive is extracted. Unlike
// the flat batch naming, this groups a designer's files per order. Collisions on
// the same code+type get a numeric suffix inside the same folder.
func zipFolderEntryName(code, assetType, rawURL string, usedNames map[string]int) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		u = &url.URL{Path: rawURL}
	}
	ext := path.Ext(u.Path)
	if ext == "" {
		ext = ".bin"
	}
	name := fmt.Sprintf("%s/%s%s", code, assetType, ext)
	if count, ok := usedNames[name]; ok {
		count++
		usedNames[name] = count
		name = fmt.Sprintf("%s/%s-%d%s", code, assetType, count, ext)
	} else {
		usedNames[name] = 1
	}
	return name
}
