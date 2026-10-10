package unixrpc

import (
	"context"
	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"github.com/dmedovich/spawnpod/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"path/filepath"
	"testing"
	"time"
)

type drainPlugin struct {
	pb.UnimplementedBackendPluginServer
	entered, release chan struct{}
}

func (p *drainPlugin) Capabilities(ctx context.Context, _ *pb.CapabilitiesRequest) (*pb.CapabilitiesResponse, error) {
	close(p.entered)
	select {
	case <-p.release:
		return &pb.CapabilitiesResponse{Name: "completed"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestDrainCompletesOrCancelsActiveRPCWithinBudget(t *testing.T) {
	for _, mode := range []string{"complete", "timeout", "lease-loss"} {
		t.Run(mode, func(t *testing.T) {
			ctx, loseLease := context.WithCancel(t.Context())
			defer loseLease()
			shutdown, stop := context.WithCancel(t.Context())
			defer stop()
			p := &drainPlugin{entered: make(chan struct{}), release: make(chan struct{})}
			metrics := &telemetry.State{}
			metrics.API(nil)
			path := filepath.Join(t.TempDir(), "private", "rpc.sock")
			done := make(chan error, 1)
			go func() {
				done <- ServeWithOptions(ctx, path, p, Options{Shutdown: shutdown, DrainTimeout: 200 * time.Millisecond, Metrics: metrics})
			}()
			conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			rpcCtx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := pb.NewBackendPluginClient(conn).Capabilities(rpcCtx, &pb.CapabilitiesRequest{}, grpc.WaitForReady(true))
				result <- err
			}()
			select {
			case <-p.entered:
			case <-rpcCtx.Done():
				t.Fatal("RPC never started")
			}
			stop()
			deadline := time.Now().Add(time.Second)
			for metrics.Ready() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if metrics.Ready() {
				t.Fatal("draining server stayed ready")
			}
			select {
			case err := <-result:
				t.Fatalf("RPC aborted before drain budget: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if mode == "complete" {
				close(p.release)
			} else if mode == "lease-loss" {
				loseLease()
			}
			select {
			case err := <-result:
				if (err == nil) != (mode == "complete") {
					t.Fatalf("mode %s: %v", mode, err)
				}
			case <-rpcCtx.Done():
				t.Fatal("RPC exceeded shutdown budget")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-rpcCtx.Done():
				t.Fatal("server did not stop")
			}
		})
	}
}
