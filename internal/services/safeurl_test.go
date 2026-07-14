package services

import (
	"net"
	"testing"
)

func TestIsDisallowedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,  // loopback
		"10.0.0.5":        true,  // RFC1918
		"192.168.1.10":    true,  // RFC1918
		"172.16.0.1":      true,  // RFC1918
		"169.254.169.254": true,  // link-local / cloud metadata
		"::1":             true,  // v6 loopback
		"fc00::1":         true,  // v6 ULA (private)
		"0.0.0.0":         true,  // unspecified
		"8.8.8.8":         false, // public
		"93.184.216.34":   false, // public
		"2606:2800:220:1:248:1893:25c8:1946": false, // public v6
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if got := isDisallowedIP(ip); got != want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestValidatePublicHTTPURL_Rejects(t *testing.T) {
	bad := []string{
		"ftp://example.com/x",            // non-http scheme
		"file:///etc/passwd",             // non-http scheme
		"http:///nohost",                 // empty host
		"http://169.254.169.254/latest/", // cloud metadata (literal)
		"http://10.0.0.5/internal",       // private (literal)
		"http://127.0.0.1:8080/admin",    // loopback (literal)
		"http://[::1]/",                  // v6 loopback (literal)
		"not a url at all ::::",          // unparseable
	}
	for _, u := range bad {
		if _, err := validatePublicHTTPURL(u); err == nil {
			t.Errorf("expected %q to be rejected", u)
		}
	}
}

func TestValidatePublicHTTPURL_AllowsPublicLiteral(t *testing.T) {
	// Literal public IPs need no DNS, so this stays deterministic/offline.
	for _, u := range []string{"http://93.184.216.34/img.png", "https://8.8.8.8/a.pdf"} {
		if _, err := validatePublicHTTPURL(u); err != nil {
			t.Errorf("expected %q to be allowed, got %v", u, err)
		}
	}
}
