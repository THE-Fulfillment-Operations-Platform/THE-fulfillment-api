package services

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// A "share link" is not a download link. Pasting the Google-Drive link a seller
// sees in their browser (drive.google.com/file/d/<id>/view) into a design field
// and fetching it server-side returns Drive's HTML viewer page — ~80KB of markup
// with a <title> and a login prompt — not the artwork. That page then went into
// the ZIP under the item's name, which is why designers got files their OS could
// not open no matter what extension they renamed them to. Rewriting the link to
// the file's direct-download endpoint is what actually fixes it.

// driveFileIDRe matches the id segment of the /file/d/<id>/… share form. Drive
// ids are opaque base64url-ish strings.
var driveFileIDRe = regexp.MustCompile(`^/file/d/([-_A-Za-z0-9]+)`)

// driveFileID extracts a Drive FILE id from the link forms people actually paste.
// A folder link (/drive/folders/<id>) deliberately yields nothing: a folder is not
// one file and cannot be downloaded as the item's artwork.
func driveFileID(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if host != "drive.google.com" && host != "docs.google.com" && host != "drive.usercontent.google.com" {
		return ""
	}
	if m := driveFileIDRe.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	// /open?id=…, /uc?id=…, /download?id=… all carry the id as a query param.
	switch strings.TrimSuffix(u.Path, "/") {
	case "/open", "/uc", "/download":
		if id := u.Query().Get("id"); id != "" {
			return id
		}
	}
	return ""
}

// normalizeAssetURL rewrites a share link into the direct-download URL for the
// same file. Anything it does not recognise is returned untouched, so a plain
// CDN/S3 link keeps working exactly as before.
//
// Drive: drive.usercontent.google.com is the endpoint that serves the bytes and
// the real file name in Content-Disposition; confirm=t clears the "can't scan
// this file for viruses" interstitial that larger files hit.
// Dropbox: ?dl=1 turns the preview page into the file itself.
func normalizeAssetURL(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return trimmed
	}

	if id := driveFileID(u); id != "" {
		return "https://drive.usercontent.google.com/download?id=" + url.QueryEscape(id) +
			"&export=download&confirm=t"
	}

	if host := strings.ToLower(u.Hostname()); host == "www.dropbox.com" || host == "dropbox.com" {
		q := u.Query()
		q.Set("dl", "1")
		u.RawQuery = q.Encode()
		return u.String()
	}

	return trimmed
}

// isHTMLResponse reports whether what came back is a web page rather than a file.
// A design asset is never HTML, so this is always a failed fetch: a viewer page,
// a login wall ("request access"), or an error page served with status 200.
func isHTMLResponse(header http.Header, head []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0]))
	if ct == "text/html" || ct == "application/xhtml+xml" {
		return true
	}
	if len(head) > 0 && strings.HasPrefix(http.DetectContentType(head), "text/html") {
		return true
	}
	return false
}

// assetFetchHint explains a link that returned a web page, in the terms of the
// person who has to fix it — whoever pasted the link.
func assetFetchHint(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "link không trả về file"
	}
	host := strings.ToLower(u.Hostname())
	if host == "drive.google.com" || host == "docs.google.com" || host == "drive.usercontent.google.com" {
		if strings.HasPrefix(u.Path, "/drive/folders/") {
			return "link là THƯ MỤC Google Drive, không phải một file — cần dán link của đúng file design"
		}
		if base := path.Base(u.Path); base == "edit" || strings.HasPrefix(u.Path, "/document/") ||
			strings.HasPrefix(u.Path, "/spreadsheets/") || strings.HasPrefix(u.Path, "/presentation/") {
			return "link là tài liệu Google (Docs/Sheets/Slides), không phải file design"
		}
		return "Google Drive trả về trang web thay vì file — nhiều khả năng file chưa được chia sẻ ở chế độ \"Bất kỳ ai có đường liên kết\""
	}
	return "link trả về trang web thay vì file — kiểm tra quyền chia sẻ hoặc dùng link tải trực tiếp"
}
