package forgejo

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/supervisor"
	"github.com/dmedovich/spawnpod/internal/workspace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	rootPath        = "/workspace"
	maxCommandBytes = 256 * 1024
)

var environmentID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Backend interface {
	Create(context.Context, engine.CreateRequest) (engine.Record, error)
	Start(context.Context, string) (engine.Record, error)
	Metadata(context.Context, string) (engine.RuntimeMetadata, error)
	Exec(context.Context, string, supervisor.Request, func(supervisor.Event) error) (engine.Record, error)
	CopyIn(context.Context, string, string, io.Reader) error
	CopyOut(context.Context, string, string, io.Writer) error
	Remove(context.Context, string) error
}

type RecordReader interface {
	Get(context.Context, string) (engine.Record, error)
}

// Server implements the runner's protobuf contract. The controller registers
// it only while holding the Kubernetes Lease.
type Server struct {
	pb.UnimplementedBackendPluginServer
	backend         Backend
	records         RecordReader
	profiles        map[string]config.Profile
	agentImage      string
	sourceProxy     string
	servicesEnabled bool
}

func New(backend Backend, records RecordReader, cfg config.Config) (*Server, error) {
	if backend == nil || records == nil {
		return nil, errors.New("forgejo backend and record reader are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	profiles := make(map[string]config.Profile, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profiles[name] = profile
	}
	return &Server{backend: backend, records: records, profiles: profiles, agentImage: cfg.AgentImage, sourceProxy: cfg.SourceProxy, servicesEnabled: cfg.Controller.EnableServices}, nil
}

func (s *Server) Capabilities(context.Context, *pb.CapabilitiesRequest) (*pb.CapabilitiesResponse, error) {
	return &pb.CapabilitiesResponse{Name: "spawnpod"}, nil
}

func (s *Server) Create(ctx context.Context, req *pb.CreateRequest) (*pb.CreateResponse, error) {
	if req == nil || req.GetName() == "" || len(req.GetName()) > 256 || strings.ContainsRune(req.GetName(), 0) {
		return nil, invalid("invalid job name")
	}
	if len(req.GetCapAdd()) != 0 || len(req.GetCapDrop()) != 0 {
		return nil, invalid("capability changes are not supported")
	}
	for key := range req.GetBackendOptions() {
		if key != "profile" {
			return nil, invalid("unknown backend option")
		}
	}
	profileName := req.GetBackendOptions()["profile"]
	if profileName == "" {
		profileName = "default"
	}
	profile, ok := s.profiles[profileName]
	if !ok {
		return nil, invalid("unknown execution profile")
	}
	if req.GetImage() != "" && req.GetImage() != profile.Image {
		return nil, invalid("job image is not approved by profile")
	}
	if req.GetLabelArg() != "" && req.GetLabelArg() != profile.Image {
		return nil, invalid("label image is not approved by profile")
	}
	if req.GetEnvironmentTimeout() == nil {
		return nil, invalid("environment timeout is required")
	}
	if err := req.GetEnvironmentTimeout().CheckValid(); err != nil {
		return nil, invalid("invalid environment timeout")
	}
	lifetime := req.GetEnvironmentTimeout().AsDuration()
	maxLifetime, _ := profile.Lifetime()
	if lifetime < time.Second || lifetime > maxLifetime {
		return nil, invalid("environment timeout exceeds profile limit")
	}
	services, environments, err := s.mapServices(req.GetServices(), profile)
	if err != nil {
		return nil, err
	}
	record, err := s.backend.Create(ctx, engine.CreateRequest{Owner: req.GetName(), Profile: profileName, Deadline: time.Now().Add(lifetime), Job: profile.JobSpec(s.agentImage), Services: services, ServiceEnvironments: environments})
	if err != nil {
		return nil, mapError(err)
	}
	arch := "x86_64"
	if profile.Architecture == "arm64" {
		arch = "aarch64"
	}
	return &pb.CreateResponse{EnvironmentId: record.ID, RootPath: rootPath, ActPath: rootPath + "/.act", ToolCachePath: rootPath + "/.toolcache", TempPath: rootPath + "/.tmp", Os: "Linux", Arch: arch}, nil
}

// mapServices accepts only services explicitly approved by the selected
// profile. Environment values are forwarded to Create and never stored here.
func (s *Server) mapServices(requests []*pb.ServiceContainer, profile config.Profile) ([]engine.ServiceSpec, map[string]map[string]string, error) {
	if len(requests) == 0 {
		return nil, nil, nil
	}
	if !s.servicesEnabled {
		return nil, nil, invalid("service containers are disabled by controller configuration")
	}
	if len(requests) > 8 {
		return nil, nil, invalid("too many service containers")
	}
	services := make([]engine.ServiceSpec, 0, len(requests))
	environments := make(map[string]map[string]string, len(requests))
	seen := make(map[string]bool, len(requests))
	for _, request := range requests {
		if request == nil || request.GetName() == "" || seen[request.GetName()] {
			return nil, nil, invalid("service name is missing or duplicated")
		}
		name := request.GetName()
		approved, ok := profile.Services[name]
		if !ok || request.GetImage() != approved.Image {
			return nil, nil, invalid("service name or image is not approved by profile")
		}
		if !approvedServicePorts(request.GetPorts(), approved.Port) {
			return nil, nil, invalid("service ports differ from approved profile")
		}
		if !validServiceEnvironment(request.GetEnv()) {
			return nil, nil, invalid("invalid service environment")
		}
		seen[name] = true
		services = append(services, approved.Spec(name, s.agentImage))
		if len(request.GetEnv()) > 0 {
			environments[name] = request.GetEnv()
		}
	}
	return services, environments, nil
}

func approvedServicePorts(ports []string, approved int32) bool {
	if len(ports) == 0 {
		return true // The approved port is still exposed inside the environment.
	}
	if len(ports) != 1 {
		return false
	}
	port := strconv.FormatInt(int64(approved), 10)
	return ports[0] == port || ports[0] == port+":"+port
}

func validServiceEnvironment(environment map[string]string) bool {
	if len(environment) > 128 {
		return false
	}
	bytes := 0
	for key, value := range environment {
		if key == "" || len(key) > 255 || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return false
		}
		bytes += len(key) + len(value)
		if bytes > 512*1024 {
			return false
		}
	}
	return true
}

