package smoke

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"github.com/dmedovich/spawnpod/internal/transport/unixrpc"
	"google.golang.org/grpc"
)

type fakePlugin struct {
	pb.UnimplementedBackendPluginServer
	archive []byte
	removed bool
}

func (*fakePlugin) Capabilities(context.Context, *pb.CapabilitiesRequest) (*pb.CapabilitiesResponse, error) {
	return &pb.CapabilitiesResponse{Name: "spawnpod"}, nil
}

func (*fakePlugin) Create(context.Context, *pb.CreateRequest) (*pb.CreateResponse, error) {
	return &pb.CreateResponse{EnvironmentId: strings.Repeat("a", 32)}, nil
}

func (*fakePlugin) Start(_ *pb.StartRequest, stream grpc.ServerStreamingServer[pb.StartOutput]) error {
	return stream.Send(&pb.StartOutput{Output: &pb.StartOutput_StartComplete{StartComplete: &pb.StartComplete{}}})
}

func (f *fakePlugin) CopyIn(stream grpc.ClientStreamingServer[pb.CopyInChunk, pb.CopyInResponse]) error {
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&pb.CopyInResponse{})
		}
		if err != nil {
			return err
		}
		f.archive = append(f.archive, chunk.GetData()...)
	}
}

func (*fakePlugin) Exec(_ *pb.ExecRequest, stream grpc.ServerStreamingServer[pb.ExecOutput]) error {
	if err := stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_Data{Data: &pb.DataChunk{Stream: pb.DataChunk_STDOUT, Data: []byte(marker)}}}); err != nil {
		return err
	}
	return stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_ExecComplete{ExecComplete: &pb.ExecComplete{ExitCode: 0}}})
}

func (f *fakePlugin) CopyOut(_ *pb.CopyOutRequest, stream grpc.ServerStreamingServer[pb.CopyOutChunk]) error {
	return stream.Send(&pb.CopyOutChunk{Data: f.archive})
}

func (f *fakePlugin) Remove(context.Context, *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	f.removed = true
	return &pb.RemoveResponse{}, nil
}

func TestSmokeRunsProtocolAndRemovesEnvironment(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "private", "plugin.sock")
	plugin := &fakePlugin{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- unixrpc.Serve(ctx, socket, plugin) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test server did not start")
		}
		time.Sleep(time.Millisecond)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), socket, "example@sha256:"+strings.Repeat("a", 64), "default", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !plugin.removed || !strings.Contains(out.String(), "file transfer and command execution passed") {
		t.Fatalf("smoke did not finish and clean up: %s", out.String())
	}
	// A broken output pipe must report failure and still remove the created job.
	plugin.removed = false
	reader, writer := io.Pipe()
	_ = reader.CloseWithError(io.ErrClosedPipe)
	defer func() { _ = writer.Close() }()
	if err := Run(context.Background(), socket, "example@sha256:"+strings.Repeat("a", 64), "default", writer); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("missing output failure: %v", err)
	}
	if !plugin.removed {
		t.Fatal("output failure skipped environment cleanup")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
