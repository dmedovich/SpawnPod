package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Server accepts commands on a Unix socket. It owns no cluster credentials.
type Server struct {
	Socket     string
	MaxTimeout time.Duration
	TermGrace  time.Duration
	mu         sync.Mutex
	activeID   string
	cancel     context.CancelFunc
	poisoned   bool
}

// Serve listens until ctx is cancelled. The caller must mount Socket's parent
// directory in an environment-private volume and set its permissions.
func (s *Server) Serve(ctx context.Context) error {
	if s.Socket == "" || s.MaxTimeout <= 0 || s.TermGrace <= 0 {
		return errors.New("invalid supervisor configuration")
	}
	// Same-UID commands must not modify agent memory through /proc/<pid>/mem.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return errors.New("supervisor process protection unavailable")
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
	defer func() { _ = ln.Close() }()
	defer func() { _ = os.Remove(s.Socket) }()
	if err := os.Chmod(s.Socket, 0600); err != nil {
		return err
	}
	stopListener := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stopListener()
	var handlers sync.WaitGroup
	handlersCtx, stopHandlers := context.WithCancel(ctx)
	defer func() { stopHandlers(); handlers.Wait() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		handlers.Add(1)
		go func() { defer handlers.Done(); s.handle(handlersCtx, conn) }()
	}
}

func (s *Server) handle(parent context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	stopParent := context.AfterFunc(parent, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopParent()
	var req Request
	decoder := json.NewDecoder(io.LimitReader(conn, 1<<20))
	if err := decoder.Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(Event{Error: "invalid request"})
		return
	}
	if err := req.validate(s.MaxTimeout); err != nil {
		_ = json.NewEncoder(conn).Encode(Event{Error: err.Error()})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if req.Op == "ping" {
		s.mu.Lock()
		poisoned := s.poisoned
		s.mu.Unlock()
		if poisoned {
			_ = json.NewEncoder(conn).Encode(Event{Error: "environment unavailable"})
			return
		}
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
	if s.activeID != "" || s.poisoned {
		s.mu.Unlock()
		cancel()
		_ = json.NewEncoder(conn).Encode(Event{Error: "environment busy"})
		return
	}
	s.activeID, s.cancel = req.ID, cancel
	s.mu.Unlock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { s.mu.Lock(); s.activeID, s.cancel = "", nil; s.mu.Unlock() }) }
	defer release()
	defer cancel()

	// Cancellation unblocks writers even when the peer stays connected but
	// stops consuming output. Cleanup never waits for terminal-event delivery.
	stopWrites := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Now()) })
	defer stopWrites()
	// A closed exec transport must cancel the process even when it emits no output.
	go func() {
		defer cancel()
		reader := io.MultiReader(decoder.Buffered(), conn)
		var b [32]byte
		for {
			n, err := reader.Read(b[:])
			for _, c := range b[:n] {
				if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	var writeMu sync.Mutex
	emit := func(ev Event) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		// A terminal event promises that cleanup is complete. Release the slot
		// before publishing it, so the next request cannot observe stale busy state.
		if ev.ExitCode != nil || ev.Error != "" {
			release()
		}
		if err := ctx.Err(); err != nil && ev.Error == "" {
			return err
		}
		_ = conn.SetWriteDeadline(time.Now().Add(s.TermGrace))
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
	if ctx.Err() != nil {
		_ = emit(Event{Error: "command cancelled"})
		return
	}
	payload, err := json.Marshal(req)
	if err != nil {
		_ = emit(Event{Error: "invalid guarded command"})
		return
	}
	executable, err := os.Executable()
	if err != nil {
		_ = emit(Event{Error: "command isolation unavailable"})
		return
	}
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		_ = emit(Event{Error: "command isolation unavailable"})
		return
	}
	defer func() { _ = statusReader.Close() }()
	defer func() { _ = statusWriter.Close() }()
	cmd := exec.Command(executable, "command-guard")
	cmd.Dir = req.Dir
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = eventWriter{"stdout", emit}
	cmd.Stderr = eventWriter{"stderr", emit}
	cmd.ExtraFiles = []*os.File{statusWriter}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = s.TermGrace
	// Pdeathsig follows the creating OS thread, not merely the Go process.
	// Keep that thread alive until the command has been reaped.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		_ = emit(Event{Error: "command failed to start"})
		return
	}
	_ = statusWriter.Close()
	// WNOWAIT keeps the leader's PID reserved until after the final group kill.
	// Calling Cmd.Wait first would allow PID/process-group reuse before kill.
	exited := make(chan error, 1)
	go func() {
		var info unix.Siginfo
		var waitErr error
		for {
			waitErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(waitErr, unix.EINTR) {
				break
			}
		}
		exited <- waitErr
	}()
	var observeErr error
	select {
	case observeErr = <-exited:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case observeErr = <-exited:
		case <-time.After(s.TermGrace):
		}
	}
	if observeErr == nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	} else {
		// Unexpected wait failure cannot establish group ownership. Process.Kill
		// uses Go's process handle; poison the environment and require teardown.
		_ = cmd.Process.Kill()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(2 * s.TermGrace):
		s.mu.Lock()
		s.poisoned = true
		s.mu.Unlock()
		_ = emit(Event{Error: "command cleanup incomplete"})
		return
	}
	if observeErr != nil {
		s.mu.Lock()
		s.poisoned = true
		s.mu.Unlock()
		_ = emit(Event{Error: "command cleanup incomplete"})
		return
	}
	if ctx.Err() != nil {
		message := "command cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			message = "command deadline exceeded"
		}
		_ = emit(Event{Error: message})
		return
	}
	_ = statusReader.SetReadDeadline(time.Now().Add(s.TermGrace))
	startup, statusErr := io.ReadAll(io.LimitReader(statusReader, 128))
	if statusErr != nil {
		_ = emit(Event{Error: "command isolation unavailable"})
		return
	}
	if len(startup) != 0 {
		_ = emit(Event{Error: SafeFailure(string(startup))})
		return
	}
	s.finish(err, emit)
}

// SafeFailure never reflects arbitrary agent diagnostic text across a trust
// boundary. Workflow stdout/stderr are a separate, intentionally raw channel.
func SafeFailure(message string) string {
	switch message {
	case "command failed to start", "command isolation unavailable", "command cancelled", "command deadline exceeded", "command cleanup incomplete", "command left descendant processes running", "command wait failed":
		return message
	default:
		return "command execution failed"
	}
}

func (s *Server) finish(err error, emit func(Event) error) {
	if err == nil {
		_ = emit(Event{ExitCode: intPtr(0)})
		return
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		_ = emit(Event{Error: "command left descendant processes running"})
		return
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		_ = emit(Event{ExitCode: &code})
		return
	}
	_ = emit(Event{Error: "command wait failed"})
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
