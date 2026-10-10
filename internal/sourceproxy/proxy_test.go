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

func TestRoutesRejectUnapprovedAndPrivateTargets(t *testing.T) {
	for _, host := range []string{"bad@host", "foo..bar", "-foo.bar", "FOO.bar"} {
		if _, err := New([]Route{{Host: host, Port: 443, Address: "1.1.1.1"}}); err == nil {
			t.Errorf("accepted invalid host %s", host)
		}
	}
	for _, address := range []string{"127.0.0.1", "10.42.0.1", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "192.0.2.1"} {
		if _, err := New([]Route{{Host: "git.example.com", Port: 443, Address: address}}); err == nil {
			t.Errorf("accepted private/special address %s", address)
		}
	}
	if _, err := New([]Route{{Host: "git.example.com", Port: 443, Address: "1.1.1.1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := New([]Route{{Host: "git.example.com", Port: 443, Address: "127.0.0.1", AllowLoopback: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := New([]Route{{Host: "git.example.com", Port: 443, Address: "169.254.169.254", AllowLoopback: true}}); err == nil {
		t.Fatal("metadata endpoint accepted through loopback exception")
	}
}

func TestHTTPExactRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "git.example.com:") || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy request headers leaked or Host changed")
		}
		_, _ = io.WriteString(w, "approved")
	}))
	defer upstream.Close()
	ip, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	number := 0
	_, _ = fmt.Sscan(port, &number)
	proxy, err := New([]Route{{Host: "git.example.com", Port: number, Address: ip, AllowLoopback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	for _, check := range []struct {
		url  string
		want int
	}{{fmt.Sprintf("http://git.example.com:%s/repo", port), 200}, {fmt.Sprintf("http://other.example.com:%s/repo", port), 403}, {"http://git.example.com:8081/repo", 403}} {
		resp, err := client.Get(check.url)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != check.want {
			t.Errorf("%s: got %d, want %d", check.url, resp.StatusCode, check.want)
		}
		_ = resp.Body.Close()
	}
}

func TestCONNECTExactRoute(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		connection, err := upstream.Accept()
		if err == nil {
			defer connection.Close()
			_, _ = connection.Write([]byte("tunnel"))
		}
	}()
	_, port, _ := net.SplitHostPort(upstream.Addr().String())
	number := 0
	_, _ = fmt.Sscan(port, &number)
	proxy, err := New([]Route{{Host: "git.example.com", Port: number, Address: "127.0.0.1", AllowLoopback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "CONNECT git.example.com:%s HTTP/1.1\r\nHost: git.example.com:%s\r\n\r\n", port, port)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("CONNECT returned %d", response.StatusCode)
	}
	_, _ = conn.Write(clientHello(t, "git.example.com"))
	bytes := make([]byte, 6)
	if _, err := io.ReadFull(reader, bytes); err != nil || string(bytes) != "tunnel" {
		t.Fatalf("tunnel data: %q, %v", bytes, err)
	}
}

func TestCONNECTRejectsDifferentSNI(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	upstreamResult := make(chan error, 1)
	go func() {
		connection, err := upstream.Accept()
		if err != nil {
			upstreamResult <- err
			return
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
		var byteRead [1]byte
		_, err = connection.Read(byteRead[:])
		upstreamResult <- err
	}()
	_, port, _ := net.SplitHostPort(upstream.Addr().String())
	var number int
	_, _ = fmt.Sscan(port, &number)
	proxy, err := New([]Route{{Host: "git.example.com", Port: number, Address: "127.0.0.1", AllowLoopback: true}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(4 * time.Second))
	_, _ = fmt.Fprintf(connection, "CONNECT git.example.com:%s HTTP/1.1\r\nHost: git.example.com:%s\r\n\r\n", port, port)
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT: %v, %v", response, err)
	}
	_, _ = connection.Write(clientHello(t, "other.example.com"))
	var one [1]byte
	if _, err := reader.Read(one[:]); err != io.EOF {
		t.Fatalf("client tunnel did not close: %v", err)
	}
	if err := <-upstreamResult; err != io.EOF {
		t.Fatalf("upstream received data or stayed open: %v", err)
	}
}

func TestClientHelloSNI(t *testing.T) {
	for _, name := range []string{"git.example.com", "other.example.com"} {
		hello := clientHello(t, name)
		got, ok := clientHelloSNI(hello[5:])
		if !ok || got != name {
			t.Fatalf("SNI got %q, %v, want %q", got, ok, name)
		}
	}
	if _, ok := clientHelloSNI([]byte{1, 0, 0, 0}); ok {
		t.Fatal("accepted empty ClientHello")
	}
}

func clientHello(t *testing.T, name string) []byte {
	t.Helper()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() {
		_ = tls.Client(client, &tls.Config{ServerName: name, MinVersion: tls.VersionTLS12}).Handshake()
	}()
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, 5)
	if _, err := io.ReadFull(peer, header); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
	if _, err := io.ReadFull(peer, data); err != nil {
		t.Fatal(err)
	}
	return append(header, data...)
}
