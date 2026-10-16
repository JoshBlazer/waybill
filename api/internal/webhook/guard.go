package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrURLNotAllowed is returned for an endpoint URL that Waybill will not send to.
var ErrURLNotAllowed = errors.New("webhook: URL not allowed")

// ErrRefused means the URL resolved to an address Waybill will not connect to
// (the SSRF guard). Nothing was sent.
var ErrRefused = errors.New("webhook: destination refused")

// CheckURL validates an endpoint URL at registration: https, a host name or
// public IP, no credentials. allowPrivate (local development only) also
// accepts http and private, loopback and link-local hosts.
func CheckURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("%w: not an absolute URL", ErrURLNotAllowed)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in the URL", ErrURLNotAllowed)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowPrivate:
	default:
		return fmt.Errorf("%w: must use https", ErrURLNotAllowed)
	}
	if allowPrivate {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || !strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return fmt.Errorf("%w: not a public host", ErrURLNotAllowed)
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return fmt.Errorf("%w: not a public address", ErrURLNotAllowed)
	}
	return nil
}

// cgnat is 100.64.0.0/10, shared address space that is not routable on the
// internet but is not covered by netip's IsPrivate.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}

// NewClient returns the HTTP client deliveries use. Its dialer checks the
// address after DNS resolution, so a public name that resolves to a private
// address (or is rebound to one later) is refused at connect time. Redirects
// are not followed, and there is no proxy.
func NewClient(allowPrivate bool, timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: timeout}
	if !allowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrRefused
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !publicIP(ip) {
				return fmt.Errorf("%w: %s is not a public address", ErrRefused, host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   2,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
