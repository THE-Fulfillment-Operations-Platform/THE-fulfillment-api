package services

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SSRF guard for server-side asset fetches (e.g. the batch-assets ZIP). Asset
// URLs originate from seller-supplied import data, so a naive fetch would let a
// seller point the server at cloud metadata (169.254.169.254), localhost, or an
// internal service and receive the response back inside the ZIP. These helpers
// ensure the server only ever connects to public IP addresses.

// maxAssetBytes caps a single downloaded asset so a hostile/huge remote file
// can't exhaust memory or disk while streaming into the archive.
const maxAssetBytes int64 = 100 << 20 // 100 MB

// isDisallowedIP reports whether an IP must never be reached by a server-side
// fetch: loopback, private (RFC1918 / ULA), link-local (incl. cloud metadata
// 169.254.169.254), unspecified or multicast.
func isDisallowedIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

// validatePublicHTTPURL parses rawURL, requires an http/https scheme and a host
// that is (or resolves to) only public IPs. Used for early rejection + clear
// errors; the dial-time guard in newSafeAssetClient is the authoritative check.
func validatePublicHTTPURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL host is empty")
	}
	if lit := net.ParseIP(host); lit != nil {
		if isDisallowedIP(lit) {
			return nil, fmt.Errorf("URL host is a private/loopback address")
		}
		return u, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("could not resolve URL host")
	}
	for _, ip := range ips {
		if isDisallowedIP(ip) {
			return nil, fmt.Errorf("URL host resolves to a private/loopback address")
		}
	}
	return u, nil
}

// newSafeAssetClient builds an http.Client whose DialContext re-resolves the
// host and refuses to connect to any non-public IP — this catches literal
// private IPs, redirect-to-private, and DNS-rebinding (it dials the exact IP it
// validated). Redirects are re-validated and capped.
func newSafeAssetClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			var chosen net.IP
			if lit := net.ParseIP(host); lit != nil {
				chosen = lit
			} else {
				ips, lerr := net.LookupIP(host)
				if lerr != nil || len(ips) == 0 {
					return nil, fmt.Errorf("cannot resolve host %s", host)
				}
				for _, cand := range ips {
					if !isDisallowedIP(cand) {
						chosen = cand
						break
					}
				}
			}
			if isDisallowedIP(chosen) {
				return nil, fmt.Errorf("blocked connection to non-public address for host %s", host)
			}
			// Dial the exact validated IP so a rebinding re-resolution can't slip a
			// private address in between the check and the connection.
			return dialer.DialContext(ctx, network, net.JoinHostPort(chosen.String(), port))
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if _, err := validatePublicHTTPURL(req.URL.String()); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}
}