func (s *Server) Start(req *pb.StartRequest, stream grpc.ServerStreamingServer[pb.StartOutput]) error {
	if req == nil || !environmentID.MatchString(req.GetEnvironmentId()) {
		return invalid("invalid environment ID")
	}
	record, err := s.backend.Start(stream.Context(), req.GetEnvironmentId())
	if err != nil {
		return mapError(err)
	}
	metadata, err := s.backend.Metadata(stream.Context(), req.GetEnvironmentId())
	if err != nil {
		return mapError(err)
	}
	profile, ok := s.profiles[record.Profile]
	if !ok || metadata.OS != "Linux" || metadata.Arch != profile.Architecture {
		return status.Error(codes.FailedPrecondition, "job Pod metadata differs from approved profile")
	}
	return stream.Send(&pb.StartOutput{Output: &pb.StartOutput_StartComplete{StartComplete: &pb.StartComplete{ImageEnv: metadata.Environment}}})
}

func (s *Server) Exec(req *pb.ExecRequest, stream grpc.ServerStreamingServer[pb.ExecOutput]) error {
	if req == nil || !environmentID.MatchString(req.GetEnvironmentId()) {
		return invalid("invalid environment ID")
	}
	if len(req.GetCommand()) == 0 || req.GetCommand()[0] == "" {
		return invalid("command argv is required")
	}
	commandBytes := 0
	for _, arg := range req.GetCommand() {
		commandBytes += len(arg)
		if strings.ContainsRune(arg, 0) {
			return invalid("command contains NUL")
		}
	}
	if commandBytes > maxCommandBytes {
		return invalid("command is too large")
	}
	workdir := req.GetWorkdir()
	if workdir == "" {
		workdir = rootPath
	}
	if !path.IsAbs(workdir) || path.Clean(workdir) != workdir || (workdir != rootPath && !strings.HasPrefix(workdir, rootPath+"/")) {
		return invalid("workdir must be inside /workspace")
	}
	for key, value := range req.GetEnv() {
		if key == "" || len(key) > 255 || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return invalid("invalid environment variable")
		}
	}
	record, err := s.records.Get(stream.Context(), req.GetEnvironmentId())
	if err != nil {
		return mapError(err)
	}
	profile, ok := s.profiles[record.Profile]
	if !ok {
		return status.Error(codes.FailedPrecondition, "record profile is no longer configured")
	}
	if user := req.GetUser(); user != "" && user != strconv.FormatInt(profile.UID, 10) {
		return invalid("user differs from approved profile UID")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return status.Error(codes.Internal, "cannot create command ID")
	}
	env := req.GetEnv()
	if profile.Network == "source" {
		env = make(map[string]string, len(req.GetEnv())+6)
		maps.Copy(env, req.GetEnv())
		for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
			env[key] = s.sourceProxy
		}
		for _, key := range []string{"NO_PROXY", "no_proxy"} {
			env[key] = ""
		}
	}
	command := supervisor.Request{Op: "run", ID: hex.EncodeToString(random[:]), Argv: req.GetCommand(), Env: env, Dir: workdir, TimeoutMillis: (30 * time.Minute).Milliseconds()}
	payload, err := json.Marshal(command)
	if err != nil || len(payload) >= 1<<20 {
		return invalid("command request exceeds agent limit")
	}
	var terminal *pb.ExecOutput
	_, err = s.backend.Exec(stream.Context(), req.GetEnvironmentId(), command, func(event supervisor.Event) error {
		if terminal != nil {
			return errors.New("output after terminal event")
		}
		if event.Stream != "" {
			var kind pb.DataChunk_Stream
			switch event.Stream {
			case "stdout":
				kind = pb.DataChunk_STDOUT
			case "stderr":
				kind = pb.DataChunk_STDERR
			default:
				return fmt.Errorf("invalid supervisor stream")
			}
			return stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_Data{Data: &pb.DataChunk{Stream: kind, Data: event.Data}}})
		}
		if event.Error != "" {
			terminal = &pb.ExecOutput{Output: &pb.ExecOutput_ExecFailed{ExecFailed: &pb.ExecFailed{ErrorMessage: supervisor.SafeFailure(event.Error)}}}
			return nil
		}
		if event.ExitCode != nil {
			terminal = &pb.ExecOutput{Output: &pb.ExecOutput_ExecComplete{ExecComplete: &pb.ExecComplete{ExitCode: int32(*event.ExitCode)}}}
			return nil
		}
		return errors.New("empty supervisor event")
	})
	if err != nil {
		if stream.Context().Err() != nil {
			return mapError(stream.Context().Err())
		}
		slog.Error("job command failed", "environment", req.GetEnvironmentId(), "failure", "backend operation failed")
		return stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_ExecFailed{ExecFailed: &pb.ExecFailed{ErrorMessage: "execution failed"}}})
	}
	if terminal == nil {
		return stream.Send(&pb.ExecOutput{Output: &pb.ExecOutput_ExecFailed{ExecFailed: &pb.ExecFailed{ErrorMessage: "command ended without a result"}}})
	}
	return stream.Send(terminal)
}

