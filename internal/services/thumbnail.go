package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "image/gif"  // register GIF for image.Decode
	_ "image/jpeg" // register JPEG for image.Decode
	_ "image/png"  // register PNG for image.Decode

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP for image.Decode (decode-only)
	"golang.org/x/sync/singleflight"

	"the-fulfillment/backend/internal/repositories"
)

// The QC station is a loop: scan a tem, eyeball the seller's mockup against the
// physical product, pass, scan the next one. Everything in that loop is fast
// except the mockup, because the mockup was never ours to serve — sellers hand
// us a Google-Drive share link and the browser used to fetch the picture
// straight from Drive. That means two DNS+TLS handshakes, a redirect, and Drive
// rendering a thumbnail on demand from a file that may be tens of megabytes. A
// heavy mockup took seconds to appear, and the operator stood there waiting.
//
// So we stop asking Drive on the operator's clock. The first time a mockup is
// needed the API fetches it once, shrinks it to something a screen can actually
// use, and writes the result to disk; every later request is a local file read.
// A 30 MB source becomes a ~60 KB JPEG, and seconds become milliseconds.
//
// Two properties make this safe to put in front of the scan station:
//
//   - It never blocks QC. A fetch that fails, times out, or returns a format Go
//     cannot decode (PSD, HEIC, an HTML error page) is not an error the operator
//     should see — the handler falls back to the original URL and QC carries on
//     exactly as before.
//   - It never trusts the URL. Mockup links come from seller-supplied import
//     data, so fetches go through the same SSRF-guarded client the batch-assets
//     ZIP uses: public IPs only, re-resolved at dial time, redirects re-checked.

const (
	// thumbMaxSourceBytes caps what we will pull down to build one thumbnail.
	// The ZIP path allows 100 MB because it hands the file to a designer intact;
	// here the bytes only exist to be decoded and thrown away, so a much tighter
	// cap keeps a hostile or accidental giant off the heap.
	thumbMaxSourceBytes int64 = 40 << 20 // 40 MB

	// thumbMaxSourcePixels rejects decompression bombs: a few-KB PNG can declare
	// 50000x50000 and cost ~10 GB once decoded. Checked from the header before
	// any pixel is allocated. 80 MP is far above any real product mockup.
	thumbMaxSourcePixels = 80 << 20

	// thumbFetchTimeout bounds one upstream fetch. Drive is usually well under
	// this; the cap exists so a stalled host cannot pin a warm worker for long.
	thumbFetchTimeout = 25 * time.Second

	// thumbJPEGQuality trades a little fidelity for a lot of bytes. QC compares
	// shape, text and colour by eye, not print fidelity.
	thumbJPEGQuality = 82

	// thumbWarmConcurrency limits how many mockups the background warmer pulls at
	// once, so warming a tray never competes with the scan the operator is
	// waiting on (or trips Drive's rate limiting).
	thumbWarmConcurrency = 3

	// thumbWarmCap bounds how many items one warm request may touch. A tray is
	// tens of items; anything far beyond that is a runaway caller, not a tray.
	thumbWarmCap = 80

	// thumbSweepTTL is how long an untouched thumbnail survives on disk. Hits
	// refresh the file's mtime, so this evicts mockups nobody looks at anymore
	// rather than mockups that are merely old.
	thumbSweepTTL = 30 * 24 * time.Hour

	// thumbFailureTTL is how long a source that could not be thumbnailed is
	// remembered as hopeless. Without this, an item whose mockup is a PSD (or a
	// link that was never shared publicly) would re-download and re-fail on
	// EVERY scan — turning the one case we cannot speed up into the slowest
	// possible one. Short enough that fixing the link in Drive takes effect
	// within a coffee break.
	thumbFailureTTL = 10 * time.Minute
)

// errThumbUnusable marks a source we cannot turn into a thumbnail — wrong
// format, too big, not an image. The caller's answer is always the same (fall
// back to the original URL), so the reason only ever reaches the log.
var errThumbUnusable = errors.New("mockup cannot be thumbnailed")

// ThumbOptions configures the mockup thumbnail cache. An empty Dir disables the
// whole feature: SignedURL then returns "", the QC payload carries no thumbnail
// URL, and the frontend keeps loading mockups straight from their origin.
type ThumbOptions struct {
	Dir string
	// MaxPx is the longest edge of the produced thumbnail.
	MaxPx int
	// URLTTL is how long a signed thumbnail URL stays valid. It only needs to
	// outlive the screen it was rendered into.
	URLTTL time.Duration
	// Secret signs thumbnail URLs. Derived from the JWT secret by the caller so
	// there is no second secret to deploy.
	Secret []byte
}

