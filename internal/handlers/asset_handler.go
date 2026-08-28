package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ThumbnailAsset serves the cached, screen-sized copy of a seller mockup.
//
// GET /api/assets/thumb/:name?i=<item>&e=<expiry>&s=<signature>
//
// This route is deliberately NOT behind the bearer-token middleware: it exists
// to be the src of an <img>, and a browser cannot attach an Authorization header
// to an image request. The signed URL is the credential instead — minted by the
// QC scan response, scoped to one item, and expiring on its own. Anything that
// does not verify is a flat 404, with no hint about which part was wrong.
//
// A miss that cannot be built answers 404 too, and the station falls back to
// loading the mockup from its original URL. Failing open like this matters more
// than the cache does: QC must never be blocked by our own optimisation.
func (h *Handlers) ThumbnailAsset(c *gin.Context) {
	thumb := h.svc.Thumb
	hash, itemID, err := thumb.ParseRequest(c.Param("name"), c.Query("i"), c.Query("e"), c.Query("s"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	// The hot path: the file name is derivable from the request alone, so a warm
	// cache answers without touching the database at all.
	if path, ok := thumb.Cached(hash); ok {
		serveThumbnail(c, hash, path)
		return
	}

	path, _, err := thumb.Materialize(c.Request.Context(), itemID, hash)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	serveThumbnail(c, hash, path)
}

// serveThumbnail writes the file with caching headers. The content behind a
// given URL can never change — the hash IS the content's identity, and editing
// the mockup produces a different hash — so the browser is told it may keep the
// picture without ever revalidating. That is what makes a re-scan of the same
// item instant rather than merely fast.
func serveThumbnail(c *gin.Context, hash, path string) {
	c.Header("Cache-Control", "private, max-age=604800, immutable")
	c.Header("ETag", `"`+hash+`"`)
	c.Header("Content-Type", "image/jpeg")
	c.File(path)
}