func (s *Server) CopyIn(stream grpc.ClientStreamingServer[pb.CopyInChunk, pb.CopyInResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return invalid("CopyIn metadata is required")
	}
	if first.EnvironmentId == nil || first.DestPath == nil || !environmentID.MatchString(first.GetEnvironmentId()) {
		return invalid("invalid CopyIn metadata")
	}
	if _, err := workspace.Relative(first.GetDestPath()); err != nil {
		return invalid("invalid CopyIn destination")
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.backend.CopyIn(stream.Context(), first.GetEnvironmentId(), first.GetDestPath(), reader)
		_ = reader.CloseWithError(err)
		done <- err
	}()
	var transferred int64
	write := func(data []byte) error {
		if int64(len(data)) > workspace.MaxArchiveBytes-transferred {
			return invalid("CopyIn archive exceeds byte limit")
		}
		transferred += int64(len(data))
		if _, err := writer.Write(data); err != nil {
			return err
		}
		return nil
	}
	if err := write(first.GetData()); err != nil {
		_ = writer.CloseWithError(err)
		<-done
		return mapError(err)
	}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = writer.CloseWithError(err)
			<-done
			return err
		}
		if chunk.EnvironmentId != nil || chunk.DestPath != nil {
			err = invalid("CopyIn metadata may appear only in first chunk")
			_ = writer.CloseWithError(err)
			<-done
			return err
		}
		if err := write(chunk.GetData()); err != nil {
			_ = writer.CloseWithError(err)
			backendErr := <-done
			if backendErr != nil {
				return mapError(backendErr)
			}
			return mapError(err)
		}
	}
	_ = writer.Close()
	if err := <-done; err != nil {
		return mapError(err)
	}
	return stream.SendAndClose(&pb.CopyInResponse{})
}

