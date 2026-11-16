package chatfiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Disk lưu file dưới một thư mục trên đĩa server.
type Disk struct{ dir string }

// NewDisk tạo kho đĩa (tạo thư mục nếu chưa có).
func NewDisk(dir string) (*Disk, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("chatfiles: thiếu thư mục lưu file")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("chatfiles: không tạo được %s: %w", abs, err)
	}
	return &Disk{dir: abs}, nil
}

func (d *Disk) Kind() string { return "disk" }

// Dir: thư mục gốc (để log).
func (d *Disk) Dir() string { return d.dir }

// path chặn khoá thoát ra ngoài thư mục gốc ("../").
func (d *Disk) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if clean == "/" || strings.Contains(key, "..") {
		return "", fmt.Errorf("chatfiles: khoá không hợp lệ %q", key)
	}
	return filepath.Join(d.dir, clean), nil
}

func (d *Disk) Put(_ context.Context, key string, data []byte, _ string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// Ghi ra file tạm rồi đổi tên: tiến trình chết giữa chừng không để lại file cụt.
	tmp := p + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (d *Disk) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (d *Disk) Locate(_ context.Context, key string, _ time.Duration, _ string) (string, string, error) {
	p, err := d.path(key)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", "", ErrNotFound
	}
	return p, "", nil
}

// FreeBytes: chỗ trống còn lại trên phân vùng chứa thư mục.
func (d *Disk) FreeBytes() int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize) //nolint:unconvert // kiểu khác nhau giữa linux/darwin
}
