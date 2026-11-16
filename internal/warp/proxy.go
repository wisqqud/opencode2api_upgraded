package warp

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// defaultConnectViaPort is the SOCKS5 port Cloudflare WARP's own proxy mode
// listens on. A bare "host:port" entry is taken literally.
const defaultConnectViaPort = "1080"

// ParseConnectVia turns a connect-via override into a SOCKS5 URL. An empty
// value yields (nil, nil), meaning a direct route. A bare "host:port" is
// treated as SOCKS5; "socks5" and "socks5h" are accepted and canonicalized.
//
// WARP carries the WireGuard handshake over UDP, so only a SOCKS5 upstream can
// raise the tunnel: an HTTP CONNECT proxy can carry the registration request
// but not the datagrams.
func ParseConnectVia(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if !strings.Contains(value, "://") {
		value = "socks5://" + value
	}
	u, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid address: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		u.Scheme = "socks5"
	default:
		return nil, fmt.Errorf("unsupported scheme %q; use socks5, because WARP needs UDP and an HTTP proxy cannot carry it", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%q has no host", raw)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), defaultConnectViaPort)
	}
	return u, nil
}

// NormalizeConnectVia validates raw and returns its canonical socks5 URL, or an
// empty string when raw is empty.
func NormalizeConnectVia(raw string) (string, error) {
	u, err := ParseConnectVia(raw)
	if err != nil || u == nil {
		return "", err
	}
	return u.String(), nil
}