// ThumbService serves shrunk, disk-cached copies of seller mockups.
type ThumbService struct {
	repo   *repositories.Repositories
	opts   ThumbOptions
	client *http.Client
	// group collapses concurrent requests for the same thumbnail into one fetch:
	// three QC stations scanning items that share a mockup pull it once.
	group singleflight.Group
	// warming tracks hashes the background warmer already has in flight, so a
	// second scan of the same tray does not re-queue the same work.
	warming sync.Map
	// failed remembers hash -> time of the last failed build, so a mockup we
	// cannot render is not re-fetched on every scan. Bounded by the number of
	// distinct broken mockups in circulation, and swept alongside the disk cache.
	failed sync.Map
}

// NewThumbService builds the cache and prepares its directory. A directory that
// cannot be created disables the cache rather than failing startup: a stale or
// missing cache disk must never be the reason the QC station cannot open.
func NewThumbService(repo *repositories.Repositories, opts ThumbOptions) *ThumbService {
	if opts.Dir != "" {
		if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
			log.Printf("thumbnails: disabled — cannot create cache dir %q: %v", opts.Dir, err)
			opts.Dir = ""
		}
	}
	if opts.MaxPx <= 0 {
		opts.MaxPx = 900
	}
	if opts.URLTTL <= 0 {
		opts.URLTTL = 24 * time.Hour
	}
	return &ThumbService{
		repo:   repo,
		opts:   opts,
		client: newSafeAssetClient(thumbFetchTimeout),
	}
}

// Enabled reports whether thumbnails are being cached at all.
func (s *ThumbService) Enabled() bool { return s != nil && s.opts.Dir != "" }

// sourceHash is the cache identity of a mockup: its URL, not the item that
// happens to reference it. Items of the same SKU routinely share one mockup, so
// hashing the URL means the picture is fetched and stored once for all of them.
func sourceHash(rawURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawURL)))
	return hex.EncodeToString(sum[:])[:32]
}

