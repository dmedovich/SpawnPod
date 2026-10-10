package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartFailureDoesNotExposeRequest(t *testing.T) {
	socket, _ := startServer(t)
	const canary = "credential-canary"
	for _, req := range []Request{
		{Op: "run", ID: "missing", Argv: []string{"/nonexistent/" + canary}, TimeoutMillis: 1000},
		{Op: "run", ID: "cwd", Argv: []string{"sh"}, Dir: "/nonexistent/" + canary, TimeoutMillis: 1000},
	} {
		err := Run(context.Background(), socket, req, func(ev Event) error {
			if strings.Contains(ev.Error, canary) || strings.Contains(string(ev.Data), canary) {
				t.Error("request leaked in diagnostic")
			}
			return nil
		})
		if err == nil || strings.Contains(err.Error(), canary) {
			t.Fatalf("unsafe start error: %v", err)
		}
	}
}

func TestGuardPreventsSessionAndProcessGroupEscape(t *testing.T) {
	socket, _ := startServer(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []string{"setsid", "setpgid", "clone-newuser", "clone-newpid", "clone3", "unshare", "setns", "ptrace", "writev"} {
		var exit *int
		err := Run(context.Background(), socket, Request{Op: "run", ID: probe, Argv: []string{exe, "guard-fork-probe", probe}, TimeoutMillis: 2000}, func(ev Event) error {
			if ev.ExitCode != nil {
				exit = ev.ExitCode
			}
			return nil
		})
		if err != nil || exit == nil || *exit != 0 {
			t.Fatalf("%s: exit=%v err=%v", probe, exit, err)
		}
	}
}

func TestSuccessfulStepKillsInheritedBackgroundChild(t *testing.T) {
	socket, _ := startServer(t)
	marker := filepath.Join(t.TempDir(), "pid")
	err := Run(context.Background(), socket, Request{Op: "run", ID: "background", Env: map[string]string{"MARKER": marker}, Argv: []string{"sh", "-c", `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$MARKER"`}, TimeoutMillis: 2000}, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		// A killed, unreaped child is harmless; this process is not its parent.
		end := strings.LastIndex(string(data), ")")
		if os.IsNotExist(err) || (end >= 0 && strings.HasPrefix(string(data)[end+1:], " Z")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background child still running")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStalledReaderReleasesSlotAndAllowsNextCommand(t *testing.T) {
	socket, server := startServer(t)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Op: "run", ID: "blocked", Argv: []string{"sh", "-c", "head -c 4194304 /dev/zero"}, TimeoutMillis: 50}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	seenActive := false
	for {
		server.mu.Lock()
		active := server.activeID
		server.mu.Unlock()
		if active != "" {
			seenActive = true
		}
		if seenActive && active == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("non-reading peer prevented bounded cleanup")
		}
		time.Sleep(time.Millisecond)
	}
	var exit *int
	err = Run(context.Background(), socket, Request{Op: "run", ID: "next", Argv: []string{"sh", "-c", "exit 0"}, TimeoutMillis: 1000}, func(ev Event) error { exit = ev.ExitCode; return nil })
	if err != nil || exit == nil || *exit != 0 {
		t.Fatalf("next command: exit=%v err=%v", exit, err)
	}
}

func TestExitCancellationRaceHasOneTerminalEvent(t *testing.T) {
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan struct{})
		go func(delay time.Duration) {
			select {
			case <-finished:
			case <-time.After(delay):
				cancel()
			}
		}(time.Duration(i%5) * time.Millisecond)
		terminals := 0
		(&Server{TermGrace: 100 * time.Millisecond}).run(ctx, Request{Argv: []string{"sh", "-c", "exit 0"}}, func(ev Event) error {
			if ev.Error != "" || ev.ExitCode != nil {
				terminals++
			}
			return nil
		})
		close(finished)
		cancel()
		if terminals != 1 {
			t.Fatalf("iteration %d: terminal events=%d", i, terminals)
		}
	}
}

func TestRequestTrailingWhitespaceDoesNotCancelCommand(t *testing.T) {
	socket, _ := startServer(t)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := Request{Op: "run", ID: "framing", Argv: []string{"sh", "-c", "sleep 0.02; exit 0"}, TimeoutMillis: 1000}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	// Deliver whitespace separately so it cannot be consumed in the first Decode.
	time.Sleep(5 * time.Millisecond)
	if _, err := conn.Write([]byte("\n \t\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var event Event
	if err := json.NewDecoder(conn).Decode(&event); err != nil || event.ExitCode == nil || *event.ExitCode != 0 {
		t.Fatalf("framing cancelled command: %+v err=%v", event, err)
	}
}

func TestShutdownWaitsForActiveProcessCleanup(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	server := &Server{Socket: socket, MaxTimeout: 5 * time.Second, TermGrace: 100 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(time.Millisecond)
	}
	marker := filepath.Join(t.TempDir(), "pid")
	result := make(chan error, 1)
	go func() {
		result <- Run(context.Background(), socket, Request{Op: "run", ID: "shutdown", Env: map[string]string{"MARKER": marker}, Argv: []string{"sh", "-c", `echo $$ > "$MARKER"; exec sleep 30`}, TimeoutMillis: 4000}, func(Event) error { return nil })
	}()
	var data []byte
	for {
		var err error
		data, err = os.ReadFile(marker)
		if err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not start")
		}
		time.Sleep(time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Serve returned before command was reaped: %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("shutdown command succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("client blocked after shutdown")
	}
}
