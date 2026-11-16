package warp

import (
	"net/netip"
	"testing"
)

func TestParseConnectVia(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		bad  bool
	}{
		{raw: "", want: ""},
		{raw: "127.0.0.1:40000", want: "socks5://127.0.0.1:40000"},
		{raw: "socks5://127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
		{raw: "socks5h://example.com", want: "socks5://example.com:1080"},
		{raw: "socks5://user:pass@example.com:1080", want: "socks5://user:pass@example.com:1080"},
		{raw: "http://127.0.0.1:8080", bad: true},
		{raw: "https://127.0.0.1:8080", bad: true},
	}
	for _, tc := range cases {
		got, err := NormalizeConnectVia(tc.raw)
		if tc.bad {
			if err == nil {
				t.Fatalf("NormalizeConnectVia(%q) = %q, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeConnectVia(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeConnectVia(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSOCKSUDPDatagramRoundTrip(t *testing.T) {
	targets := []netip.AddrPort{
		netip.MustParseAddrPort("162.159.192.1:2408"),
		netip.MustParseAddrPort("[2606:4700:d0::a29f:c001]:2408"),
	}
	payload := []byte("wireguard-handshake")
	for _, target := range targets {
		datagram := encodeUDPDatagram(target, payload)
		gotPayload, gotFrom, err := parseUDPDatagram(datagram)
		if err != nil {
			t.Fatalf("parse %s: %v", target, err)
		}
		if string(gotPayload) != string(payload) {
			t.Fatalf("payload for %s = %q, want %q", target, gotPayload, payload)
		}
		if gotFrom != target {
			t.Fatalf("source for %s = %s", target, gotFrom)
		}
	}
}

func TestParseUDPDatagramRejectsFragment(t *testing.T) {
	datagram := encodeUDPDatagram(netip.MustParseAddrPort("1.2.3.4:5"), []byte("x"))
	datagram[2] = 1
	if _, _, err := parseUDPDatagram(datagram); err == nil {
		t.Fatal("fragmented datagram was accepted")
	}
}
