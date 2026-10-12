// Package sourceproxy implements an operator-configured HTTP forward proxy.
// Every destination is an exact host and port with a pinned dial address.
package sourceproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Route struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	Address       string `yaml:"address"`
	AllowLoopback bool   `yaml:"allowLoopback"`
}

type Proxy struct {
	routes      map[string]string
	slots       chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]struct{}
}

func New(routes []Route) (*Proxy, error) {
	return NewWithLimit(routes, 128)
}

// NewWithLimit bounds simultaneous HTTP requests and CONNECT tunnels. Idle TCP
// connections are separately bounded by the executable's listener.
func NewWithLimit(routes []Route, maxConnections int) (*Proxy, error) {
	if maxConnections < 1 || maxConnections > 4096 {
		return nil, errors.New("source proxy maxConnections must be between 1 and 4096")
	}
	if len(routes) == 0 || len(routes) > 64 {
		return nil, errors.New("source proxy requires 1–64 routes")
	}
	p := &Proxy{routes: make(map[string]string, len(routes)), slots: make(chan struct{}, maxConnections), connections: make(map[net.Conn]struct{})}
	for _, route := range routes {
		if !validHost(route.Host) || route.Port < 1 || route.Port > 65535 {
			return nil, errors.New("invalid source proxy host or port")
		}
		ip, err := netip.ParseAddr(route.Address)
		allowedAddress := publicAddress(ip) || route.AllowLoopback && ip.IsLoopback()
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || !allowedAddress {
			return nil, fmt.Errorf("source proxy route %s: invalid or disallowed address", route.Host)
		}
		key := net.JoinHostPort(route.Host, strconv.Itoa(route.Port))
		if _, exists := p.routes[key]; exists {
			return nil, fmt.Errorf("duplicate source proxy route %s", key)
		}
		p.routes[key] = net.JoinHostPort(ip.String(), strconv.Itoa(route.Port))
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	return p, nil
}

// Close cancels HTTP requests and closes hijacked tunnels, which http.Server
// does not close itself. It is safe to call concurrently and more than once.
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cancel()
	for connection := range p.connections {
		_ = connection.Close()
	}
	return nil
}

func (p *Proxy) track(connection net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = connection.Close()
		return false
	}
	p.connections[connection] = struct{}{}
	return true
}

func (p *Proxy) untrack(connection net.Conn) {
	_ = connection.Close()
	p.mu.Lock()
	delete(p.connections, connection)
	p.mu.Unlock()
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 || host != strings.ToLower(host) || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if c < 'a' || c > 'z' {
				if c < '0' || c > '9' {
					if c != '-' {
						return false
					}
				}
			}
		}
	}
	return true
}

