// Package smoke drives the real Forgejo plugin RPCs for a minimal cluster
// scenario without requiring a registered Forgejo Runner.
package smoke

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
)

const marker = "spawnpod-smoke-ok"

// Run exercises Create, Start, CopyIn, Exec, CopyOut and Remove against the
// local controller socket. It always attempts cleanup after Create succeeds.
func Run(ctx context.Context, socket, image, profile string, out io.Writer) (runErr error) {
	if socket == "" || image == "" || profile == "" || out == nil {
		return errors.New("socket, image, profile and output are required")
	}
	conn, err := grpc.NewClient("passthrough:///spawnpod", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := pb.NewBackendPluginClient(conn)
	if _, err := client.Capabilities(ctx, &pb.CapabilitiesRequest{}); err != nil {
		return fmt.Errorf("Capabilities: %w", err)
	}
	created, err := client.Create(ctx, &pb.CreateRequest{Name: "spawnpod-smoke", Image: image, LabelArg: image, BackendOptions: map[string]string{"profile": profile}, EnvironmentTimeout: durationpb.New(10 * time.Minute)})
	if err != nil {
		return fmt.Errorf("Create: %w", err)
	}
	id := created.GetEnvironmentId()
	if id == "" {
		return errors.New("Create returned no environment ID; inspect records before retrying")
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, err := client.Remove(cleanupCtx, &pb.RemoveRequest{EnvironmentId: id})
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("Remove %s: %w", id, err))
		} else {
			if _, err := fmt.Fprintf(out, "removed environment %s\n", id); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("write smoke report: %w", err))
			}
		}
	}()
	if _, err := fmt.Fprintf(out, "created environment %s\n", id); err != nil {
		return fmt.Errorf("write smoke report: %w", err)
	}
	started, err := client.Start(ctx, &pb.StartRequest{EnvironmentId: id})
	if err != nil {
		return fmt.Errorf("Start: %w", err)
	}
	completed := false
	for {
		message, err := started.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("Start stream: %w", err)
		}
		if message.GetStartComplete() != nil {
			completed = true
		}
	}
	if !completed {
		return errors.New("Start ended without StartComplete")
	}
	if _, err := fmt.Fprintln(out, "job Pod ready"); err != nil {
		return fmt.Errorf("write smoke report: %w", err)
	}
	archive, err := smokeArchive()
	if err != nil {
		return err
	}
	in, err := client.CopyIn(ctx)
	if err != nil {
		return fmt.Errorf("CopyIn: %w", err)
	}
	dest := "/workspace"
	if err := in.Send(&pb.CopyInChunk{EnvironmentId: &id, DestPath: &dest, Data: archive}); err != nil {
		return fmt.Errorf("CopyIn send: %w", err)
	}
	if _, err := in.CloseAndRecv(); err != nil {
		return fmt.Errorf("CopyIn completion: %w", err)
	}
	if err := execute(ctx, client, id); err != nil {
		return err
	}
	if err := copyOut(ctx, client, id); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "file transfer and command execution passed")
	return err
}

func smokeArchive() ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: "smoke.txt", Mode: 0644, Size: int64(len(marker)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := writer.Write([]byte(marker)); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func execute(ctx context.Context, client pb.BackendPluginClient, id string) error {
	stream, err := client.Exec(ctx, &pb.ExecRequest{EnvironmentId: id, Command: []string{"sh", "-c", "cat /workspace/smoke.txt"}, Workdir: "/workspace"})
	if err != nil {
		return fmt.Errorf("Exec: %w", err)
	}
	var stdout bytes.Buffer
	completed := false
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("Exec stream: %w", err)
		}
		if data := message.GetData(); data != nil && data.GetStream() == pb.DataChunk_STDOUT {
			if stdout.Len()+len(data.GetData()) > 1<<20 {
				return errors.New("Exec output exceeds smoke limit")
			}
			stdout.Write(data.GetData())
		}
		if done := message.GetExecComplete(); done != nil {
			if done.GetExitCode() != 0 {
				return fmt.Errorf("Exec exited %d", done.GetExitCode())
			}
			completed = true
		}
		if failure := message.GetExecFailed(); failure != nil {
			return fmt.Errorf("Exec failed: %s", failure.GetErrorMessage())
		}
	}
	if !completed || stdout.String() != marker {
		return fmt.Errorf("Exec result differs from expected marker: completed=%t, output=%q", completed, stdout.String())
	}
	return nil
}

func copyOut(ctx context.Context, client pb.BackendPluginClient, id string) error {
	stream, err := client.CopyOut(ctx, &pb.CopyOutRequest{EnvironmentId: id, SrcPath: "/workspace/smoke.txt"})
	if err != nil {
		return fmt.Errorf("CopyOut: %w", err)
	}
	var buffer bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("CopyOut stream: %w", err)
		}
		if buffer.Len()+len(chunk.GetData()) > 1<<20 {
			return errors.New("CopyOut archive exceeds smoke limit")
		}
		if _, err := buffer.Write(chunk.GetData()); err != nil {
			return err
		}
	}
	reader := tar.NewReader(&buffer)
	header, err := reader.Next()
	if err != nil {
		return fmt.Errorf("read copy-out archive: %w", err)
	}
	if header == nil || header.Name != "smoke.txt" {
		return errors.New("copy-out archive has unexpected entry")
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read copy-out content: %w", err)
	}
	if string(data) != marker {
		return errors.New("copy-out content differs from expected marker")
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return errors.New("CopyOut archive has extra entries")
	}
	return nil
}
