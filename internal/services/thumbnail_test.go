package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

// newThumbService builds a cache backed by a temp dir and an in-memory DB.
func newThumbService(t *testing.T) (*ThumbService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Order{}, &models.OrderItem{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewThumbService(repositories.New(db), ThumbOptions{
		Dir:    t.TempDir(),
		MaxPx:  900,
		URLTTL: time.Hour,
		Secret: []byte("test-secret"),
	})
	return svc, db
}

// pngOf renders a test image of the given size. The pixels are deliberately
// noisy rather than a smooth gradient: a real product mockup is a render or a
// photograph, which PNG cannot compress away, and a flat test pattern would
// make the compression numbers here meaningless.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			n := uint32(x)*2654435761 ^ uint32(y)*2246822519
			img.Set(x, y, color.RGBA{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// parseQuery pulls the signed parameters back out of a URL SignedURL produced.
func parseQuery(t *testing.T, signed string) (name, item, exp, sig string) {
	t.Helper()
	path, query, found := strings.Cut(signed, "?")
	if !found {
		t.Fatalf("signed URL has no query: %q", signed)
	}
	name = path[strings.LastIndex(path, "/")+1:]
	for _, pair := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(pair, "=")
		switch k {
		case "i":
			item = v
		case "e":
			exp = v
		case "s":
			sig = v
		}
	}
	return name, item, exp, sig
}

// A thumbnail URL is a bearer credential: it is the only thing standing between
// the public internet and this endpoint, because an <img> tag cannot carry the
// API's bearer token. So it has to survive exactly one round trip and reject
// every alteration of it.
func TestThumbSignedURL_RoundTripsAndRejectsTampering(t *testing.T) {
	svc, _ := newThumbService(t)

	signed := svc.SignedURL(42, "https://cdn.example.com/mockup.png")
	if signed == "" {
		t.Fatal("expected a signed URL for an item with a mockup")
	}
	name, item, exp, sig := parseQuery(t, signed)

	hash, itemID, err := svc.ParseRequest(name, item, exp, sig)
	if err != nil {
		t.Fatalf("freshly signed URL must verify: %v", err)
	}
	if itemID != 42 {
		t.Fatalf("item id = %d, want 42", itemID)
	}
	if hash != sourceHash("https://cdn.example.com/mockup.png") {
		t.Fatalf("hash %q does not identify the source URL", hash)
	}

	// Re-pointing a valid signature at another item is the attack that matters:
	// it would turn one leaked URL into a reader for every mockup in the system.
	if _, _, err := svc.ParseRequest(name, "43", exp, sig); err == nil {
		t.Error("a signature must not verify for a different item")
	}
	if _, _, err := svc.ParseRequest(name, item, exp, strings.Repeat("0", 32)); err == nil {
		t.Error("a forged signature must not verify")
	}
	other := sourceHash("https://cdn.example.com/other.png")
	if _, _, err := svc.ParseRequest(other+".jpg", item, exp, sig); err == nil {
		t.Error("a signature must not verify for a different source")
	}
	// Path traversal dressed up as a hash must never reach the filesystem.
	if _, _, err := svc.ParseRequest("../../etc/passwd", item, exp, sig); err == nil {
		t.Error("a non-hash name must be rejected")
	}
}

func TestThumbSignedURL_ExpiredIsRejected(t *testing.T) {
	svc, _ := newThumbService(t)
	svc.opts.URLTTL = -time.Minute // already expired when minted

	signed := svc.SignedURL(7, "https://cdn.example.com/m.png")
	name, item, exp, sig := parseQuery(t, signed)
	if _, _, err := svc.ParseRequest(name, item, exp, sig); err == nil {
		t.Error("an expired URL must not verify even with a valid signature")
	}
}

// The cache is an optimisation, so every "off" path has to degrade to an empty
// URL — that is the signal the station uses to fall back to the origin.
func TestThumbSignedURL_EmptyWhenNothingToServe(t *testing.T) {
	svc, _ := newThumbService(t)

	if got := svc.SignedURL(1, "   "); got != "" {
		t.Errorf("blank mockup should yield no URL, got %q", got)
	}
	if got := svc.SignedURL(0, "https://cdn.example.com/m.png"); got != "" {
		t.Errorf("item 0 should yield no URL, got %q", got)
	}

	off := NewThumbService(nil, ThumbOptions{Dir: ""})
	if off.Enabled() {
		t.Error("a cache with no directory must report itself disabled")
	}
	if got := off.SignedURL(1, "https://cdn.example.com/m.png"); got != "" {
		t.Errorf("disabled cache should yield no URL, got %q", got)
	}

	// A nil service is a supported state (tests and any wiring that skips the
	// cache construct QCService directly), so it must not panic.
	var nilSvc *ThumbService
	if nilSvc.Enabled() {
		t.Error("nil cache must report itself disabled")
	}
	if got := nilSvc.SignedURL(1, "https://cdn.example.com/m.png"); got != "" {
		t.Errorf("nil cache should yield no URL, got %q", got)
	}
}

// Items of the same SKU share one mockup; hashing the URL (not the item) is what
// makes them share one cached file instead of one file each.
func TestSourceHash_IdentifiesTheURLNotTheItem(t *testing.T) {
	a := sourceHash("https://cdn.example.com/m.png")
	b := sourceHash("  https://cdn.example.com/m.png  ")
	if a != b {
		t.Error("surrounding whitespace must not change a mockup's identity")
	}
	if a == sourceHash("https://cdn.example.com/other.png") {
		t.Error("different mockups must not collide")
	}
	if len(a) != 32 {
		t.Fatalf("hash length = %d, want 32", len(a))
	}
}

func TestShrinkToJPEG_DownscalesToTheLongEdge(t *testing.T) {
	source := pngOf(t, 1200, 600)
	out, err := shrinkToJPEG(source, 900)
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %s, want jpeg", format)
	}
	if cfg.Width != 900 || cfg.Height != 450 {
		t.Errorf("size = %dx%d, want 900x450 (aspect ratio preserved)", cfg.Width, cfg.Height)
	}
	// The whole point is bytes on the wire: a shrunk mockup must be far smaller
	// than the original the browser used to download.
	if len(out)*4 >= len(source) {
		t.Errorf("thumbnail %d bytes vs source %d — not the order-of-magnitude saving the cache exists for",
			len(out), len(source))
	}
}

func TestShrinkToJPEG_NeverUpscales(t *testing.T) {
	out, err := shrinkToJPEG(pngOf(t, 120, 80), 900)
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if cfg.Width != 120 || cfg.Height != 80 {
		t.Errorf("size = %dx%d, want the original 120x80", cfg.Width, cfg.Height)
	}
}

func TestShrinkToJPEG_RejectsWhatIsNotAnImage(t *testing.T) {
	// Drive answers an unshared link with an HTML login page, status 200.
	if _, err := shrinkToJPEG([]byte("<html><body>Request access</body></html>"), 900); err == nil {
		t.Error("HTML must not be accepted as a mockup")
	}
	if _, err := shrinkToJPEG(nil, 900); err == nil {
		t.Error("empty input must not be accepted")
	}
}

// bombPNG builds a header-only PNG that CLAIMS an enormous canvas. Decoding one
// for real would allocate gigabytes, which is exactly why the size is checked
// from the header before any pixels are touched.
func bombPNG(w, h uint32) []byte {
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	binary.Write(&ihdr, binary.BigEndian, w)
	binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // 8-bit truecolour, no interlace

	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	binary.Write(&out, binary.BigEndian, uint32(13))
	out.Write(ihdr.Bytes())
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))
	return out.Bytes()
}

