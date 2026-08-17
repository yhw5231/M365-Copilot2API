package chathub

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ValidateRemoteDownloadURL blocks SSRF: only https and public routable
// addresses are accepted, with a lookup-time recheck against private,
// loopback, link-local and cloud metadata ranges. It is the single gate used
// both by ChatHub attachment downloads and by the web image-fetch path.
func ValidateRemoteDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid attachment URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("attachment download requires https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("attachment URL has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("attachment host does not resolve")
	}
	for _, ip := range ips {
		if ipUnsafe(ip) {
			return fmt.Errorf("attachment URL targets a non-public address")
		}
	}
	return nil
}

// MaxRedirects caps how many redirects a validated fetch may follow; every
// hop is re-validated by the caller's CheckRedirect.
const MaxRedirects = 3

// RedirectCheck validates each redirect target with the same SSRF rules as
// the initial request and rejects chains longer than MaxRedirects. Use it as
// http.Client.CheckRedirect for any client that fetches attacker-influenced
// URLs.
func RedirectCheck(req *http.Request, via []*http.Request) error {
	if len(via) >= MaxRedirects {
		return fmt.Errorf("too many redirects (%d)", len(via))
	}
	if err := ValidateRemoteDownloadURL(req.URL.String()); err != nil {
		return fmt.Errorf("redirect target rejected: %w", err)
	}
	return nil
}

func ipUnsafe(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 169.254.0.0/16 link-local is covered above on Go >= 1.17;
		// 100.64.0.0/10 (CGNAT) is not private per IP.IsPrivate.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	// 169.254.169.254 cloud metadata is link-local; belt and braces.
	if strings.HasPrefix(ip.String(), "169.254.169.254") {
		return true
	}
	return false
}
