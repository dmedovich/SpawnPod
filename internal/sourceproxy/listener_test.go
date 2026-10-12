package sourceproxy

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestListenerBoundsSocketsAndCloseUnblocksAccept(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := LimitListener(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	client, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	accepted, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	result := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if c != nil {
			_ = c.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("Accept was not bounded: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	_ = l.Close()
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept on closed listener: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close left Accept blocked")
	}
	// Multiple closes of a hijacked connection must release only one slot.
	_ = accepted.Close()
	_ = accepted.Close()
}
