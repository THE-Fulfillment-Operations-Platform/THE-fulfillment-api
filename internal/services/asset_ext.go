package services

import (
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

// Naming a downloaded asset inside a ZIP means guessing its file type. The URL is
// the cheapest source but often has none — a Google-Drive share link, a Supabase
// signed URL and a CDN "?download" endpoint all end in an opaque id — and naming
// those ".bin" gives the designer a file their OS refuses to open even though the
// bytes are a perfectly good PNG. So the URL is only the FIRST guess: what the
// server said it was sending (Content-Disposition, then Content-Type) and finally
// the file's own magic bytes all get a turn before ".bin" is used.

// contentTypeExt maps the media types our assets actually arrive as to the
// extension a designer expects. It is consulted before mime.ExtensionsByType
// because the standard table answers "image/jpeg" with ".jfif" on some systems.
var contentTypeExt = map[string]string{
	"image/png":                    ".png",
	"image/jpeg":                   ".jpg",
	"image/jpg":                    ".jpg",
	"image/gif":                    ".gif",
	"image/webp":                   ".webp",
	"image/bmp":                    ".bmp",
	"image/tiff":                   ".tif",
	"image/svg+xml":                ".svg",
	"image/heic":                   ".heic",
	"image/vnd.adobe.photoshop":    ".psd",
	"application/x-photoshop":      ".psd",
	"application/photoshop":        ".psd",
	"application/pdf":              ".pdf",
	"application/postscript":       ".eps",
	"application/illustrator":      ".ai",
	"application/zip":              ".zip",
	"application/x-zip-compressed": ".zip",
	"application/x-rar-compressed": ".rar",
	"application/vnd.rar":          ".rar",
	"application/x-7z-compressed":  ".7z",
}

// extCandidate accepts a string only if it looks like a real file extension:
// a dot, 1–5 chars, letters/digits, at least one letter. The letter rule is what
// stops a version number in a path ("/api/v1.0/download" → ".0") or a hash
// fragment from being pasted onto the file name as its type.
func extCandidate(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if !strings.HasPrefix(ext, ".") || len(ext) < 2 || len(ext) > 6 {
		return ""
	}
	hasLetter := false
	for _, r := range ext[1:] {
		switch {
		case r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9':
		default:
			return ""
		}
	}
	if !hasLetter {
		return ""
	}
	return ext
}

// extFromURL reads the extension off a URL's path (query string excluded).
// Returns "" when the URL carries no usable one.
func extFromURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		u = &url.URL{Path: rawURL}
	}
	return extCandidate(path.Ext(u.Path))
}

// extFromDisposition reads the extension off a Content-Disposition filename —
// the header Drive/S3-style download endpoints use to name a file whose URL is
// just an id.
func extFromDisposition(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	name := params["filename"]
	if name == "" {
		name = params["filename*"]
	}
	return extCandidate(path.Ext(name))
}

// extFromContentType maps a media type to an extension. Generic binary types are
// treated as "no information" so the caller falls through to sniffing.
func extFromContentType(value string) string {
	mt, _, err := mime.ParseMediaType(value)
	if err != nil {
		mt = strings.TrimSpace(strings.Split(value, ";")[0])
	}
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch mt {
	case "", "application/octet-stream", "binary/octet-stream", "application/binary":
		return ""
	}
	if ext, ok := contentTypeExt[mt]; ok {
		return ext
	}
	exts, err := mime.ExtensionsByType(mt)
	if err != nil || len(exts) == 0 {
		return ""
	}
	// ExtensionsByType has no defined order; sort so the same type always yields
	// the same extension instead of one that varies per machine.
	sort.Strings(exts)
	return extCandidate(exts[0])
}

// resolveAssetExt decides the extension for a downloaded asset, best source
// first: the URL path, the Content-Disposition filename, the Content-Type, then
// the leading bytes. ".bin" only when every source is silent — which now means
// the bytes really are of an unknown type, not merely that the link was opaque.
func resolveAssetExt(rawURL string, header http.Header, head []byte) string {
	if ext := extFromURL(rawURL); ext != "" {
		return ext
	}
	if ext := extFromDisposition(header.Get("Content-Disposition")); ext != "" {
		return ext
	}
	if ext := extFromContentType(header.Get("Content-Type")); ext != "" {
		return ext
	}
	if len(head) > 0 {
		if ext := extFromContentType(http.DetectContentType(head)); ext != "" {
			return ext
		}
	}
	return ".bin"
}
