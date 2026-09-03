package config

import "testing"

// TestIsLocalDB phân biệt database trên máy với database từ xa. Cảnh báo lúc
// khởi động dựa vào nó: chạy dev mà trỏ vào host từ xa gần như luôn là quên đổi
// file env, và cái giá là AutoMigrate + seeder chạy trên dữ liệu thật của khách.
func TestIsLocalDB(t *testing.T) {
	local := []string{"localhost", "127.0.0.1", "::1", "  LOCALHOST  ", "host.docker.internal", "db", "postgres"}
	for _, host := range local {
		if !(&Config{DBHost: host}).isLocalDB() {
			t.Errorf("host %q phải được coi là local", host)
		}
	}
	remote := []string{
		"aws-1-ap-southeast-1.pooler.supabase.com",
		"db.abcdefgh.supabase.co",
		"10.0.0.5",
		"",
	}
	for _, host := range remote {
		if (&Config{DBHost: host}).isLocalDB() {
			t.Errorf("host %q KHÔNG được coi là local", host)
		}
	}
}
