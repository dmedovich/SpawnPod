package forgejo

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/supervisor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const testID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeBackend struct {
	created     engine.CreateRequest
	creates     int
	events      []supervisor.Event
	execErr     error
	removeErr   error
	copiedIn    strings.Builder
	lastCommand supervisor.Request
}

func (f *fakeBackend) Create(_ context.Context, req engine.CreateRequest) (engine.Record, error) {
	f.created = req
	f.creates++
	return engine.Record{ID: testID}, nil
}
func (f *fakeBackend) Start(context.Context, string) (engine.Record, error) {
	return engine.Record{ID: testID, Profile: "default"}, nil
}
func (f *fakeBackend) Metadata(context.Context, string) (engine.RuntimeMetadata, error) {
	return engine.RuntimeMetadata{OS: "Linux", Arch: "amd64", Environment: map[string]string{"PATH": "/bin"}}, nil
}
func (f *fakeBackend) Exec(_ context.Context, _ string, req supervisor.Request, emit func(supervisor.Event) error) (engine.Record, error) {
	f.lastCommand = req
	if req.Op != "run" || req.ID == "" || req.Dir != rootPath || req.TimeoutMillis != (30*time.Minute).Milliseconds() {
		return engine.Record{}, errors.New("bad supervisor request")
	}
	for _, event := range f.events {
		if err := emit(event); err != nil {
			return engine.Record{}, err
		}
	}
	return engine.Record{}, f.execErr
}
func (f *fakeBackend) Remove(context.Context, string) error { return f.removeErr }
func (f *fakeBackend) CopyIn(_ context.Context, _ string, _ string, input io.Reader) error {
	_, err := io.Copy(&f.copiedIn, input)
	return err
}
func (f *fakeBackend) CopyOut(_ context.Context, _ string, _ string, output io.Writer) error {
	_, err := io.WriteString(output, "archive")
	return err
}

type fakeRecords struct{ profile string }

func (r fakeRecords) Get(context.Context, string) (engine.Record, error) {
	profile := r.profile
	if profile == "" {
		profile = "default"
	}
	return engine.Record{ID: testID, Profile: profile}, nil
}

