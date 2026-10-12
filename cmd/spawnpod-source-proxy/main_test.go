package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProxyProcess(t *testing.T) {
	path := os.Getenv("SPAWNPOD_PROXY_TEST_CONFIG")
	if path == "" {
		return
	}
	if err := run([]string{"--config", path}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSIGTERMClosesActiveCONNECTAndExits(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	path := filepath.Join(t.TempDir(), "proxy.yaml")
	content := "listen: " + address + "\nmaxConnections: 2\nroutes:\n  - host: git.example.com\n    port: 443\n    address: 1.1.1.1\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProxyProcess$")
	command.Env = append(os.Environ(), "SPAWNPOD_PROXY_TEST_CONFIG="+path)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var processErr error
	go func() { processErr = command.Wait(); close(done) }()
	defer func() { cancel(); <-done }()
	var connection net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, err = net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("proxy exited before listening: %v", processErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = fmt.Fprint(connection, "CONNECT git.example.com:443 HTTP/1.1\r\nHost: git.example.com:443\r\n\r\n")
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT did not start: %v", err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if processErr != nil {
			t.Fatalf("SIGTERM exit: %v", processErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy failed to exit on SIGTERM")
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("tunnel remained open after shutdown")
	}
}

func TestValidateStrictConfigurationWithoutListening(t *testing.T) {
	valid := "listen: 127.0.0.1:3128\nmaxConnections: 8\nroutes:\n  - host: git.example.com\n    port: 443\n    address: 1.1.1.1\n"
	for name, content := range map[string]string{
		"valid":             valid,
		"unknown field":     valid + "credentials: secret\n",
		"trailing document": valid + "---\nlisten: 127.0.0.1:1234\n",
		"duplicate field":   valid + "listen: 127.0.0.1:1234\n",
		"DNS listener":      strings.Replace(valid, "127.0.0.1", "localhost", 1),
		"zero port":         strings.Replace(valid, ":3128", ":0", 1),
		"named port":        strings.Replace(valid, ":3128", ":http", 1),
		"negative limit":    strings.Replace(valid, "maxConnections: 8", "maxConnections: -1", 1),
		"excessive limit":   strings.Replace(valid, "maxConnections: 8", "maxConnections: 4097", 1),
		"private upstream":  strings.Replace(valid, "address: 1.1.1.1", "address: 10.0.0.1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxy.yaml")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			err := run([]string{"--config", path, "--validate"})
			if (err == nil) != (name == "valid") {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}
