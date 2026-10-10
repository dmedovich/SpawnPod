package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func startServer(t *testing.T) (string, *Server) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	server := &Server{Socket: socket, MaxTimeout: 5 * time.Second, TermGrace: 100 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("server did not stop")
		}
	})
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not created")
		}
		time.Sleep(time.Millisecond)
	}
	return socket, server
}

func TestRunStreamsAndExitCode(t *testing.T) {
	socket, _ := startServer(t)
	var mu sync.Mutex
	var stdout, stderr string
	var exit *int
	err := Run(context.Background(), socket, Request{Op: "run", ID: "one", Argv: []string{"sh", "-c", "printf out; printf err >&2; exit 7"}, TimeoutMillis: 1000}, func(ev Event) error {
		mu.Lock()
		defer mu.Unlock()
		if ev.Stream == "stdout" {
			stdout += string(ev.Data)
		}
		if ev.Stream == "stderr" {
			stderr += string(ev.Data)
		}
		if ev.ExitCode != nil {
			exit = ev.ExitCode
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "out" || stderr != "err" || exit == nil || *exit != 7 {
		t.Fatalf("stdout=%q stderr=%q exit=%v", stdout, stderr, exit)
	}
}

func TestCancelStopsSilentCommand(t *testing.T) {
	socket, _ := startServer(t)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), socket, Request{Op: "run", ID: "long", Argv: []string{"sh", "-c", "sleep 10"}, TimeoutMillis: 4000}, func(Event) error { return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		err := Run(context.Background(), socket, Request{Op: "cancel", ID: "long"}, func(Event) error { return nil })
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel never found command: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled command reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("command did not stop")
	}
}

func TestTimeout(t *testing.T) {
	socket, _ := startServer(t)
	started := time.Now()
	err := Run(context.Background(), socket, Request{Op: "run", ID: "timeout", Argv: []string{"sh", "-c", "sleep 10"}, TimeoutMillis: 30}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("timeout reported success")
	}
	if time.Since(started) > time.Second {
		t.Fatal("timeout was not enforced")
	}
}

func TestDisconnectCancels(t *testing.T) {
	socket, _ := startServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	started := make(chan struct{})
	var once sync.Once
	go func() {
		done <- Run(ctx, socket, Request{Op: "run", ID: "disconnect", Argv: []string{"sh", "-c", "printf ready; sleep 10"}, TimeoutMillis: 4000}, func(ev Event) error {
			if ev.Stream == "stdout" && string(ev.Data) == "ready" {
				once.Do(func() { close(started) })
			}
			return nil
		})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("command ended before start: %v", err)
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("disconnect: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := Run(context.Background(), socket, Request{Op: "run", ID: "after", Argv: []string{"true"}, TimeoutMillis: 1000}, func(Event) error { return nil })
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot remained busy: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}
