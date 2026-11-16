package warp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

const (
	socks5Version  = 0x05
	socks5NoAuth   = 0x00
	socks5UserPass = 0x02
	socks5CmdUDP   = 0x03

	socks5ATYPIPv4   = 0x01
	socks5ATYPDomain = 0x03
	socks5ATYPIPv6   = 0x04

	socks5UDPBuffer = 65535
)

// socksBind carries the outer WireGuard socket through a SOCKS5 proxy using UDP
// ASSOCIATE. It exists for hosts whose direct route to Cloudflare's endpoint is
// blocked: the same proxy that carries the registration request also carries
// the handshake and data datagrams, so WARP can still come up.
type socksBind struct {
	proxy *url.URL

	mu      sync.Mutex
	ctrl    net.Conn
	udp     *net.UDPConn
	relay   netip.AddrPort
	closed  bool
	scratch [socks5UDPBuffer]byte
}

func (b *socksBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctrl != nil || b.udp != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	b.closed = false
	ctrl, err := dialSocks5(b.proxy)
	if err != nil {
		return nil, 0, err
	}
	relay, err := socks5UDPAssociate(ctrl)
	if err != nil {
		ctrl.Close()
		return nil, 0, err
	}
	network := "udp4"
	if relay.Addr().Is6() && !relay.Addr().Is4In6() {
		network = "udp6"
	}
	udp, err := net.ListenUDP(network, nil)
	if err != nil {
		ctrl.Close()
		return nil, 0, err
	}
	b.ctrl, b.udp, b.relay = ctrl, udp, relay
	return []conn.ReceiveFunc{b.receive}, uint16(udp.LocalAddr().(*net.UDPAddr).Port), nil
}

func (b *socksBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var errs []error
	if b.udp != nil {
		errs = append(errs, b.udp.Close())
		b.udp = nil
	}
	if b.ctrl != nil {
		errs = append(errs, b.ctrl.Close())
		b.ctrl = nil
	}
	return errors.Join(errs...)
}

func (b *socksBind) SetMark(uint32) error { return nil }

