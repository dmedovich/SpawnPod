//go:build ignore

// forgejo-tls is a loopback-only TLS reverse proxy for the local smoke.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:3443", "loopback listen address")
	cert := flag.String("cert", ".local/forgejo-tls/server.crt", "TLS certificate")
	key := flag.String("key", ".local/forgejo-tls/server.key", "TLS private key")
	flag.Parse()
	if flag.NArg() != 0 || *listen != "127.0.0.1:3443" {
		fmt.Fprintln(os.Stderr, "smoke TLS endpoint must listen on 127.0.0.1:3443")
		os.Exit(2)
	}
	target, _ := url.Parse("http://127.0.0.1:3000")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "Forgejo unavailable", http.StatusBadGateway)
	}
	server := &http.Server{Addr: *listen, Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServeTLS(*cert, *key); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