type chunkWriter struct {
	stream      grpc.ServerStreamingServer[pb.CopyOutChunk]
	transferred int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > workspace.MaxArchiveBytes+(16<<20)-w.transferred {
		return 0, errors.New("CopyOut archive exceeds byte limit")
	}
	w.transferred += int64(len(p))
	n := len(p)
	for len(p) > 0 {
		size := len(p)
		if size > 256<<10 {
			size = 256 << 10
		}
		data := append([]byte(nil), p[:size]...)
		if err := w.stream.Send(&pb.CopyOutChunk{Data: data}); err != nil {
			return n - len(p), err
		}
		p = p[size:]
	}
	return n, nil
}

func (s *Server) CopyOut(req *pb.CopyOutRequest, stream grpc.ServerStreamingServer[pb.CopyOutChunk]) error {
	if req == nil || !environmentID.MatchString(req.GetEnvironmentId()) {
		return invalid("invalid environment ID")
	}
	if _, err := workspace.Relative(req.GetSrcPath()); err != nil {
		return invalid("invalid CopyOut source")
	}
	buffered := bufio.NewWriterSize(&chunkWriter{stream: stream}, 256<<10)
	if err := s.backend.CopyOut(stream.Context(), req.GetEnvironmentId(), req.GetSrcPath(), buffered); err != nil {
		return mapError(err)
	}
	return buffered.Flush()
}

func (s *Server) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	if req == nil || !environmentID.MatchString(req.GetEnvironmentId()) {
		return nil, invalid("invalid environment ID")
	}
	if err := s.backend.Remove(ctx, req.GetEnvironmentId()); err != nil {
		return nil, mapError(err)
	}
	return &pb.RemoveResponse{}, nil
}

func invalid(message string) error { return status.Error(codes.InvalidArgument, message) }

func mapError(err error) error {

	switch {
	case errors.Is(err, engine.ErrNotFound), apierrors.IsNotFound(err):
		return status.Error(codes.NotFound, "environment not found")
	case errors.Is(err, engine.ErrBusy), errors.Is(err, engine.ErrInvalidState):
		return status.Error(codes.FailedPrecondition, "environment is busy or unavailable")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "operation cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "operation deadline exceeded")
	case apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return status.Error(codes.Unavailable, "Kubernetes API unavailable")
	default:
		if remote, ok := status.FromError(err); ok && remote.Code() != codes.OK {
			return status.Error(remote.Code(), "backend operation failed")
		}
		return status.Error(codes.Internal, "backend operation failed")
	}
}
