package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceProcessSurvivesStartConnection(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "control.sock")
	output := filepath.Join(root, "received")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&ServiceServer{Socket: socket, TermGrace: 100 * time.Millisecond}).Serve(ctx)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("service server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("service server did not stop")
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service socket was not created")
		}
		time.Sleep(time.Millisecond)
	}
	request := Request{Op: "start_service", ID: "service", Argv: []string{"sh", "-c", `printf %s "$SERVICE_TOKEN" > "$OUTPUT_FILE"; sleep 10`}, Env: map[string]string{"SERVICE_TOKEN": "private-value", "OUTPUT_FILE": output}, TimeoutMillis: 5000}
	if err := Run(context.Background(), socket, request, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for {
		content, err := os.ReadFile(output)
		if err == nil && string(content) == "private-value" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service process did not receive its environment: %q, %v", content, err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := Run(context.Background(), socket, Request{Op: "ping"}, func(Event) error { return nil }); err != nil {
		t.Fatal("supervisor stopped when start connection closed:", err)
	}
	if err := Run(context.Background(), socket, request, func(Event) error { return nil }); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("duplicate service start was not rejected: %v", err)
	}
}

func TestServiceServerStopsWhenChildExits(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&ServiceServer{Socket: socket, TermGrace: 100 * time.Millisecond}).Serve(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service socket was not created")
		}
		time.Sleep(time.Millisecond)
	}
	request := Request{Op: "start_service", ID: "service", Argv: []string{"sh", "-c", "exit 0"}, TimeoutMillis: 1000}
	if err := Run(context.Background(), socket, request, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "service process exited") {
			t.Fatalf("unexpected supervisor exit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service supervisor stayed alive after child exit")
	}
}
