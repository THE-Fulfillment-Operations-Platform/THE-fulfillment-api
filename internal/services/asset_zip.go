package services

import (
	"fmt"
	"net/url"
	"path"
	"strings"

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

// extFromURL extracts a file extension from a URL path, defaulting to .bin when
// the URL has none (e.g. a Google-Drive share link).
func extFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		u = &url.URL{Path: rawURL}
	}
	ext := path.Ext(u.Path)
	if ext == "" {
		return ".bin"
	}
	return ext
}

// designFileName builds the "INTERNALCODE_SKU_QUANTITY[_SIDE].EXT" name required
// for design downloads (e.g. 100001_1-3_WDHWB-10IN_2_FRONT.pdf), where INTERNALCODE
// is the item's internal (QR) code and QUANTITY is the item's ordered quantity —
// so a designer opening a file sees exactly which order/SKU it belongs to. Both
// sides of one item share the same internal-code prefix; SINGLE-side files carry
// no side suffix. usedNames guards against overwriting when two files would
// otherwise collide, appending -2, -3, … The optional folder prefixes the name.
func designFileName(folder, internalCode, sku string, qty int, side models.DesignSide, rawURL string, usedNames map[string]int) string {
	ext := extFromURL(rawURL)
	if qty < 1 {
		qty = 1
	}
	base := fmt.Sprintf("%s_%s_%d", sanitizeFileToken(internalCode), sanitizeFileToken(sku), qty)
	switch side {
	case models.DesignSideFront:
		base += "_FRONT"
	case models.DesignSideBack:
		base += "_BACK"
	}
	name := base + ext
	if folder != "" {
		name = folder + "/" + name
	}
	if count, ok := usedNames[name]; ok {
		count++
		usedNames[name] = count
		name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), count, ext)
	} else {
		usedNames[name] = 1
	}
	return name
}
