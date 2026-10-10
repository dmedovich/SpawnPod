package unixrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"github.com/dmedovich/spawnpod/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("socket directory must be private")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("socket directory must belong to controller UID")
	}
	return nil
}

func listen(path string) (net.Listener, func(), error) {
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || stat.Uid != uint32(os.Getuid()) {
			return nil, nil, errors.New("socket path exists and is not an owned socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, nil, errors.New("socket path already has an active server")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, nil, fmt.Errorf("cannot classify existing socket: %w", dialErr)
		}
		if err := os.Remove(path); err != nil {
			return nil, nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, nil, err
	}
	owned, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = listener.Close()
		current, err := os.Lstat(path)
		if err == nil && os.SameFile(owned, current) {
			_ = os.Remove(path)
		}
	}
	return listener, cleanup, nil
}

// Serve registers the plugin and health service while ctx holds the Lease.
// A cancelled context stops active streams and removes only our own socket.
type Options struct {
	Shutdown     context.Context
	DrainTimeout time.Duration
	Metrics      *telemetry.State
}

func Serve(ctx context.Context, path string, plugin pb.BackendPluginServer) error {
	return ServeWithOptions(ctx, path, plugin, Options{})
}

// Lease loss stops immediately. Operator shutdown drains existing RPCs while
// the caller continues renewing the Lease, then forces cancellation at timeout.
func ServeWithOptions(ctx context.Context, path string, plugin pb.BackendPluginServer, options Options) error {
	if plugin == nil {
		return errors.New("forgejo plugin is nil")
	}
	listener, cleanup, err := listen(path)
	if err != nil {
		return err
	}
	defer cleanup()
	var draining atomic.Bool
	serverOptions := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if draining.Load() && info.FullMethod == pb.BackendPlugin_Create_FullMethodName {
				return nil, status.Error(codes.Unavailable, "controller is draining")
			}
			return handler(ctx, req)
		}),
	}
	if options.Metrics != nil {
		serverOptions = append(serverOptions, grpc.ChainUnaryInterceptor(options.Metrics.Unary), grpc.ChainStreamInterceptor(options.Metrics.Stream))
	}
	serverOptions = append(serverOptions,
		grpc.MaxRecvMsgSize(32<<20), grpc.MaxSendMsgSize(32<<20),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 30 * time.Second}),
	)
	server := grpc.NewServer(serverOptions...)
	pb.RegisterBackendPluginServer(server, plugin)
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if options.Metrics != nil {
		options.Metrics.Serving(true)
		defer options.Metrics.Serving(false)
	}
	var shutdown <-chan struct{}
	if options.Shutdown != nil {
		shutdown = options.Shutdown.Done()
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
			server.Stop()
		case <-shutdown:
			draining.Store(true)
			if options.Metrics != nil {
				options.Metrics.Drain()
			}
			healthServer.Shutdown()
			graceful := make(chan struct{})
			go func() { server.GracefulStop(); close(graceful) }()
			timeout := options.DrainTimeout
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-graceful:
			case <-timer.C:
				server.Stop()
				<-graceful
			case <-ctx.Done():
				server.Stop()
				<-graceful
			}
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	if ctx.Err() != nil || options.Shutdown != nil && options.Shutdown.Err() != nil {
		return nil
	}
	return err
}