func TestShrinkToJPEG_RejectsDecompressionBomb(t *testing.T) {
	// A few dozen bytes on the wire, ~7 GB once decoded.
	if _, err := shrinkToJPEG(bombPNG(50000, 50000), 900); err == nil {
		t.Error("an absurdly large canvas must be refused before decoding")
	}
}

// Drive's direct-download endpoint hands back the seller's ORIGINAL file, which
// for a print mockup is routinely tens of megabytes — all but 900px of which we
// immediately discard. Asking Drive for its own thumbnail first is what keeps
// the first scan of a tray from waiting on that download.
func TestThumbSourceCandidates_PrefersDriveThumbnail(t *testing.T) {
	got := thumbSourceCandidates("https://drive.google.com/file/d/ABC123xyz/view?usp=sharing")
	if len(got) != 2 {
		t.Fatalf("want a preferred URL and a fallback, got %v", got)
	}
	if !strings.Contains(got[0], "drive.google.com/thumbnail") || !strings.Contains(got[0], "ABC123xyz") {
		t.Errorf("first candidate should be Drive's thumbnail endpoint, got %q", got[0])
	}
	if !strings.Contains(got[1], "drive.usercontent.google.com/download") {
		t.Errorf("fallback should be the direct download, got %q", got[1])
	}

	// A plain CDN link has nothing to optimise and must be left alone.
	plain := thumbSourceCandidates("https://cdn.example.com/m.png")
	if len(plain) != 1 || plain[0] != "https://cdn.example.com/m.png" {
		t.Errorf("non-Drive URL should pass through unchanged, got %v", plain)
	}
}

