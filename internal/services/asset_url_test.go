package services

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The real defect behind the ".bin" files: the design field held a Google-Drive
// SHARE link, so the server downloaded Drive's HTML viewer page instead of the
// artwork. normalizeAssetURL must turn every form of that link into the endpoint
// that serves the bytes.
func TestNormalizeAssetURL(t *testing.T) {
	const id = "1Vcx0GoWWxK8kq72zOhXIN1mgnZdTMd3G"
	direct := "https://drive.usercontent.google.com/download?id=" + id + "&export=download&confirm=t"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"drive view link", "https://drive.google.com/file/d/" + id + "/view?usp=sharing", direct},
		{"drive view link, no query", "https://drive.google.com/file/d/" + id + "/view", direct},
		{"drive open?id", "https://drive.google.com/open?id=" + id, direct},
		{"drive uc?export=download", "https://drive.google.com/uc?export=download&id=" + id, direct},
		{"docs.google.com host", "https://docs.google.com/uc?id=" + id, direct},
		{
			"drive FOLDER link is left alone (not one file)",
			"https://drive.google.com/drive/folders/1ZMpMiwFW6jbxT6J-Yb0CBb_QVqgmmr7j",
			"https://drive.google.com/drive/folders/1ZMpMiwFW6jbxT6J-Yb0CBb_QVqgmmr7j",
		},
		{
			"a plain CDN link is untouched",
			"https://cdn.example.com/designs/artwork.png?token=abc",
			"https://cdn.example.com/designs/artwork.png?token=abc",
		},
		{"whitespace is trimmed", "  https://cdn.example.com/a.png  ", "https://cdn.example.com/a.png"},
		{"not a URL at all", "chưa có link", "chưa có link"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeAssetURL(tc.in); got != tc.want {
				t.Errorf("normalizeAssetURL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Dropbox preview links have the same disease with a different cure.
func TestNormalizeAssetURL_Dropbox(t *testing.T) {
	got := normalizeAssetURL("https://www.dropbox.com/s/abc123/design.png?dl=0")
	if !strings.Contains(got, "dl=1") || strings.Contains(got, "dl=0") {
		t.Errorf("dropbox link must switch to dl=1, got %q", got)
	}
}

// Even after the rewrite a link can still answer with a page — an unshared file,
// a folder, a login wall. That must FAIL the asset, not get filed as artwork: a
// silently-included web page is exactly what designers could not open.
func TestWriteResponseToZipEntry_RejectsHTMLPage(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	used := map[string]int{}

	page := `<!DOCTYPE html><html lang="en"><head><title>WB300626.01 – Google Drive</title></head><body>Sign in</body></html>`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(page)),
	}
	err := writeResponseToZipEntry(zw, resp, "https://drive.google.com/file/d/abc/view", "AAA_1", used)
	if err == nil {
		t.Fatal("an HTML page must be rejected, not written into the ZIP")
	}
	if !strings.Contains(err.Error(), "Google Drive") {
		t.Errorf("error should name the cause for the person fixing the link, got %q", err.Error())
	}

	_ = zw.Close()
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if len(zr.File) != 0 {
		t.Fatalf("nothing may be written for a rejected asset, got %d entries", len(zr.File))
	}
}

// A page served as HTML without the header (some hosts send text/plain) is still
// a page — sniffing has to catch it.
func TestWriteResponseToZipEntry_RejectsSniffedHTML(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("<!DOCTYPE html><html><body>nope</body></html>")),
	}
	if err := writeResponseToZipEntry(zw, resp, "https://files.example.com/x", "AAA_1", map[string]int{}); err == nil {
		t.Fatal("sniffed HTML must be rejected too")
	}
}

// The hint has to tell the user which mistake they made — folder vs unshared file
// are different fixes.
func TestAssetFetchHint(t *testing.T) {
	for _, link := range []string{
		"https://drive.google.com/drive/folders/1ZMpMiwFW6jb",
		// The account-scoped form batch #101068 was full of: matching only the
		// plain form blamed these folders on sharing settings.
		"https://drive.google.com/drive/u/0/folders/1O_xR_Ovg",
	} {
		if folder := assetFetchHint(link); !strings.Contains(folder, "THƯ MỤC") {
			t.Errorf("folder link hint for %s: got %q", link, folder)
		}
	}
	file := assetFetchHint("https://drive.google.com/file/d/abc/view")
	if !strings.Contains(file, "chia sẻ") {
		t.Errorf("unshared file hint: got %q", file)
	}
}

// A folder link is recognised from the URL alone, in every form Drive hands out,
// and never mistaken for a file link or another host's path.
func TestIsDriveFolderLink(t *testing.T) {
	cases := map[string]bool{
		"https://drive.google.com/drive/folders/1ZMp":                 true,
		"https://drive.google.com/drive/u/0/folders/1O_xR_Ovg":        true,
		"https://drive.google.com/drive/u/2/folders/1O_x?usp=sharing": true,
		"https://drive.google.com/drive/mobile/folders/1O_x":          true,
		"https://drive.google.com/embeddedfolderview?id=1O_x":         true,
		"https://drive.google.com/file/d/abc/view":                    false,
		"https://drive.google.com/uc?id=abc":                          false,
		"https://files.example.com/drive/u/0/folders/abc":             false,
		"not a url %%": false,
	}
	for link, want := range cases {
		if got := isDriveFolderLink(link); got != want {
			t.Errorf("isDriveFolderLink(%q) = %v, want %v", link, got, want)
		}
	}
}

// A folder link fails before any fetch: the nil client would panic if it dialed.
func TestWriteURLToZipEntry_FolderLinkFailsWithoutFetching(t *testing.T) {
	zw := zip.NewWriter(io.Discard)
	err := writeURLToZipEntry(context.Background(), nil, zw, "https://drive.google.com/drive/u/0/folders/1O_x", "A_1", map[string]int{})
	if code, _ := assetFailReason(err); code != assetReasonDriveFolder {
		t.Fatalf("code = %q, want %q (err %v)", code, assetReasonDriveFolder, err)
	}
}