func publicAddress(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Is4In6() {
		return false
	}
	// IsGlobalUnicast also includes documentation, translation and reserved
	// prefixes. Accept ordinary IPv6 global unicast only, excluding special
	// protocol/tunnel allocations; never translate a pin to an embedded IPv4.
	// Review against IANA's IPv4/IPv6 Special-Purpose Address Registries.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(block).Contains(ip) {
			return false
		}
	}
	return true
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p == nil || r.Host == "" || r.URL == nil || r.URL.User != nil {
		http.Error(w, "invalid proxy request", http.StatusBadRequest)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "source proxy busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	if p.ctx.Err() != nil {
		http.Error(w, "source proxy unavailable", http.StatusServiceUnavailable)
		return
	}
	r = r.WithContext(ctx)
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" || r.URL.Host != r.Host || r.URL.Opaque != "" || r.URL.Fragment != "" {
		http.Error(w, "absolute HTTP URL required", http.StatusBadRequest)
		return
	}
	deadline := time.Now().Add(30 * time.Minute)
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	address, ok := p.lookup(r.URL.Host, "80")
	if !ok {
		http.Error(w, "source host denied", http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = r.URL.Host
	out.Header = r.Header.Clone()
	stripHopHeaders(out.Header)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	}, ResponseHeaderTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "source upstream unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	stripHopHeaders(resp.Header)
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) lookup(authority, defaultPort string) (string, bool) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		if strings.Contains(authority, ":") {
			return "", false
		}
		host, port = authority, defaultPort
	}
	number, err := strconv.Atoi(port)
	if !validHost(host) || err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return "", false
	}
	address, ok := p.routes[net.JoinHostPort(host, port)]
	return address, ok
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, _, authorityErr := net.SplitHostPort(r.Host)
	if authorityErr != nil || r.URL.Host != r.Host || r.URL.Scheme != "" || r.URL.Opaque != "" || r.URL.Path != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	address, ok := p.lookup(r.Host, "443")
	if !ok {
		http.Error(w, "source host denied", http.StatusForbidden)
		return
	}
	client, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "tunnel unavailable", http.StatusInternalServerError)
		return
	}
	if !p.track(client) {
		return
	}
	defer p.untrack(client)
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	var header [5]byte
	if _, err := io.ReadFull(rw, header[:]); err != nil || header[0] != 22 || header[1] != 3 || header[2] < 1 || header[2] > 3 {
		return
	}
	length := int(binary.BigEndian.Uint16(header[3:5]))
	if length < 4 || length > 16384 {
		return
	}
	first := make([]byte, length)
	if _, err := io.ReadFull(rw, first); err != nil {
		return
	}
	if sni, ok := clientHelloSNI(first); !ok || sni != host {
		return
	}
	// No upstream socket is opened until the complete ClientHello proves SNI.
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(r.Context(), "tcp", address)
	if err != nil {
		return
	}
	if !p.track(upstream) {
		return
	}
	defer p.untrack(upstream)
	_ = client.SetDeadline(time.Now().Add(30 * time.Minute))
	_ = upstream.SetDeadline(time.Now().Add(30 * time.Minute))
	if _, err := upstream.Write(append(header[:], first...)); err != nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, rw)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	_, _ = io.Copy(client, upstream)
	// An upstream EOF/error must not leave the upload goroutine waiting until
	// the absolute tunnel deadline when the client keeps its write side open.
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

// clientHelloSNI accepts one complete, ordinary TLS ClientHello. Fragmented,
// encrypted or absent names fail closed because they cannot prove the route.
func clientHelloSNI(data []byte) (string, bool) {
	if len(data) < 4 || data[0] != 1 || int(data[1])<<16|int(data[2])<<8|int(data[3]) != len(data)-4 {
		return "", false
	}
	data = data[4:]
	if len(data) < 35 {
		return "", false
	}
	if data[0] != 3 || data[1] < 1 || data[1] > 3 {
		return "", false
	}
	data = data[34:] // client version and random
	skip := func(length int) bool {
		if len(data) < length {
			return false
		}
		data = data[length:]
		return true
	}
	if data[0] > 32 || !skip(1+int(data[0])) || len(data) < 2 {
		return "", false
	}
	cipherLength := int(binary.BigEndian.Uint16(data[:2]))
	if cipherLength == 0 || cipherLength%2 != 0 || !skip(2+cipherLength) || len(data) < 1 {
		return "", false
	}
	if data[0] != 1 || len(data) < 2 || data[1] != 0 || !skip(2) || len(data) < 2 {
		return "", false
	}
	extensionsLength := int(binary.BigEndian.Uint16(data[:2]))
	if !skip(2) || len(data) != extensionsLength {
		return "", false
	}
	name := ""
	seen := make(map[uint16]bool)
	for len(data) > 0 {
		if len(data) < 4 {
			return "", false
		}
		kind := binary.BigEndian.Uint16(data[:2])
		if seen[kind] {
			return "", false
		}
		seen[kind] = true
		length := int(binary.BigEndian.Uint16(data[2:4]))
		if !skip(4) || len(data) < length {
			return "", false
		}
		value := data[:length]
		data = data[length:]
		if kind == 0xfe0d { // Encrypted ClientHello cannot prove the inner name.
			return "", false
		}
		if kind != 0 {
			continue
		}
		if len(value) < 5 || int(binary.BigEndian.Uint16(value[:2])) != len(value)-2 || value[2] != 0 || int(binary.BigEndian.Uint16(value[3:5])) != len(value)-5 {
			return "", false
		}
		name = string(value[5:])
		if !validHost(name) {
			return "", false
		}
	}
	return name, name != ""
}

func stripHopHeaders(headers http.Header) {
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(name)
	}
}
