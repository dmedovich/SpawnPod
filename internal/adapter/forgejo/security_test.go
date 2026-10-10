package forgejo

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/supervisor"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestExecDiagnosticsDoNotExposeAgentErrors(t *testing.T) {
	const canary = "credential-canary"
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for _, transport := range []bool{false, true} {
		s, backend, _ := server(t)
		if transport {
			backend.execErr = errors.New(canary)
		} else {
			backend.events = []supervisor.Event{{Error: canary}}
		}
		stream := &fakeStream[pb.ExecOutput]{}
		if err := s.Exec(&pb.ExecRequest{EnvironmentId: testID, Command: []string{"sh"}}, stream); err != nil {
			t.Fatal(err)
		}
		if len(stream.sent) != 1 || stream.sent[0].GetExecFailed() == nil || strings.Contains(stream.sent[0].String(), canary) {
			t.Fatalf("unsafe terminal output: %v", stream.sent)
		}
	}
	if strings.Contains(logs.String(), canary) {
		t.Fatal("raw backend error leaked into controller log")
	}
	for _, err := range []error{status.Error(codes.Internal, canary), fmt.Errorf("%w: %s", engine.ErrBusy, canary), fmt.Errorf("%w: %s", engine.ErrInvalidState, canary)} {
		if strings.Contains(mapError(err).Error(), canary) {
			t.Fatal("raw status error exposed")
		}
	}
}

func TestBackendStatusKeepsCodeWithoutMessageOrDetails(t *testing.T) {
	for _, code := range []codes.Code{codes.Canceled, codes.DeadlineExceeded, codes.Unavailable, codes.PermissionDenied, codes.Internal} {
		mapped := mapError(status.Error(code, "credential-canary"))
		if status.Code(mapped) != code || strings.Contains(mapped.Error(), "credential-canary") {
			t.Fatalf("unsafe status mapping: %v", mapped)
		}
	}
}
