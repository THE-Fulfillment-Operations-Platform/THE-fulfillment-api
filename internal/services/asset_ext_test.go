package services

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// pngBytes is a real 1x1 PNG: the magic header is what http.DetectContentType
// reads, so this doubles as the sniffing fixture.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
}

// The bug this fixes: a design link with no extension in its path (Drive/Supabase
// signed URLs, CDN download endpoints) was named ".bin" in the ZIP, so a designer
// got a PNG their OS refused to open. Every source of truth about the type now
// gets a turn before ".bin".
func TestResolveAssetExt(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		header http.Header
		head   []byte
		want   string
	}{
		{
			name: "extension in the URL path wins",
			url:  "https://cdn.example.com/files/design.png?token=abc",
			want: ".png",
		},
		{
			name:   "no extension: Content-Disposition filename",
			url:    "https://drive.google.com/uc?id=1a2b3c&export=download",
			header: http.Header{"Content-Disposition": {`attachment; filename="artwork.jpg"`}},
			want:   ".jpg",
		},
		{
			name:   "no extension, no disposition: Content-Type",
			url:    "https://xyz.supabase.co/storage/v1/object/sign/designs/6f1c2",
			header: http.Header{"Content-Type": {"image/png"}},
			want:   ".png",
		},
		{
			name:   "octet-stream tells us nothing: sniff the bytes",
			url:    "https://files.example.com/download/9f8e7d",
			header: http.Header{"Content-Type": {"application/octet-stream"}},
			head:   pngBytes,
			want:   ".png",
		},
		{
			name:   "PDF served as octet-stream",
			url:    "https://files.example.com/download/aaaa",
			header: http.Header{"Content-Type": {"application/octet-stream"}},
			head:   []byte("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n"),
			want:   ".pdf",
		},
		{
			name: "a version number in the path is not an extension",
			url:  "https://api.example.com/v1.0/assets/download",
			header: http.Header{
				"Content-Type": {"image/jpeg"},
			},
			want: ".jpg",
		},
		{
			name:   "charset parameter does not defeat the lookup",
			url:    "https://files.example.com/x",
			header: http.Header{"Content-Type": {"image/svg+xml; charset=utf-8"}},
			want:   ".svg",
		},
		{
			name:   "truly unknown stays .bin",
			url:    "https://files.example.com/blob",
			header: http.Header{"Content-Type": {"application/octet-stream"}},
			head:   []byte{0x07, 0x03, 0x01, 0x09},
			want:   ".bin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.header
			if h == nil {
				h = http.Header{}
			}
			if got := resolveAssetExt(tc.url, h, tc.head); got != tc.want {
				t.Errorf("resolveAssetExt(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// End-to-end over the ZIP writer: the entry must be named from the response, and
// the bytes peeked at for sniffing must still land in the archive intact.
func TestWriteResponseToZipEntry_NamesFromResponseAndKeepsBytes(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	used := map[string]int{}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/octet-stream"}},
		Body:       io.NopCloser(bytes.NewReader(pngBytes)),
	}
	if err := writeResponseToZipEntry(zw, resp, "https://drive.google.com/uc?id=1a2b3c",
		"Design_2026-08-27/AAA_100001_1-1_1", used); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("entries: got %d, want 1", len(zr.File))
	}
	want := "Design_2026-08-27/AAA_100001_1-1_1.png"
	if zr.File[0].Name != want {
		t.Fatalf("entry name: got %q, want %q", zr.File[0].Name, want)
	}

	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatalf("open entry: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if !bytes.Equal(got, pngBytes) {
		t.Fatalf("content: got %d bytes, want the original %d — sniffing must not eat the head",
			len(got), len(pngBytes))
	}
}

// A body shorter than the 512-byte sniff window must not be truncated or error:
// io.ReadFull returns ErrUnexpectedEOF there, which is normal, not a failure.
func TestWriteResponseToZipEntry_ShortBody(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	used := map[string]int{}

	body := "<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"image/svg+xml"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	if err := writeResponseToZipEntry(zw, resp, "https://files.example.com/x", "folder/file", used); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if zr.File[0].Name != "folder/file.svg" {
		t.Fatalf("entry name: got %q, want folder/file.svg", zr.File[0].Name)
	}
	rc, _ := zr.File[0].Open()
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Fatalf("content: got %q, want %q", got, body)
	}
}

// Two assets of the same item that resolve to the same name must not overwrite
// each other, and the de-dup suffix must sit BEFORE the extension.
func TestWriteResponseToZipEntry_DedupsAcrossDownloads(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	used := map[string]int{}

	for i := 0; i < 2; i++ {
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(pngBytes)),
		}
		if err := writeResponseToZipEntry(zw, resp, "https://files.example.com/x", "AAA_1", used); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	_ = zw.Close()

	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	names := []string{zr.File[0].Name, zr.File[1].Name}
	if names[0] != "AAA_1.png" || names[1] != "AAA_1-2.png" {
		t.Fatalf("names: got %v, want [AAA_1.png AAA_1-2.png]", names)
	}
}
