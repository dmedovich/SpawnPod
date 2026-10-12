package sourceproxy

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRejectedClientHellosNeverOpenUpstream(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	_, port, _ := net.SplitHostPort(upstream.Addr().String())
	var number int
	_, _ = fmt.Sscan(port, &number)
	p, err := New([]Route{{Host: "git.example.com", Port: number, Address: "127.0.0.1", AllowLoopback: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	server := httptest.NewServer(p)
	defer server.Close()
	hello := helloWithExtensions(sniExtension("git.example.com"))
	noSNI := helloWithExtensions(nil)
	noSNIRecord := binary.BigEndian.AppendUint16([]byte{22, 3, 1}, uint16(len(noSNI)))
	noSNIRecord = append(noSNIRecord, noSNI...)
	for name, firstRecord := range map[string][]byte{
		"absent SNI":                noSNIRecord,
		"non TLS":                   []byte("GET / HTTP/1.1\r\n\r\n"),
		"oversized":                 {22, 3, 1, 0x40, 1},
		"short":                     {22, 3, 1, 0, 3},
		"unexpected record version": {22, 3, 255, 0, 4},
		"fragmented handshake":      append([]byte{22, 3, 1, 0, 4}, hello[:4]...),
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			_, _ = fmt.Fprintf(c, "CONNECT git.example.com:%s HTTP/1.1\r\nHost: git.example.com:%s\r\n\r\n", port, port)
			reader := bufio.NewReader(c)
			response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("CONNECT failed: %v", err)
			}
			_, _ = c.Write(firstRecord)
			if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("malformed TLS tunnel remained open: %v", err)
			}
		})
	}
	_ = upstream.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if c, err := upstream.Accept(); err == nil {
		_ = c.Close()
		t.Fatal("malformed ClientHello opened upstream socket")
	} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatal(err)
	}
}

func TestRouteLimitsAndAddressCategories(t *testing.T) {
	route := Route{Host: "git.example.com", Port: 443, Address: "1.1.1.1"}
	for _, routes := range [][]Route{nil, {route, route}, make([]Route, 65)} {
		if _, err := New(routes); err == nil {
			t.Fatal("invalid route count/duplicates accepted")
		}
	}
	for _, address := range []string{"", "upstream.example.com", "0.0.0.0", "192.168.1.1", "172.16.0.1", "224.0.0.1", "240.0.0.1", "198.18.0.1", "192.88.99.1", "::", "ff02::1", "fe80::1", "fec0::1", "2001:db8::1", "::ffff:1.1.1.1", "fe80::1%eth0", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1", "100:0:0:1::1", "2001:2::1", "2001:10::1", "2001:20::1", "2002:a00:1::1", "3fff::1", "5f00::1", "4000::1"} {
		route.Address = address
		route.AllowLoopback = true
		if _, err := New([]Route{route}); err == nil {
			t.Errorf("accepted special destination %q", address)
		}
	}
	for _, address := range []string{"138.124.231.231", "1.1.1.1", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		route.Address = address
		p, err := New([]Route{route})
		if err != nil {
			t.Fatalf("ordinary public address rejected %s: %v", address, err)
		}
		_ = p.Close()
	}
	for _, limit := range []int{-1, 0, 4097} {
		if _, err := NewWithLimit([]Route{{Host: "git.example.com", Port: 443, Address: "1.1.1.1"}}, limit); err == nil {
			t.Fatalf("invalid connection limit %d", limit)
		}
	}
}

func TestPendingClientHelloTimesOutWithoutUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises the real ten-second ClientHello deadline")
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	port := upstream.Addr().(*net.TCPAddr).Port
	p, err := New([]Route{{Host: "git.example.com", Port: port, Address: "127.0.0.1", AllowLoopback: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	server := httptest.NewServer(p)
	defer server.Close()
	client, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	_ = client.SetDeadline(time.Now().Add(13 * time.Second))
	_, _ = fmt.Fprintf(client, "CONNECT git.example.com:%d HTTP/1.1\r\nHost: git.example.com:%d\r\n\r\n", port, port)
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response: %v", err)
	}
	started := time.Now()
	// A partial record must not hold a tunnel forever or open the upstream.
	_, _ = client.Write([]byte{22, 3})
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("pending tunnel did not close at its server deadline: %v", err)
	}
	if time.Since(started) < 9*time.Second {
		t.Fatal("tunnel closed before exercising the ClientHello deadline")
	}
	_ = upstream.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if c, err := upstream.Accept(); err == nil {
		_ = c.Close()
		t.Fatal("incomplete ClientHello opened upstream")
	} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatal(err)
	}
}

