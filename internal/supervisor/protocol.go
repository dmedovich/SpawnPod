package supervisor

import (
	"errors"
	"time"
)

// Request is one operation sent to the supervisor. Env values are transmitted
// in the request body, never as process arguments.
type Request struct {
	Op            string            `json:"op"`
	ID            string            `json:"id"`
	Argv          []string          `json:"argv,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Dir           string            `json:"dir,omitempty"`
	TimeoutMillis int64             `json:"timeout_ms,omitempty"`
}

// Event is either output on one stream or the final result. ExitCode is set
// only for an ordinary process exit; Error indicates an infrastructure or
// cancellation failure. Exactly one terminal event ends a successful exchange.
type Event struct {
	Stream   string `json:"stream,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (r Request) validate(maxTimeout time.Duration) error {
	if r.Op == "ping" {
		return nil
	}
	if r.ID == "" {
		return errors.New("command ID is required")
	}
	if r.Op == "cancel" {
		return nil
	}
	if r.Op != "run" {
		return errors.New("unknown operation")
	}
	if len(r.Argv) == 0 || r.Argv[0] == "" {
		return errors.New("command argv is required")
	}
	if r.TimeoutMillis <= 0 || r.TimeoutMillis > maxTimeout.Milliseconds() {
		return errors.New("command timeout is outside policy")
	}
	for k := range r.Env {
		if k == "" || len(k) > 255 {
			return errors.New("invalid environment variable name")
		}
		for _, c := range k {
			if c == '=' || c == 0 {
				return errors.New("invalid environment variable name")
			}
		}
	}
	return nil
}
