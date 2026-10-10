package unixrpc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

type testPlugin struct {
	pb.UnimplementedBackendPluginServer
}

func (testPlugin) Capabilities(context.Context, *pb.CapabilitiesRequest) (*pb.CapabilitiesResponse, error) {
	return &pb.CapabilitiesResponse{Name: "spawnpod"}, nil
}

func TestServeHealthAndCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "plugin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, testPlugin{}) }()
	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("server exited: %v", err)
		case <-deadline:
			t.Fatal("socket not created")
		case <-time.After(time.Millisecond):
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode=%#o", info.Mode().Perm())
	}
	conn, err := grpc.NewClient("passthrough:///spawnpod", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	requestCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	health, err := grpc_health_v1.NewHealthClient(conn).Check(requestCtx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil || health.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	caps, err := pb.NewBackendPluginClient(conn).Capabilities(requestCtx, &pb.CapabilitiesRequest{})
	if err != nil || caps.GetName() != "spawnpod" {
		t.Fatalf("caps=%+v err=%v", caps, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestRejectsSharedSocketDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listen(filepath.Join(dir, "plugin.sock")); err == nil {
		t.Fatal("accepted shared directory")
	}
}

// Runner sends a ping every 30 seconds during quiet Start/Exec streams.
// Three rejected pings used to close the connection after about 90 seconds.
func TestServeRunnerKeepalive(t *testing.T) {
	if testing.Short() {
		t.Skip("requires three real Runner keepalive intervals")
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "private", "plugin.sock")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, testPlugin{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server exited: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	conn, err := grpc.NewClient("passthrough:///spawnpod",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	watchCtx, stop := context.WithTimeout(t.Context(), 100*time.Second)
	defer stop()
	stream, err := grpc_health_v1.NewHealthClient(conn).Watch(watchCtx, &grpc_health_v1.HealthCheckRequest{}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := stream.Recv()
	if err != nil || initial.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("initial health=%v err=%v", initial, err)
	}
	// Health stays unchanged, so this RPC has no application traffic.
	if _, err := stream.Recv(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("quiet stream closed before its deadline: %v", err)
	}
}