func TestRealHTTPSWithCertificateVerification(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "scoped-test-token" {
			t.Error("TLS request changed")
		}
		_, _ = io.WriteString(w, "tls-approved")
	}))
	defer upstream.Close()
	cert := upstream.Certificate()
	if len(cert.DNSNames) == 0 {
		t.Fatal("TLS fixture lacks DNS SAN")
	}
	host := cert.DNSNames[0]
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	var number int
	for _, digit := range port {
		number = number*10 + int(digit-'0')
	}
	p, err := New([]Route{{Host: host, Port: number, Address: "127.0.0.1", AllowLoopback: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	server := httptest.NewServer(p)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: upstream.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	req, _ := http.NewRequest(http.MethodPut, "https://"+net.JoinHostPort(host, port)+"/v2/test", strings.NewReader("test artifact"))
	req.Header.Set("Authorization", "scoped-test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(data) != "tls-approved" {
		t.Fatalf("HTTPS result: %d %q %v", resp.StatusCode, data, err)
	}
}

func TestHopHeadersAcrossMultipleConnectionLines(t *testing.T) {
	header := http.Header{"Connection": {"X-First", "X-Second, Keep-Alive"}, "X-First": {"private"}, "X-Second": {"private"}, "Proxy-Authorization": {"private"}, "Authorization": {"end-to-end"}}
	stripHopHeaders(header)
	if len(header) != 1 || header.Get("Authorization") != "end-to-end" {
		t.Fatalf("hop headers retained: %v", header)
	}
}

func TestOverloadAndClosedProxy(t *testing.T) {
	p, err := NewWithLimit([]Route{{Host: "git.example.com", Port: 443, Address: "1.1.1.1"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	p.slots <- struct{}{}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://git.example.com/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("overload: %d", w.Code)
	}
	<-p.slots
	client, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	if !p.track(client) {
		t.Fatal("track failed")
	}
	_ = p.Close()
	_ = p.Close()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Close left tunnel open: %v", err)
	}
	w = httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://git.example.com/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed proxy accepted request: %d", w.Code)
	}
}

// A minimal ordinary ClientHello permits deliberate malformed extension tails.
func helloWithExtensions(extensions []byte) []byte {
	body := append([]byte{3, 3}, make([]byte, 32)...)
	body = append(body, 0, 0, 2, 0x13, 1, 1, 0)
	body = binary.BigEndian.AppendUint16(body, uint16(len(extensions)))
	body = append(body, extensions...)
	hello := []byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(hello, body...)
}

func sniExtension(name string) []byte {
	value := binary.BigEndian.AppendUint16(nil, uint16(len(name)+3))
	value = append(value, 0)
	value = binary.BigEndian.AppendUint16(value, uint16(len(name)))
	value = append(value, name...)
	extension := binary.BigEndian.AppendUint16(nil, 0)
	extension = binary.BigEndian.AppendUint16(extension, uint16(len(value)))
	return append(extension, value...)
}

func TestMalformedClientHelloFailsClosed(t *testing.T) {
	sni := sniExtension("git.example.com")
	for name, extensions := range map[string][]byte{
		"missing SNI":                nil,
		"duplicate SNI":              append(append([]byte{}, sni...), sni...),
		"trailing partial extension": append(append([]byte{}, sni...), 0),
		"trailing truncated payload": append(append([]byte{}, sni...), 0, 1, 0, 4, 0),
		"ECH":                        append(append([]byte{}, sni...), 0xfe, 0x0d, 0, 0),
		"uppercase SNI":              sniExtension("GIT.example.com"),
		"empty SNI":                  sniExtension(""),
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := clientHelloSNI(helloWithExtensions(extensions)); ok {
				t.Fatalf("accepted malformed hello: %q", got)
			}
		})
	}
	hello := helloWithExtensions(sni)
	if name, ok := clientHelloSNI(hello); !ok || name != "git.example.com" {
		t.Fatal("valid ClientHello rejected")
	}
	for length := 0; length < len(hello); length++ {
		if _, ok := clientHelloSNI(hello[:length]); ok {
			t.Fatalf("accepted truncated ClientHello at %d", length)
		}
	}
}

func FuzzClientHelloSNI(f *testing.F) {
	f.Add(helloWithExtensions(sniExtension("git.example.com")))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		name, ok := clientHelloSNI(data)
		if ok && (!validHost(name) || len(data) < 4 || int(data[1])<<16|int(data[2])<<8|int(data[3]) != len(data)-4) {
			t.Fatal("accepted invalid handshake/name")
		}
	})
}
