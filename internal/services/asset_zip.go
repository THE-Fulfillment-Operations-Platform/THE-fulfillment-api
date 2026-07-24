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

// padSeq zero-pads the STT to at least 3 digits so files sort naturally
// (001, 002, ... 010). A missing/zero sequence renders as 000.
func padSeq(seq int) string {
	if seq < 0 {
		seq = 0
	}
	return fmt.Sprintf("%03d", seq)
}

// designSeq hands out the STT that prefixes design file names: it counts position
// WITHIN THIS ARCHIVE — 001 for the first item, 002 for the next — so the numbers
// run 1,2,3,… with no gaps and the designer can count files off against the list
// they exported. (It deliberately does not use the order's DailySeq, which skips
// around because it numbers the whole day's orders, not this download.)
//
// A number is only consumed once the item actually lands a file in the ZIP, so an
// item whose every URL is broken doesn't burn an STT and leave a hole.
type designSeq struct{ n int }

// next is the number the current item would take; commit claims it.
func (d *designSeq) next() int { return d.n + 1 }
func (d *designSeq) commit()   { d.n++ }

// sanitizeSKUForFile strips any character that could break a file name, keeping
// only ASCII letters, digits, dash and underscore. Everything else becomes a
// dash. Guarantees a non-empty token.
func sanitizeSKUForFile(sku string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(sku) {
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

// designFileName builds the "STT_SKU_QUANTITY[_SIDE].EXT" name required for design
// downloads (e.g. 001_WDHWB-10IN_2_FRONT.pdf), where STT is the file's position in
// this archive (see designSeq) and QUANTITY is the item's ordered quantity. Both
// sides of one item share an STT — it numbers items, not files. SINGLE-side files
// carry no side suffix. usedNames guards against overwriting when two files would
// otherwise collide, appending -2, -3, … The optional folder prefixes the name.
func designFileName(folder string, seq int, sku string, qty int, side models.DesignSide, rawURL string, usedNames map[string]int) string {
	ext := extFromURL(rawURL)
	if qty < 1 {
		qty = 1
	}
	base := fmt.Sprintf("%s_%s_%d", padSeq(seq), sanitizeSKUForFile(sku), qty)
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
