package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Server accepts commands on a Unix socket. It owns no cluster credentials.
type Server struct {
	Socket     string
	MaxTimeout time.Duration
	TermGrace  time.Duration
	mu         sync.Mutex
	activeID   string
	cancel     context.CancelFunc
}

// Serve listens until ctx is cancelled. The caller must mount Socket's parent
// directory in an environment-private volume and set its permissions.
func (s *Server) Serve(ctx context.Context) error {
	if s.Socket == "" || s.MaxTimeout <= 0 || s.TermGrace <= 0 {
		return errors.New("invalid supervisor configuration")
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0700); err != nil {
		return err
	}
	if err := os.Remove(s.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(s.Socket)
	if err := os.Chmod(s.Socket, 0600); err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(parent context.Context, conn net.Conn) {
	defer conn.Close()
	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(Event{Error: "invalid request"})
		return
	}
	if err := req.validate(s.MaxTimeout); err != nil {
		_ = json.NewEncoder(conn).Encode(Event{Error: err.Error()})
		return
	}
	if req.Op == "ping" {
		_ = json.NewEncoder(conn).Encode(Event{ExitCode: intPtr(0)})
		return
	}
	if req.Op == "cancel" {
		s.mu.Lock()
		if s.activeID != req.ID {
			s.mu.Unlock()
			_ = json.NewEncoder(conn).Encode(Event{Error: "command not active"})
			return
		}
		cancel := s.cancel
		s.mu.Unlock()
		cancel()
		_ = json.NewEncoder(conn).Encode(Event{ExitCode: intPtr(0)})
		return
	}

	ctx, cancel := context.WithTimeout(parent, time.Duration(req.TimeoutMillis)*time.Millisecond)
	s.mu.Lock()
	if s.activeID != "" {
		s.mu.Unlock()
		cancel()
		_ = json.NewEncoder(conn).Encode(Event{Error: "environment busy"})
		return
	}
	s.activeID, s.cancel = req.ID, cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.activeID, s.cancel = "", nil; s.mu.Unlock(); cancel() }()

	// A closed exec transport must cancel the process even when it emits no output.
	go func() { var b [1]byte; _, _ = conn.Read(b[:]); cancel() }()

	var writeMu sync.Mutex
	emit := func(ev Event) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := json.NewEncoder(conn).Encode(ev); err != nil {
			cancel()
			return err
		}
		return nil
	}
	s.run(ctx, req, emit)
}

type eventWriter struct {
	stream string
	emit   func(Event) error
}

func (w eventWriter) Write(p []byte) (int, error) {
	data := append([]byte(nil), p...)
	if err := w.emit(Event{Stream: w.stream, Data: data}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Server) run(ctx context.Context, req Request, emit func(Event) error) {
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = mergeEnv(os.Environ(), req.Env)
	cmd.Stdout = eventWriter{"stdout", emit}
	cmd.Stderr = eventWriter{"stderr", emit}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = s.TermGrace
	if err := cmd.Start(); err != nil {
		_ = emit(Event{Error: fmt.Sprintf("start command: %v", err)})
		return
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = emit(Event{Error: fmt.Sprintf("command cancelled: %v", ctx.Err())})
		} else {
			s.finish(cmd, err, emit)
		}
	case <-ctx.Done():
		// Kill the group, not only the original process. A process may have forked.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(s.TermGrace)
		select {
		case <-done:
			// Children can still hold inherited pipes or continue after the parent.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(s.TermGrace):
			}
		}
		timer.Stop()
		_ = emit(Event{Error: fmt.Sprintf("command cancelled: %v", ctx.Err())})
	}
}

func (s *Server) finish(cmd *exec.Cmd, err error, emit func(Event) error) {
	// A completed step must not leave children in its original process group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil {
		_ = emit(Event{ExitCode: intPtr(0)})
		return
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = emit(Event{Error: "command left descendant processes running"})
		return
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		_ = emit(Event{ExitCode: &code})
		return
	}
	_ = emit(Event{Error: fmt.Sprintf("wait command: %v", err)})
}

func intPtr(n int) *int { return &n }

func mergeEnv(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				values[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	for k, v := range overrides {
		values[k] = v
	}
	result := make([]string, 0, len(values))
	for k, v := range values {
		result = append(result, k+"="+v)
	}
	return result
}