func (b *socksBind) Send(bufs [][]byte, endpoint conn.Endpoint) error {
	ep, ok := endpoint.(*simpleEndpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	b.mu.Lock()
	udp, relay, closed := b.udp, b.relay, b.closed
	b.mu.Unlock()
	if closed || udp == nil {
		return net.ErrClosed
	}
	for _, buf := range bufs {
		datagram := encodeUDPDatagram(ep.addr, buf)
		if _, err := udp.WriteToUDPAddrPort(datagram, relay); err != nil {
			return err
		}
	}
	return nil
}

func (*socksBind) ParseEndpoint(value string) (conn.Endpoint, error) {
	addr, err := netip.ParseAddrPort(value)
	if err != nil {
		return nil, err
	}
	return &simpleEndpoint{addr: addr}, nil
}

func (*socksBind) BatchSize() int { return 1 }

func (b *socksBind) receive(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
	b.mu.Lock()
	udp, closed := b.udp, b.closed
	b.mu.Unlock()
	if closed || udp == nil {
		return 0, net.ErrClosed
	}
	n, _, err := udp.ReadFromUDPAddrPort(b.scratch[:])
	if err != nil {
		return 0, err
	}
	payload, from, err := parseUDPDatagram(b.scratch[:n])
	if err != nil || len(payload) == 0 || len(payload) > len(packets[0]) {
		// Malformed or oversized datagrams are dropped; the WireGuard reader
		// asks again instead of tearing the tunnel down.
		return 0, nil
	}
	copy(packets[0], payload)
	sizes[0] = len(payload)
	eps[0] = &simpleEndpoint{addr: from}
	return 1, nil
}

func dialSocks5(proxy *url.URL) (net.Conn, error) {
	if proxy == nil {
		return nil, errors.New("no SOCKS5 proxy configured")
	}
	address := proxy.Host
	if proxy.Port() == "" {
		address = net.JoinHostPort(proxy.Hostname(), defaultConnectViaPort)
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	ctrl, err := dialer.Dial("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial SOCKS5 proxy %s: %w", address, err)
	}
	_ = ctrl.SetDeadline(time.Now().Add(10 * time.Second))
	if err := socks5Negotiate(ctrl, proxy); err != nil {
		ctrl.Close()
		return nil, err
	}
	_ = ctrl.SetDeadline(time.Time{})
	return ctrl, nil
}

func socks5Negotiate(ctrl net.Conn, proxy *url.URL) error {
	methods := []byte{socks5NoAuth}
	user, pass := "", ""
	if proxy.User != nil {
		user = proxy.User.Username()
		pass, _ = proxy.User.Password()
		methods = append(methods, socks5UserPass)
	}
	if _, err := ctrl.Write(append([]byte{socks5Version, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	var reply [2]byte
	if _, err := io.ReadFull(ctrl, reply[:]); err != nil {
		return err
	}
	if reply[0] != socks5Version {
		return fmt.Errorf("SOCKS5 proxy answered version %d", reply[0])
	}
	switch reply[1] {
	case socks5NoAuth:
		return nil
	case socks5UserPass:
		if len(user) > 255 || len(pass) > 255 {
			return errors.New("SOCKS5 username or password is longer than 255 bytes")
		}
		msg := append([]byte{0x01, byte(len(user))}, user...)
		msg = append(msg, byte(len(pass)))
		msg = append(msg, pass...)
		if _, err := ctrl.Write(msg); err != nil {
			return err
		}
		var authReply [2]byte
		if _, err := io.ReadFull(ctrl, authReply[:]); err != nil {
			return err
		}
		if authReply[1] != 0 {
			return errors.New("SOCKS5 proxy rejected the username/password")
		}
		return nil
	case 0xFF:
		return errors.New("SOCKS5 proxy accepted none of the offered auth methods")
	default:
		return fmt.Errorf("SOCKS5 proxy selected unsupported auth method %d", reply[1])
	}
}

func socks5UDPAssociate(ctrl net.Conn) (netip.AddrPort, error) {
	// 0.0.0.0:0 lets the proxy infer the client's UDP source from the first
	// datagram sent on the association.
	request := []byte{socks5Version, socks5CmdUDP, 0x00, socks5ATYPIPv4, 0, 0, 0, 0, 0, 0}
	if _, err := ctrl.Write(request); err != nil {
		return netip.AddrPort{}, err
	}
	// Read VER and REP first: a proxy that does not implement UDP ASSOCIATE
	// often answers with just those two bytes and closes, so waiting for the
	// full header would hide the real reason behind "unexpected EOF".
	var head [2]byte
	if _, err := io.ReadFull(ctrl, head[:]); err != nil {
		return netip.AddrPort{}, fmt.Errorf("SOCKS5 proxy closed the connection during UDP ASSOCIATE: %w", err)
	}
	if head[0] != socks5Version {
		return netip.AddrPort{}, fmt.Errorf("SOCKS5 UDP ASSOCIATE answered version %d", head[0])
	}
	if head[1] != 0 {
		return netip.AddrPort{}, fmt.Errorf("SOCKS5 UDP ASSOCIATE refused: %s", socks5ReplyText(head[1]))
	}
	var tail [2]byte
	if _, err := io.ReadFull(ctrl, tail[:]); err != nil {
		return netip.AddrPort{}, fmt.Errorf("SOCKS5 proxy sent a truncated UDP ASSOCIATE reply: %w", err)
	}
	addr, err := readSocks5Addr(ctrl, tail[1])
	if err != nil {
		return netip.AddrPort{}, err
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(ctrl, portBuf[:]); err != nil {
		return netip.AddrPort{}, err
	}
	port := binary.BigEndian.Uint16(portBuf[:])
	if !addr.IsValid() || addr.IsUnspecified() {
		// The proxy told us to use the address the control connection already
		// talks to.
		if remote, ok := ctrl.RemoteAddr().(*net.TCPAddr); ok {
			if ip, ok := netip.AddrFromSlice(remote.IP); ok {
				addr = ip.Unmap()
			}
		}
	}
	if !addr.IsValid() {
		return netip.AddrPort{}, errors.New("SOCKS5 proxy returned no usable UDP relay address")
	}
	return netip.AddrPortFrom(addr.Unmap(), port), nil
}

func readSocks5Addr(r io.Reader, atyp byte) (netip.Addr, error) {
	switch atyp {
	case socks5ATYPIPv4:
		var raw [4]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return netip.Addr{}, err
		}
		return netip.AddrFrom4(raw), nil
	case socks5ATYPIPv6:
		var raw [16]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return netip.Addr{}, err
		}
		return netip.AddrFrom16(raw), nil
	case socks5ATYPDomain:
		var length [1]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return netip.Addr{}, err
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(r, name); err != nil {
			return netip.Addr{}, err
		}
		ips, err := net.LookupIP(string(name))
		if err != nil || len(ips) == 0 {
			return netip.Addr{}, fmt.Errorf("resolve SOCKS5 relay host %q: %w", name, err)
		}
		addr, ok := netip.AddrFromSlice(ips[0])
		if !ok {
			return netip.Addr{}, fmt.Errorf("resolve SOCKS5 relay host %q", name)
		}
		return addr.Unmap(), nil
	default:
		return netip.Addr{}, fmt.Errorf("SOCKS5 reply used unknown address type %d", atyp)
	}
}

func encodeUDPDatagram(target netip.AddrPort, payload []byte) []byte {
	addr := target.Addr().Unmap()
	if addr.Is4() {
		raw := addr.As4()
		header := make([]byte, 0, 10+len(payload))
		header = append(header, 0, 0, 0, socks5ATYPIPv4)
		header = append(header, raw[:]...)
		header = binary.BigEndian.AppendUint16(header, target.Port())
		return append(header, payload...)
	}
	raw := addr.As16()
	header := make([]byte, 0, 22+len(payload))
	header = append(header, 0, 0, 0, socks5ATYPIPv6)
	header = append(header, raw[:]...)
	header = binary.BigEndian.AppendUint16(header, target.Port())
	return append(header, payload...)
}

func parseUDPDatagram(datagram []byte) ([]byte, netip.AddrPort, error) {
	if len(datagram) < 4 {
		return nil, netip.AddrPort{}, errors.New("short SOCKS5 UDP datagram")
	}
	if datagram[0] != 0 || datagram[1] != 0 {
		return nil, netip.AddrPort{}, errors.New("bad SOCKS5 UDP reserved bytes")
	}
	if datagram[2] != 0 {
		return nil, netip.AddrPort{}, errors.New("fragmented SOCKS5 UDP datagrams are not supported")
	}
	addr, offset, err := parseUDPAddr(datagram, 3)
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	if len(datagram) < offset+2 {
		return nil, netip.AddrPort{}, errors.New("short SOCKS5 UDP datagram")
	}
	port := binary.BigEndian.Uint16(datagram[offset:])
	return datagram[offset+2:], netip.AddrPortFrom(addr, port), nil
}

func parseUDPAddr(buf []byte, offset int) (netip.Addr, int, error) {
	if len(buf) <= offset {
		return netip.Addr{}, 0, errors.New("short SOCKS5 UDP datagram")
	}
	switch buf[offset] {
	case socks5ATYPIPv4:
		if len(buf) < offset+5 {
			return netip.Addr{}, 0, errors.New("short SOCKS5 UDP datagram")
		}
		return netip.AddrFrom4([4]byte(buf[offset+1 : offset+5])), offset + 5, nil
	case socks5ATYPIPv6:
		if len(buf) < offset+17 {
			return netip.Addr{}, 0, errors.New("short SOCKS5 UDP datagram")
		}
		return netip.AddrFrom16([16]byte(buf[offset+1 : offset+17])), offset + 17, nil
	case socks5ATYPDomain:
		// A datagram source must be an address; resolving a domain here would
		// block the WireGuard reader, and WARP peers always answer with an IP.
		return netip.Addr{}, 0, errors.New("SOCKS5 UDP relay returned a domain source address")
	default:
		return netip.Addr{}, 0, fmt.Errorf("SOCKS5 UDP datagram used unknown address type %d", buf[offset])
	}
}

func socks5ReplyText(code byte) string {
	texts := map[byte]string{
		0x01: "general proxy failure",
		0x02: "connection not allowed by ruleset",
		0x03: "network unreachable",
		0x04: "host unreachable",
		0x05: "connection refused",
		0x06: "TTL expired",
		0x07: "command not supported (the proxy does not allow UDP ASSOCIATE)",
		0x08: "address type not supported",
	}
	if text, ok := texts[code]; ok {
		return text
	}
	return fmt.Sprintf("unknown reply code %d", code)
}
