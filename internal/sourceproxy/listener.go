package sourceproxy

import (
	"errors"
	"net"
	"sync"
)

// LimitListener bounds all accepted sockets, including idle HTTP connections
// and hijacked CONNECT tunnels. Close also unblocks an Accept waiting for space.
func LimitListener(listener net.Listener, limit int) (net.Listener, error) {
	if limit < 1 || limit > 4096 {
		return nil, errors.New("source proxy maxConnections must be between 1 and 4096")
	}
	return &limitedListener{Listener: listener, slots: make(chan struct{}, limit), done: make(chan struct{})}, nil
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	connection, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitedConnection{Conn: connection, release: func() { <-l.slots }}, nil
}

func (l *limitedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitedConnection struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
