package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dmedovich/spawnpod/internal/sourceproxy"
	"go.yaml.in/yaml/v3"
)

var version = "dev"

type configuration struct {
	Listen         string              `yaml:"listen"`
	Routes         []sourceproxy.Route `yaml:"routes"`
	MaxConnections int                 `yaml:"maxConnections"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	flags := flag.NewFlagSet("spawnpod-source-proxy", flag.ContinueOnError)
	path := flags.String("config", "", "operator-owned YAML configuration")
	validate := flags.Bool("validate", false, "validate configuration without listening")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("usage: spawnpod-source-proxy --config PATH [--validate]")
	}
	file, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var cfg configuration
	if err := decoder.Decode(&cfg); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("source proxy configuration has trailing data")
	}
	if cfg.Listen == "" {
		return errors.New("source proxy listen address is required")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || net.ParseIP(host) == nil {
		return errors.New("source proxy listen must use a literal IP and port")
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 || port != strconv.Itoa(number) {
		return errors.New("source proxy listen port must be between 1 and 65535")
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 128
	}
	proxy, err := sourceproxy.NewWithLimit(cfg.Routes, cfg.MaxConnections)
	if err != nil {
		return err
	}
	defer func() { _ = proxy.Close() }()
	if *validate {
		fmt.Println("source proxy configuration valid")
		return nil
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	listener, err = sourceproxy.LimitListener(listener, cfg.MaxConnections)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); _ = proxy.Close(); _ = server.Close() }()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
