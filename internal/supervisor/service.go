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

// ServiceServer starts one long-running process after receiving its command
// and environment over the private socket. The process is independent of the
// start request connection and exits with this container's PID 1.
type ServiceServer struct {
	Socket    string
	TermGrace time.Duration
	mu        sync.Mutex
	started   bool
	exitDone  chan struct{}
	exitErr   error
}

func (s *ServiceServer) Serve(ctx context.Context) error {
	if s == nil || s.Socket == "" || s.TermGrace <= 0 {
		return errors.New("invalid service supervisor configuration")
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0700); err != nil {
		return err
	}
	if err := os.Remove(s.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(s.Socket)
	if err := os.Chmod(s.Socket, 0600); err != nil {
		return err
	}
	s.exitDone = make(chan struct{})
	stopAccept := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopAccept:
		}
	}()
	defer close(stopAccept)
	go func() {
		select {
		case <-s.exitDone:
			_ = listener.Close()
		case <-stopAccept:
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.mu.Lock()
				started := s.started
				s.mu.Unlock()
				if started {
					select {
					case <-s.exitDone:
					case <-time.After(2 * s.TermGrace):
					}
				}
				return nil
			}
			select {
			case <-s.exitDone:
				s.mu.Lock()
				processErr := s.exitErr
				s.mu.Unlock()
				return fmt.Errorf("service process exited: %w", processErr)
			default:
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *ServiceServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(Event{Error: "invalid service request"})
		return
	}
	if req.Op == "ping" {
		_ = json.NewEncoder(conn).Encode(Event{ExitCode: intPtr(0)})
		return
	}
	if err := validateServiceStart(req); err != nil {
		_ = json.NewEncoder(conn).Encode(Event{Error: err.Error()})
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		_ = json.NewEncoder(conn).Encode(Event{Error: "service already started"})
		return
	}
	if ctx.Err() != nil {
		s.mu.Unlock()
		_ = json.NewEncoder(conn).Encode(Event{Error: "service supervisor is stopping"})
		return
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = "/"
	cmd.Env = mergeEnv(os.Environ(), req.Env)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		_ = json.NewEncoder(conn).Encode(Event{Error: "service process failed to start"})
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.monitor(ctx, cmd, time.Duration(req.TimeoutMillis)*time.Millisecond)
	_ = json.NewEncoder(conn).Encode(Event{ExitCode: intPtr(0)})
}

func validateServiceStart(req Request) error {
	if req.Op != "start_service" || req.ID == "" || len(req.Argv) == 0 || req.Argv[0] == "" {
		return errors.New("invalid service start request")
	}
	if req.TimeoutMillis <= 0 || req.TimeoutMillis > (24*time.Hour).Milliseconds() {
		return errors.New("service lifetime is outside policy")
	}
	if len(req.Argv) > 128 {
		return errors.New("service command is too large")
	}
	for _, arg := range req.Argv {
		if len(arg) > 4096 || containsNUL(arg) {
			return errors.New("invalid service command argument")
		}
	}
	for key, value := range req.Env {
		if key == "" || len(key) > 255 || containsNUL(key) || containsNUL(value) {
			return errors.New("invalid service environment")
		}
		for _, c := range key {
			if c == '=' {
				return errors.New("invalid service environment")
			}
		}
	}
	return nil
}

func containsNUL(value string) bool {
	for _, c := range value {
		if c == 0 {
			return true
		}
	}
	return false
}

func (s *ServiceServer) monitor(parent context.Context, cmd *exec.Cmd, lifetime time.Duration) {
	ctx, cancel := context.WithTimeout(parent, lifetime)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == nil {
			err = errors.New("service stopped")
		}
		s.finish(err)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(s.TermGrace):
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		s.finish(ctx.Err())
	}
}

func (s *ServiceServer) finish(err error) {
	s.mu.Lock()
	s.exitErr = err
	close(s.exitDone)
	s.mu.Unlock()
}