func TestSourceExecEnforcesOperatorProxyWithoutMutatingWorkflowEnv(t *testing.T) {
	_, backend, cfg := server(t)
	p := cfg.Profiles["default"]
	p.Network = "source"
	cfg.Profiles["source"] = p
	cfg.SourceProxy = "http://10.43.0.50:3128"
	s, err := New(backend, fakeRecords{profile: "source"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	backend.events = []supervisor.Event{{ExitCode: &zero}}
	env := map[string]string{"HTTP_PROXY": "http://evil.invalid", "http_proxy": "http://evil.invalid", "HTTPS_PROXY": "http://evil.invalid", "https_proxy": "http://evil.invalid", "NO_PROXY": "*", "no_proxy": "*", "SCOPED_TOKEN": "private"}
	if err := s.Exec(&pb.ExecRequest{EnvironmentId: testID, Command: []string{"true"}, Env: env}, &fakeStream[pb.ExecOutput]{}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if backend.lastCommand.Env[key] != cfg.SourceProxy || env[key] != "http://evil.invalid" {
			t.Fatalf("proxy override failed or input mutated for %s", key)
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if backend.lastCommand.Env[key] != "" || env[key] != "*" {
			t.Fatalf("bypass env retained or input mutated for %s", key)
		}
	}
	if backend.lastCommand.Env["SCOPED_TOKEN"] != "private" {
		t.Fatal("unrelated command env lost")
	}
}

type fakeStream[T any] struct {
	grpc.ServerStream
	sent []*T
}

type fakeCopyInStream struct {
	grpc.ServerStream
	chunks   []*pb.CopyInChunk
	response *pb.CopyInResponse
}

func (f *fakeCopyInStream) Context() context.Context { return context.Background() }
func (f *fakeCopyInStream) Recv() (*pb.CopyInChunk, error) {
	if len(f.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := f.chunks[0]
	f.chunks = f.chunks[1:]
	return chunk, nil
}
func (f *fakeCopyInStream) SendAndClose(response *pb.CopyInResponse) error {
	f.response = response
	return nil
}

func (f *fakeStream[T]) Context() context.Context { return context.Background() }
func (f *fakeStream[T]) Send(value *T) error      { f.sent = append(f.sent, value); return nil }

func server(t *testing.T) (*Server, *fakeBackend, config.Config) {
	t.Helper()
	content, err := os.ReadFile("../../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Decode(strings.NewReader(string(content)))
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	s, err := New(backend, fakeRecords{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, backend, cfg
}

func TestCreateAppliesProfileAndRejectsUnsupportedInputs(t *testing.T) {
	s, backend, cfg := server(t)
	request := &pb.CreateRequest{Name: "job", EnvironmentTimeout: durationpb.New(10 * time.Minute)}
	response, err := s.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetEnvironmentId() != testID || response.GetRootPath() != rootPath || response.GetArch() != "x86_64" || backend.created.Job.Image != cfg.Profiles["default"].Image || backend.created.Profile != "default" {
		t.Fatalf("wrong Create mapping: %+v, %+v", response, backend.created)
	}
	if backend.created.Deadline.Before(time.Now().Add(9 * time.Minute)) {
		t.Fatal("deadline was not applied")
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.CreateRequest)
	}{
		{"unapproved image", func(r *pb.CreateRequest) { r.Image = "unapproved:latest" }},
		{"unapproved label", func(r *pb.CreateRequest) { r.LabelArg = "other" }},
		{"capabilities", func(r *pb.CreateRequest) { r.CapAdd = []string{"SYS_ADMIN"} }},
		{"services", func(r *pb.CreateRequest) { r.Services = []*pb.ServiceContainer{{Name: "db"}} }},
		{"unknown option", func(r *pb.CreateRequest) { r.BackendOptions = map[string]string{"namespace": "prod"} }},
		{"unknown profile", func(r *pb.CreateRequest) { r.BackendOptions = map[string]string{"profile": "unknown"} }},
		{"timeout above profile", func(r *pb.CreateRequest) { r.EnvironmentTimeout = durationpb.New(2 * time.Hour) }},
		{"timeout too short", func(r *pb.CreateRequest) { r.EnvironmentTimeout = durationpb.New(time.Millisecond) }},
		{"name too long", func(r *pb.CreateRequest) { r.Name = strings.Repeat("x", 257) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &pb.CreateRequest{Name: "job", EnvironmentTimeout: durationpb.New(time.Minute)}
			tc.change(r)
			if _, err := s.Create(context.Background(), r); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestCreateMapsApprovedServicesAndTransientEnvironment(t *testing.T) {
	_, backend, cfg := server(t)
	approved := config.ServiceProfile{
		Image:        "registry.example.com/redis@sha256:" + strings.Repeat("d", 64),
		Command:      []string{"redis-server", "--port", "6379"},
		Architecture: "amd64", UID: 10002, GID: 10002, Port: 6379,
		CPURequest: "50m", CPULimit: "250m", MemoryRequest: "64Mi", MemoryLimit: "256Mi",
		StorageRequest: "128Mi", StorageLimit: "512Mi",
	}
	cfg.Controller.EnableServices = true
	profile := cfg.Profiles["default"]
	profile.Services = map[string]config.ServiceProfile{"redis": approved}
	cfg.Profiles["default"] = profile
	s, err := New(backend, fakeRecords{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := func(ports ...string) *pb.CreateRequest {
		return &pb.CreateRequest{Name: "job", EnvironmentTimeout: durationpb.New(time.Minute), Services: []*pb.ServiceContainer{{Name: "redis", Image: approved.Image, Ports: ports, Env: map[string]string{"SERVICE_TOKEN": "private-value"}}}}
	}
	for _, ports := range [][]string{nil, {"6379"}, {"6379:6379"}} {
		if _, err := s.Create(context.Background(), request(ports...)); err != nil {
			t.Fatalf("ports %v: %v", ports, err)
		}
		if len(backend.created.Services) != 1 || backend.created.Services[0].Name != "redis" || backend.created.Services[0].Port != 6379 || backend.created.Services[0].Image != approved.Image || backend.created.ServiceEnvironments["redis"]["SERVICE_TOKEN"] != "private-value" {
			t.Fatalf("wrong service mapping: %+v", backend.created)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*pb.CreateRequest)
	}{
		{"nil service", func(r *pb.CreateRequest) { r.Services[0] = nil }},
		{"duplicate service", func(r *pb.CreateRequest) { r.Services = append(r.Services, r.Services[0]) }},
		{"unknown service", func(r *pb.CreateRequest) { r.Services[0].Name = "db" }},
		{"mutable image", func(r *pb.CreateRequest) { r.Services[0].Image = "redis:latest" }},
		{"wrong port", func(r *pb.CreateRequest) { r.Services[0].Ports = []string{"6380"} }},
		{"remapped port", func(r *pb.CreateRequest) { r.Services[0].Ports = []string{"16379:6379"} }},
		{"dynamic host port", func(r *pb.CreateRequest) { r.Services[0].Ports = []string{":6379"} }},
		{"host IP", func(r *pb.CreateRequest) { r.Services[0].Ports = []string{"127.0.0.1:6379:6379"} }},
		{"multiple ports", func(r *pb.CreateRequest) { r.Services[0].Ports = []string{"6379", "6380"} }},
		{"invalid env key", func(r *pb.CreateRequest) { r.Services[0].Env = map[string]string{"BAD=NAME": "value"} }},
		{"env NUL", func(r *pb.CreateRequest) { r.Services[0].Env = map[string]string{"KEY": "bad\x00value"} }},
		{"oversize env", func(r *pb.CreateRequest) {
			r.Services[0].Env = map[string]string{"KEY": strings.Repeat("x", 512*1024+1)}
		}},
		{"too many services", func(r *pb.CreateRequest) { r.Services = make([]*pb.ServiceContainer, 9) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request("6379")
			tc.edit(r)
			before := backend.creates
			if _, err := s.Create(context.Background(), r); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v", err)
			}
			if backend.creates != before {
				t.Fatal("invalid request reached engine")
			}
		})
	}
	cfg.Controller.EnableServices = false
	s, err = New(backend, fakeRecords{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := backend.creates
	if _, err := s.Create(context.Background(), request("6379")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("disabled services: %v", err)
	}
	if backend.creates != before {
		t.Fatal("disabled service request reached engine")
	}
}

func TestStartAndExecTerminalOrdering(t *testing.T) {
	s, backend, _ := server(t)
	start := &fakeStream[pb.StartOutput]{}
	if err := s.Start(&pb.StartRequest{EnvironmentId: testID}, start); err != nil {
		t.Fatal(err)
	}
	if len(start.sent) != 1 || start.sent[0].GetStartComplete() == nil || start.sent[0].GetStartComplete().GetImageEnv()["PATH"] != "/bin" {
		t.Fatal("StartComplete missing")
	}
	zero := 7
	backend.events = []supervisor.Event{{Stream: "stdout", Data: []byte("hello")}, {ExitCode: &zero}}
	execStream := &fakeStream[pb.ExecOutput]{}
	if err := s.Exec(&pb.ExecRequest{EnvironmentId: testID, Command: []string{"sh", "-c", "exit 7"}}, execStream); err != nil {
		t.Fatal(err)
	}
	if len(execStream.sent) != 2 || string(execStream.sent[0].GetData().GetData()) != "hello" || execStream.sent[1].GetExecComplete().GetExitCode() != 7 {
		t.Fatalf("wrong Exec stream: %+v", execStream.sent)
	}
	backend.execErr = errors.New("transport failed")
	execStream = &fakeStream[pb.ExecOutput]{}
	if err := s.Exec(&pb.ExecRequest{EnvironmentId: testID, Command: []string{"true"}}, execStream); err != nil {
		t.Fatal(err)
	}
	if len(execStream.sent) != 2 || execStream.sent[1].GetExecFailed().GetErrorMessage() == "" {
		t.Fatal("expected nonempty ExecFailed after backend error")
	}
	backend.events = nil
	backend.execErr = nil
	execStream = &fakeStream[pb.ExecOutput]{}
	if err := s.Exec(&pb.ExecRequest{EnvironmentId: testID, Command: []string{"true"}}, execStream); err != nil {
		t.Fatal(err)
	}
	if len(execStream.sent) != 1 || execStream.sent[0].GetExecFailed().GetErrorMessage() == "" {
		t.Fatal("expected nonempty ExecFailed when backend ends without a terminal event")
	}
}

func TestExecRejectsUserAndWorkdirOverrides(t *testing.T) {
	s, _, _ := server(t)
	for _, req := range []*pb.ExecRequest{
		{EnvironmentId: testID, Command: []string{"true"}, User: ptr("0")},
		{EnvironmentId: testID, Command: []string{"true"}, Workdir: "/etc"},
		{EnvironmentId: testID, Command: []string{"true"}, Workdir: "/workspace/../etc"},
		{EnvironmentId: testID, Command: []string{"true"}, Env: map[string]string{"LARGE": strings.Repeat("x", 1<<20)}},
	} {
		if err := s.Exec(req, &fakeStream[pb.ExecOutput]{}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("got %v", err)
		}
	}
}

func TestRemoveMapsNotFound(t *testing.T) {
	s, backend, _ := server(t)
	backend.removeErr = engine.ErrNotFound
	if _, err := s.Remove(context.Background(), &pb.RemoveRequest{EnvironmentId: testID}); status.Code(err) != codes.NotFound {
		t.Fatalf("got %v", err)
	}
}

func TestCopyStreamsAndRejectsRepeatedMetadata(t *testing.T) {
	s, backend, _ := server(t)
	dest := "/workspace/.act/"
	id := testID
	in := &fakeCopyInStream{chunks: []*pb.CopyInChunk{{EnvironmentId: &id, DestPath: &dest, Data: []byte("a")}, {Data: []byte("b")}}}
	if err := s.CopyIn(in); err != nil {
		t.Fatal(err)
	}
	if in.response == nil || backend.copiedIn.String() != "ab" {
		t.Fatalf("response=%+v data=%q", in.response, backend.copiedIn.String())
	}
	out := &fakeStream[pb.CopyOutChunk]{}
	if err := s.CopyOut(&pb.CopyOutRequest{EnvironmentId: testID, SrcPath: "/workspace/.act/file"}, out); err != nil {
		t.Fatal(err)
	}
	if len(out.sent) != 1 || string(out.sent[0].GetData()) != "archive" {
		t.Fatalf("CopyOut chunks=%+v", out.sent)
	}
	bad := &fakeCopyInStream{chunks: []*pb.CopyInChunk{{EnvironmentId: &id, DestPath: &dest}, {EnvironmentId: &id}}}
	if err := s.CopyIn(bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
}

func ptr(value string) *string { return &value }