// sign authenticates a (hash, item, expiry) triple. The thumbnail endpoint has
// to be reachable from an <img> tag, and an <img> tag cannot carry the bearer
// token the rest of the API uses — so the URL itself is the credential. Binding
// the item id in means a signature minted for one mockup cannot be replayed to
// read another, and the expiry means a link that leaks stops working.
func (s *ThumbService) sign(hash string, itemID uint, exp int64) string {
	mac := hmac.New(sha256.New, s.opts.Secret)
	fmt.Fprintf(mac, "%s|%d|%d", hash, itemID, exp)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// SignedURL returns the API path the frontend should point an <img> at, or ""
// when the cache is off or the item has no mockup (the caller then keeps using
// the raw URL). The hash rides in the path so a cache hit needs no database
// lookup at all — the file name is derivable from the request alone.
func (s *ThumbService) SignedURL(itemID uint, mockupURL string) string {
	if !s.Enabled() || strings.TrimSpace(mockupURL) == "" || itemID == 0 {
		return ""
	}
	hash := sourceHash(mockupURL)
	exp := time.Now().Add(s.opts.URLTTL).Unix()
	return fmt.Sprintf("/api/assets/thumb/%s.jpg?i=%d&e=%d&s=%s",
		hash, itemID, exp, s.sign(hash, itemID, exp))
}

// ParseRequest validates a signed thumbnail request and returns its hash and
// item id. Every failure is deliberately the same opaque error: the endpoint is
// unauthenticated, so it should not explain to a prober which part was wrong.
func (s *ThumbService) ParseRequest(name, itemParam, expParam, sig string) (hash string, itemID uint, err error) {
	bad := errors.New("invalid thumbnail request")
	if !s.Enabled() {
		return "", 0, bad
	}
	hash = strings.TrimSuffix(name, ".jpg")
	if len(hash) != 32 || strings.ContainsAny(hash, "/\\.") {
		return "", 0, bad
	}
	if _, decErr := hex.DecodeString(hash); decErr != nil {
		return "", 0, bad
	}
	id64, convErr := strconv.ParseUint(itemParam, 10, 64)
	if convErr != nil || id64 == 0 {
		return "", 0, bad
	}
	exp, convErr := strconv.ParseInt(expParam, 10, 64)
	if convErr != nil || time.Now().Unix() > exp {
		return "", 0, bad
	}
	if !hmac.Equal([]byte(s.sign(hash, uint(id64), exp)), []byte(sig)) {
		return "", 0, bad
	}
	return hash, uint(id64), nil
}

// path is where a given thumbnail lives. The first byte of the hash shards the
// files across 256 directories so a catalogue with many thousands of mockups
// never puts them all in one directory.
func (s *ThumbService) path(hash string) string {
	return filepath.Join(s.opts.Dir, hash[:2], hash+".jpg")
}

// Cached returns the file path when the thumbnail is already on disk. It also
// refreshes the file's modification time so the sweeper measures "last used"
// rather than "first built" — the touch is best-effort and its failure only
// costs an early eviction.
func (s *ThumbService) Cached(hash string) (string, bool) {
	if !s.Enabled() {
		return "", false
	}
	p := s.path(hash)
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return "", false
	}
	if time.Since(info.ModTime()) > time.Hour {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
	return p, true
}

// recentlyFailed reports whether this source failed recently enough that trying
// again would just make the operator wait for the same failure.
func (s *ThumbService) recentlyFailed(hash string) bool {
	at, ok := s.failed.Load(hash)
	if !ok {
		return false
	}
	if time.Since(at.(time.Time)) > thumbFailureTTL {
		s.failed.Delete(hash)
		return false
	}
	return true
}

// Materialize produces the thumbnail for an item, fetching and shrinking the
// source if it is not cached yet. It returns the cached file path on success
// and the item's original mockup URL alongside any failure, so the handler can
// redirect there instead of showing QC a broken image.
func (s *ThumbService) Materialize(ctx context.Context, itemID uint, hash string) (path string, sourceURL string, err error) {
	if !s.Enabled() {
		return "", "", errThumbUnusable
	}
	sourceURL, err = s.repo.OrderItem.MockupURLByID(itemID)
	if err != nil || strings.TrimSpace(sourceURL) == "" {
		return "", "", errThumbUnusable
	}
	// The signature proved the caller was given this pair by us; this proves the
	// pair still matches the database. A mockup edited since the URL was signed
	// hashes differently, and must not be served from the old file.
	if sourceHash(sourceURL) != hash {
		return "", sourceURL, errThumbUnusable
	}
	if p, ok := s.Cached(hash); ok {
		return p, sourceURL, nil
	}
	if s.recentlyFailed(hash) {
		return "", sourceURL, errThumbUnusable
	}
	res, err, _ := s.group.Do(hash, func() (any, error) {
		if p, ok := s.Cached(hash); ok {
			return p, nil
		}
		p, buildErr := s.build(ctx, hash, sourceURL)
		if buildErr != nil {
			s.failed.Store(hash, time.Now())
		}
		return p, buildErr
	})
	if err != nil {
		return "", sourceURL, err
	}
	return res.(string), sourceURL, nil
}

// thumbSourceCandidates lists, best first, the URLs worth trying to obtain the
// picture behind a mockup link.
//
// For Google Drive the direct-download endpoint (what normalizeAssetURL returns,
// and what the assets ZIP correctly uses) hands back the ORIGINAL file — which
// for a print mockup is routinely tens of megabytes. We are about to throw all
// but a 900px copy of it away, so pulling the whole thing is waste that shows up
// as a slow first scan. Drive's own thumbnail endpoint answers the same picture
// pre-shrunk, in a few hundred KB. We ask for it at a size comfortably above our
// own target so the downscale still has pixels to work with, and fall back to
// the full download when Drive declines to render one (it does not thumbnail
// every format).
//
// Anything that is not Drive gets the single normalized URL, exactly as before.
func thumbSourceCandidates(rawURL string) []string {
	trimmed := strings.TrimSpace(rawURL)
	if u, err := url.Parse(trimmed); err == nil {
		if id := driveFileID(u); id != "" {
			return []string{
				"https://drive.google.com/thumbnail?id=" + url.QueryEscape(id) + "&sz=w1600",
				normalizeAssetURL(trimmed),
			}
		}
	}
	return []string{normalizeAssetURL(trimmed)}
}

// build fetches the source, shrinks it and writes the JPEG to the cache.
func (s *ThumbService) build(ctx context.Context, hash, sourceURL string) (string, error) {
	var lastErr error = errThumbUnusable
	for _, candidate := range thumbSourceCandidates(sourceURL) {
		encoded, err := s.fetchAndShrink(ctx, candidate)
		if err != nil {
			lastErr = err
			continue
		}
		return s.writeThumb(hash, encoded)
	}
	return "", lastErr
}

// fetchAndShrink downloads one candidate URL and returns the encoded thumbnail.
func (s *ThumbService) fetchAndShrink(ctx context.Context, rawURL string) ([]byte, error) {
	if _, err := validatePublicHTTPURL(rawURL); err != nil {
		return nil, fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: upstream returned %d", errThumbUnusable, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, thumbMaxSourceBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	// A share link that was never made public answers 200 with a login page.
	// Decoding would fail anyway, but saying so plainly keeps the log readable.
	if isHTMLResponse(resp.Header, raw) {
		return nil, fmt.Errorf("%w: %s", errThumbUnusable, assetFetchHint(rawURL))
	}
	return shrinkToJPEG(raw, s.opts.MaxPx)
}

// writeThumb stores an encoded thumbnail and returns its path. It writes to a
// temporary file and renames: a request arriving mid-write must never see (and
// then cache-serve) half a JPEG.
func (s *ThumbService) writeThumb(hash string, encoded []byte) (string, error) {
	p := s.path(hash)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	return p, nil
}

// shrinkToJPEG decodes an arbitrary image and re-encodes it as a JPEG whose
// longest edge is at most maxPx.
func shrinkToJPEG(raw []byte, maxPx int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable (%v)", errThumbUnusable, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > thumbMaxSourcePixels {
		return nil, fmt.Errorf("%w: %dx%d is out of range", errThumbUnusable, cfg.Width, cfg.Height)
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable (%v)", errThumbUnusable, err)
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	// Never upscale: a mockup smaller than the target is already as good as it
	// gets, and blowing it up would only cost bytes.
	if longest := max(w, h); longest > maxPx {
		scale := float64(maxPx) / float64(longest)
		w = max(1, int(float64(w)*scale))
		h = max(1, int(float64(h)*scale))
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// JPEG has no alpha. Mockups are frequently transparent PNGs, and letting
	// those composite onto JPEG's default black would hide dark artwork from the
	// very person whose job is to look at it — so flatten onto white, the colour
	// the product is judged against anyway.
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: thumbJPEGQuality}); err != nil {
		return nil, fmt.Errorf("%w: %v", errThumbUnusable, err)
	}
	return buf.Bytes(), nil
}

// Warm builds thumbnails in the background for mockups QC is about to need.
// This is what turns "the first scan of a tray is slow" into "no scan is slow":
// scanning any item in a batch queues the rest of that batch, so by the time the
// operator reaches item two the picture is already on disk.
//
// It is entirely best-effort — nothing waits on it and no failure is reported.
func (s *ThumbService) Warm(items []repositories.ItemMockup) {
	if !s.Enabled() || len(items) == 0 {
		return
	}
	if len(items) > thumbWarmCap {
		items = items[:thumbWarmCap]
	}
	type job struct {
		id   uint
		hash string
		url  string
	}
	queue := make([]job, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.MockupURL) == "" || it.ItemID == 0 {
			continue
		}
		hash := sourceHash(it.MockupURL)
		if _, cached := s.Cached(hash); cached {
			continue
		}
		if s.recentlyFailed(hash) {
			continue
		}
		// Claim the hash so overlapping scans of the same tray queue it once.
		if _, busy := s.warming.LoadOrStore(hash, struct{}{}); busy {
			continue
		}
		queue = append(queue, job{id: it.ItemID, hash: hash, url: it.MockupURL})
	}
	if len(queue) == 0 {
		return
	}
	go func() {
		// Detached from the scan's request context on purpose: the operator's
		// HTTP response ends long before this finishes, and cancelling with it
		// would defeat the entire point of warming.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		sem := make(chan struct{}, thumbWarmConcurrency)
		var wg sync.WaitGroup
		for _, j := range queue {
			wg.Add(1)
			go func(j job) {
				defer wg.Done()
				defer s.warming.Delete(j.hash)
				sem <- struct{}{}
				defer func() { <-sem }()
				if _, err, _ := s.group.Do(j.hash, func() (any, error) {
					if p, ok := s.Cached(j.hash); ok {
						return p, nil
					}
					p, buildErr := s.build(ctx, j.hash, j.url)
					if buildErr != nil {
						s.failed.Store(j.hash, time.Now())
					}
					return p, buildErr
				}); err != nil {
					log.Printf("thumbnails: warm failed for item %d: %v", j.id, err)
				}
			}(j)
		}
		wg.Wait()
	}()
}

// Sweep deletes thumbnails nobody has requested in a long time, so the cache
// tracks the mockups actually in circulation instead of growing for the life of
// the deployment. Safe to run while the cache is serving: a file deleted out
// from under a request is simply rebuilt on the next one.
func (s *ThumbService) Sweep() {
	if !s.Enabled() {
		return
	}
	s.failed.Range(func(k, v any) bool {
		if time.Since(v.(time.Time)) > thumbFailureTTL {
			s.failed.Delete(k)
		}
		return true
	})
	cutoff := time.Now().Add(-thumbSweepTTL)
	removed := 0
	err := filepath.WalkDir(s.opts.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		info, statErr := d.Info()
		if statErr != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if os.Remove(p) == nil {
			removed++
		}
		return nil
	})
	if err != nil {
		log.Printf("thumbnails: sweep failed: %v", err)
		return
	}
	if removed > 0 {
		log.Printf("thumbnails: swept %d unused thumbnail(s)", removed)
	}
}

// StartSweeper runs Sweep on an interval until ctx is done.
func (s *ThumbService) StartSweeper(ctx context.Context, every time.Duration) {
	if !s.Enabled() || every <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Sweep()
			}
		}
	}()
}