// The hot path must not touch the database or the network: the file name is
// derivable from the request alone. This test proves it by pointing the item at
// a URL that could never be fetched — if the cache tried, it would fail.
func TestThumbMaterialize_ServesCachedFileWithoutFetching(t *testing.T) {
	svc, db := newThumbService(t)
	const mockup = "https://cdn.invalid.example/unreachable.png"
	if err := db.Create(&models.OrderItem{
		OrderID: 1, InternalCode: "IT-1", SKUCode: "SKU-1", Quantity: 1, MockupURL: mockup,
	}).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}

	hash := sourceHash(mockup)
	path := svc.path(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("cached-bytes"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	got, _, err := svc.Materialize(context.Background(), 1, hash)
	if err != nil {
		t.Fatalf("a cached thumbnail must be served, got error: %v", err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
}

// Editing a mockup changes its hash, so the old URL must stop resolving rather
// than keep showing QC the picture the seller replaced.
func TestThumbMaterialize_RefusesHashThatNoLongerMatchesTheMockup(t *testing.T) {
	svc, db := newThumbService(t)
	if err := db.Create(&models.OrderItem{
		OrderID: 1, InternalCode: "IT-1", SKUCode: "SKU-1", Quantity: 1,
		MockupURL: "https://cdn.example.com/new.png",
	}).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}

	stale := sourceHash("https://cdn.example.com/old.png")
	path := svc.path(stale)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if _, _, err := svc.Materialize(context.Background(), 1, stale); err == nil {
		t.Error("a hash that no longer matches the item's mockup must not be served")
	}
}

func TestThumbMaterialize_ItemWithoutMockupIsNotAnError(t *testing.T) {
	svc, db := newThumbService(t)
	if err := db.Create(&models.OrderItem{
		OrderID: 1, InternalCode: "IT-1", SKUCode: "SKU-1", Quantity: 1,
	}).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, _, err := svc.Materialize(context.Background(), 1, sourceHash("x")); err == nil {
		t.Error("an item with no mockup has no thumbnail to serve")
	}
}

// A mockup we cannot render (a PSD, a link that was never shared) must be
// remembered as hopeless. Without this, every scan of that item re-downloads and
// re-fails — making the one case we cannot speed up the slowest one of all.
func TestThumbFailures_AreRememberedThenForgotten(t *testing.T) {
	svc, _ := newThumbService(t)
	hash := sourceHash("https://cdn.example.com/broken.psd")

	if svc.recentlyFailed(hash) {
		t.Fatal("nothing has failed yet")
	}
	svc.failed.Store(hash, time.Now())
	if !svc.recentlyFailed(hash) {
		t.Error("a fresh failure must be remembered")
	}
	svc.failed.Store(hash, time.Now().Add(-thumbFailureTTL-time.Minute))
	if svc.recentlyFailed(hash) {
		t.Error("an old failure must expire so a fixed link recovers on its own")
	}
}

func TestThumbSweep_RemovesUnusedButKeepsRecent(t *testing.T) {
	svc, _ := newThumbService(t)

	fresh := svc.path(sourceHash("fresh"))
	stale := svc.path(sourceHash("stale"))
	for _, p := range []string{fresh, stale} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	old := time.Now().Add(-thumbSweepTTL - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	svc.Sweep()

	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recently used thumbnail must survive the sweep")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a thumbnail nobody has looked at in a month must be evicted")
	}
}

// Warming is fire-and-forget, so the contract worth pinning is that it never
// blocks and never panics on the inputs it will really see.
func TestThumbWarm_IsSafeOnEmptyAndOversizedInput(t *testing.T) {
	svc, _ := newThumbService(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Warm(nil)
		svc.Warm([]repositories.ItemMockup{{ItemID: 0, MockupURL: ""}})
		// Well past the per-tray cap; must be trimmed, not queued wholesale.
		big := make([]repositories.ItemMockup, 0, thumbWarmCap*2)
		for i := 0; i < thumbWarmCap*2; i++ {
			big = append(big, repositories.ItemMockup{
				ItemID: uint(i + 1), MockupURL: "https://cdn.invalid.example/" + strconv.Itoa(i) + ".png",
			})
		}
		svc.Warm(big)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Warm must return immediately; the operator's scan does not wait on it")
	}
}

// The scan payload is where the station learns the thumbnail exists at all, so
// the wiring from QCService through to a verifiable URL is worth pinning end to
// end — and so is the fact that turning the cache off changes nothing else.
func TestQCScan_CarriesAVerifiableThumbnailURL(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)
	thumb := NewThumbService(repo, ThumbOptions{
		Dir: t.TempDir(), MaxPx: 900, URLTTL: time.Hour, Secret: []byte("test-secret"),
	})
	qc := &QCService{repo: repo, audit: &AuditService{repo: repo}, thumb: thumb}

	const mockup = "https://cdn.example.com/mockup.png"
	order := &models.Order{
		InternalCode: "ORD-000001", StoreOrderID: "US-1", StoreOrderRef: "US-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved, SellerStatus: models.SellerStatusProduction,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	item := &models.OrderItem{
		OrderID: order.ID, InternalCode: "IT-000001", SKUCode: "SKU-1", Quantity: 1,
		MockupURL: mockup, InternalStatus: models.StatusCut,
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}

	res, err := qc.Scan(Actor{ID: 1}, ScanRef{Code: "IT-000001"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.MockupURL != mockup {
		t.Errorf("the original mockup URL must survive for the open-in-new-tab link, got %q", res.MockupURL)
	}
	if res.MockupThumbURL == "" {
		t.Fatal("scan should carry a thumbnail URL when the cache is on")
	}
	name, itemParam, exp, sig := parseQuery(t, res.MockupThumbURL)
	gotHash, gotItem, err := thumb.ParseRequest(name, itemParam, exp, sig)
	if err != nil {
		t.Fatalf("the URL handed to the station must verify: %v", err)
	}
	if gotItem != item.ID || gotHash != sourceHash(mockup) {
		t.Errorf("URL resolves to item %d/%s, want %d/%s", gotItem, gotHash, item.ID, sourceHash(mockup))
	}

	// With the cache off, the field is simply absent and the station falls back.
	qcOff := &QCService{repo: repo, audit: &AuditService{repo: repo}}
	offRes, err := qcOff.Scan(Actor{ID: 1}, ScanRef{Code: "IT-000001"})
	if err != nil {
		t.Fatalf("scan with cache off: %v", err)
	}
	if offRes.MockupThumbURL != "" {
		t.Errorf("no cache should mean no thumbnail URL, got %q", offRes.MockupThumbURL)
	}
	if offRes.MockupURL != mockup {
		t.Error("the station must still receive the original mockup URL")
	}
}

// Warming is driven by "the rest of the tray", so the query behind it has to
// mean exactly that: everything produced in the same batch, minus the pieces
// that were scrapped and will never reach QC.
func TestMockupsForBatches_ListsTheTrayWithoutScrappedParts(t *testing.T) {
	db := newQCDB(t)
	repo := repositories.New(db)

	order := &models.Order{
		InternalCode: "ORD-000001", StoreOrderID: "US-1", StoreOrderRef: "US-1", SellerID: 1,
		ReviewStatus: models.ReviewApproved,
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	batch := &models.Batch{Code: "B-1", MaterialID: 1, Status: models.StatusCut}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("create batch: %v", err)
	}

	mk := func(code, mockup string) *models.OrderItem {
		it := &models.OrderItem{
			OrderID: order.ID, InternalCode: code, SKUCode: "SKU-1", Quantity: 1, MockupURL: mockup,
		}
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("create item %s: %v", code, err)
		}
		return it
	}
	wanted := mk("IT-1", "https://cdn.example.com/a.png")
	scrapped := mk("IT-2", "https://cdn.example.com/b.png")
	noMockup := mk("IT-3", "")
	elsewhere := mk("IT-4", "https://cdn.example.com/d.png")

	scrapAt := time.Now()
	rows := []models.BatchItem{
		{BatchID: batch.ID, OrderItemID: wanted.ID, MaterialID: 1, Status: models.StatusCut, Attempt: 1},
		{BatchID: batch.ID, OrderItemID: scrapped.ID, MaterialID: 1, Status: models.StatusCut, Attempt: 1, ScrappedAt: &scrapAt},
		{BatchID: batch.ID, OrderItemID: noMockup.ID, MaterialID: 1, Status: models.StatusCut, Attempt: 1},
		{BatchID: batch.ID + 99, OrderItemID: elsewhere.ID, MaterialID: 1, Status: models.StatusCut, Attempt: 1},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create batch item %d: %v", i, err)
		}
	}

	got, err := repo.OrderItem.MockupsForBatches([]uint{batch.ID})
	if err != nil {
		t.Fatalf("MockupsForBatches: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d mockups (%v), want just the one live item with a mockup", len(got), got)
	}
	if got[0].ItemID != wanted.ID || got[0].MockupURL != "https://cdn.example.com/a.png" {
		t.Errorf("got %+v, want item %d", got[0], wanted.ID)
	}

	if rows, err := repo.OrderItem.MockupsForBatches(nil); err != nil || rows != nil {
		t.Errorf("no batches should mean no query and no rows, got %v / %v", rows, err)
	}
}
