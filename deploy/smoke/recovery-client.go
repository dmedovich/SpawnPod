// recovery-client exercises recovery and service behavior through the real
// Forgejo plugin RPC without registering a workflow. Remove test environments.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func main() {
	var socket, image, idFile, mode, target, profile string
	flag.StringVar(&socket, "socket", ".local/run/backend.sock", "local plugin socket")
	flag.StringVar(&image, "image", "", "approved job image for create")
	flag.StringVar(&idFile, "id-file", ".local/recovery-environment-id", "private environment ID file")
	flag.StringVar(&mode, "mode", "", "create, create-service, exec, hold, service-echo, probe, tar-traversal, or remove")
	flag.StringVar(&target, "target", "", "service IP for probe mode")
	flag.StringVar(&profile, "profile", "default", "approved execution profile for create")
	flag.Parse()
	if err := run(socket, image, idFile, mode, target, profile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(socket, image, idFile, mode, target, profile string) error {
	if mode != "create" && mode != "create-service" && mode != "exec" && mode != "hold" && mode != "service-echo" && mode != "probe" && mode != "source-probe" && mode != "tar-traversal" && mode != "remove" {
		return errors.New("invalid mode")
	}
	conn, err := grpc.NewClient("passthrough:///spawnpod-recovery", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewBackendPluginClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if mode == "create" || mode == "create-service" {
		if image == "" {
			return errors.New("-image is required for create")
		}
		request := &pb.CreateRequest{Name: "spawnpod-recovery-smoke", Image: image, LabelArg: image, BackendOptions: map[string]string{"profile": profile}, EnvironmentTimeout: durationpb.New(10 * time.Minute)}
		if mode == "create-service" {
			request.Services = []*pb.ServiceContainer{{Name: "echo", Image: image, Ports: []string{"8080:8080"}}}
		}
		created, err := client.Create(ctx, request)
		if err != nil {
			return fmt.Errorf("Create: %w", err)
		}
		id := created.GetEnvironmentId()
		if id == "" {
			return errors.New("Create returned no environment ID")
		}
		if err := os.WriteFile(idFile, []byte(id+"\n"), 0600); err != nil {
			return fmt.Errorf("save environment ID %s: %w", id, err)
		}
		started, err := client.Start(ctx, &pb.StartRequest{EnvironmentId: id})
		if err != nil {
			return fmt.Errorf("Start %s: %w", id, err)
		}
		complete := false
		for {
			message, err := started.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("Start stream %s: %w", id, err)
			}
			if message.GetStartComplete() != nil {
				complete = true
			}
		}
		if !complete {
			return errors.New("Start ended without StartComplete")
		}
		fmt.Printf("ready environment %s\n", id)
		return nil
	}
	data, err := os.ReadFile(idFile)
	if err != nil {
		return err
	}
	id := string(bytes.TrimSpace(data))
	if id == "" {
		return errors.New("empty environment ID")
	}
	if mode == "remove" {
		if _, err := client.Remove(ctx, &pb.RemoveRequest{EnvironmentId: id}); err != nil {
			return fmt.Errorf("Remove %s: %w", id, err)
		}
		fmt.Printf("removed environment %s\n", id)
		return nil
	}
	if mode == "tar-traversal" {
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		if err := writer.WriteHeader(&tar.Header{Name: "../escape", Mode: 0600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := writer.Write([]byte("x")); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		stream, err := client.CopyIn(ctx)
		if err != nil {
			return err
		}
		dest := "/workspace"
		if err := stream.Send(&pb.CopyInChunk{EnvironmentId: &id, DestPath: &dest, Data: archive.Bytes()}); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		_, copyErr := stream.CloseAndRecv()
		if status.Code(copyErr) != codes.Internal {
			return fmt.Errorf("expected tar traversal denial, got %v", copyErr)
		}
		fmt.Printf("tar traversal denied in %s; controller reports the extraction error\n", id)
		return nil
	}
	command := []string{"sh", "-c", "printf 'recovery-ok'"}
	if mode == "hold" {
		command = []string{"sh", "-c", "printf 'hold-started\\n'; sleep 300"}
	} else if mode == "service-echo" {
		command = []string{"sh", "-c", "printf 'service-ok\\n' | /bin/busybox nc -w 3 echo 8080"}
	} else if mode == "probe" {
		if net.ParseIP(target) == nil {
			return errors.New("-target must be a literal IP for probe")
		}
		command = []string{"sh", "-c", "if /bin/busybox nc -z -w 3 " + target + " 8080; then printf reachable; else printf denied; fi"}
	} else if mode == "source-probe" {
		command = []string{"sh", "-c", "set -e; /bin/busybox wget -q -O /tmp/source-response http://forgejo.local:3000/api/v1/version; test -s /tmp/source-response; if /bin/busybox wget -q -O /dev/null http://other.local:3000/; then exit 1; fi; for endpoint in '10.42.0.1 3000' '10.43.0.1 443' '169.254.169.254 80'; do if /bin/busybox nc -z -w 2 $endpoint; then exit 1; fi; done; printf source-ok"}
	}
	stream, err := client.Exec(ctx, &pb.ExecRequest{EnvironmentId: id, Command: command, Workdir: "/workspace"})
	if err != nil {
		return fmt.Errorf("Exec %s: %w", id, err)
	}
	var output bytes.Buffer
	completed := false
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("Exec stream %s: %w", id, err)
		}
		if data := message.GetData(); data != nil && data.GetStream() == pb.DataChunk_STDOUT {
			if output.Len()+len(data.GetData()) > 1<<20 {
				return errors.New("output exceeded smoke limit")
			}
			output.Write(data.GetData())
			if mode == "hold" && bytes.Contains(output.Bytes(), []byte("hold-started\n")) {
				fmt.Printf("command started in %s\n", id)
				output.Reset()
			}
		}
		if failure := message.GetExecFailed(); failure != nil {
			return fmt.Errorf("Exec failed: %s", failure.GetErrorMessage())
		}
		if done := message.GetExecComplete(); done != nil {
			if done.GetExitCode() != 0 {
				return fmt.Errorf("Exec exited %d", done.GetExitCode())
			}
			completed = true
		}
	}
	if !completed {
		return errors.New("Exec ended without ExecComplete")
	}
	if mode == "exec" && output.String() != "recovery-ok" {
		return fmt.Errorf("unexpected command output %q", output.String())
	}
	if mode == "service-echo" && output.String() != "service-ok\n" {
		return fmt.Errorf("unexpected service response %q", output.String())
	}
	if mode == "probe" && output.String() != "denied" {
		return fmt.Errorf("cross-environment probe was %q", output.String())
	}
	if mode == "source-probe" && output.String() != "source-ok" {
		return fmt.Errorf("source probe was %q", output.String())
	}
	switch mode {
	case "service-echo":
		fmt.Printf("service echo passed in %s\n", id)
	case "probe":
		fmt.Printf("cross-environment connection denied in %s\n", id)
	default:
		fmt.Printf("exec passed in %s\n", id)
	}
	return nil
}
