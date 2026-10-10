// spawnpod-agent is copied into each approved job Pod. Its serve mode is the
// container's main process; serve-service keeps a workflow service alive.
// Client, metadata and tar transfer modes are launched through pods/exec.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/supervisor"
	"github.com/dmedovich/spawnpod/internal/workspace"
)

var version = "dev"

const socket = "/tmp/spawnpod/control.sock"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agent operation failed")
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spawnpod-agent serve|serve-service|install|ping|service-ping|invoke|service-start|cancel|copy-in|copy-out|metadata")
	}
	switch args[0] {
	case "command-guard":
		if len(args) != 1 {
			return fmt.Errorf("invalid command guard invocation")
		}
		return supervisor.GuardCommand()
	case "version":
		if len(args) != 1 {
			return fmt.Errorf("usage: spawnpod-agent version")
		}
		fmt.Println(version)
		return nil
	case "serve":
		if err := workspace.EnsureLayout(workspace.Mount); err != nil {
			return fmt.Errorf("prepare workspace: %w", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return (&supervisor.Server{Socket: socket, MaxTimeout: 30 * time.Minute, TermGrace: 5 * time.Second}).Serve(ctx)
	case "serve-service":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return (&supervisor.ServiceServer{Socket: socket, TermGrace: 5 * time.Second}).Serve(ctx)
	case "install":
		if len(args) != 2 {
			return fmt.Errorf("usage: spawnpod-agent install DESTINATION")
		}
		if err := os.MkdirAll(filepath.Dir(args[1]), 0755); err != nil {
			return err
		}
		source, err := os.Open("/proc/self/exe")
		if err != nil {
			return err
		}
		defer source.Close()
		dest, err := os.OpenFile(args[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0555)
		if err != nil {
			return err
		}
		if _, err := io.Copy(dest, source); err != nil {
			_ = dest.Close()
			return err
		}
		return dest.Close()
	case "ping":
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		return supervisor.Run(ctx, socket, supervisor.Request{Op: "ping"}, func(supervisor.Event) error { return nil })
	case "service-ping":
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		return supervisor.Run(ctx, socket, supervisor.Request{Op: "ping"}, func(ev supervisor.Event) error { return json.NewEncoder(os.Stdout).Encode(ev) })
	case "invoke":
		var req supervisor.Request
		if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
			return fmt.Errorf("decode request: %w", err)
		}
		if req.Op == "run" {
			if err := workspace.EnsureDirectory(workspace.Mount, req.Dir); err != nil {
				return fmt.Errorf("prepare command workdir: %w", err)
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return supervisor.Run(ctx, socket, req, func(ev supervisor.Event) error { return json.NewEncoder(os.Stdout).Encode(ev) })
	case "service-start":
		var req supervisor.Request
		if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
			return fmt.Errorf("decode service request: %w", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return supervisor.Run(ctx, socket, req, func(ev supervisor.Event) error { return json.NewEncoder(os.Stdout).Encode(ev) })
	case "cancel":
		if len(args) != 2 {
			return fmt.Errorf("usage: spawnpod-agent cancel COMMAND_ID")
		}
		ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return supervisor.Run(ctx, socket, supervisor.Request{Op: "cancel", ID: args[1]}, func(ev supervisor.Event) error { return json.NewEncoder(os.Stdout).Encode(ev) })
	case "copy-in":
		if len(args) != 2 {
			return fmt.Errorf("usage: spawnpod-agent copy-in DEST")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return workspace.Extract(ctx, workspace.Mount, args[1], os.Stdin)
	case "copy-out":
		if len(args) != 2 {
			return fmt.Errorf("usage: spawnpod-agent copy-out SRC")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return workspace.Archive(ctx, workspace.Mount, args[1], os.Stdout)
	case "metadata":
		if len(args) != 1 {
			return fmt.Errorf("usage: spawnpod-agent metadata")
		}
		values := make(map[string]string)
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if ok && key != "HOSTNAME" && key != "PWD" && key != "SHLVL" && key != "_" {
				values[key] = value
			}
		}
		return json.NewEncoder(os.Stdout).Encode(engine.RuntimeMetadata{OS: "Linux", Arch: runtime.GOARCH, Environment: values})
	case "help":
		_, err := fmt.Fprintln(os.Stdout, "usage: spawnpod-agent serve|serve-service|install|ping|service-ping|invoke|service-start|cancel|copy-in|copy-out|metadata|version")
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
