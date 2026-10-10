package engine

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/dmedovich/spawnpod/internal/supervisor"
)

type State string

const (
	StateProvisioning  State = "provisioning"
	StateReady         State = "ready"
	StateExecuting     State = "executing"
	StateTransferring  State = "transferring"
	StateFailed        State = "failed"
	StateIndeterminate State = "indeterminate"
	StateTerminating   State = "terminating"
	StateCleaned       State = "cleaned"
)

var (
	ErrNotFound     = errors.New("environment not found")
	ErrBusy         = errors.New("environment has an active operation")
	ErrInvalidState = errors.New("environment is not in the required state")
)

// JobSpec contains only policy-approved values. EnvironmentID is assigned by
// Engine. No workflow-provided PodSpec, mount or security context enters Jobs.
type JobSpec struct {
	EnvironmentID  string
	Network        string
	ServiceAliases []ServiceAlias
	Architecture   string
	Image          string
	AgentImage     string
	UID            int64
	GID            int64
	CPURequest     string
	CPULimit       string
	MemoryRequest  string
	MemoryLimit    string
	StorageRequest string
	StorageLimit   string
	WorkspaceSize  string
}

// ServiceAlias maps a workflow service name to its assigned ClusterIP inside
// the job Pod's /etc/hosts file. The engine derives it from owned Services.
type ServiceAlias struct {
	Name string
	IP   string
}

// ServiceSpec is an approved, non-secret snapshot of one service. The engine
// assigns EnvironmentID before persisting intent or creating resources.
type ServiceSpec struct {
	EnvironmentID  string   `json:"environment_id"`
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	AgentImage     string   `json:"agent_image"`
	Command        []string `json:"command"`
	Architecture   string   `json:"architecture"`
	UID            int64    `json:"uid"`
	GID            int64    `json:"gid"`
	Port           int32    `json:"port"`
	CPURequest     string   `json:"cpu_request"`
	CPULimit       string   `json:"cpu_limit"`
	MemoryRequest  string   `json:"memory_request"`
	MemoryLimit    string   `json:"memory_limit"`
	StorageRequest string   `json:"storage_request"`
	StorageLimit   string   `json:"storage_limit"`
}

type ServiceHandle struct {
	Name             string `json:"name"`
	PodUID           string `json:"pod_uid,omitempty"`
	ServiceUID       string `json:"service_uid,omitempty"`
	EgressPolicyUID  string `json:"egress_policy_uid,omitempty"`
	IngressPolicyUID string `json:"ingress_policy_uid,omitempty"`
}

type ServiceRecord struct {
	Spec      ServiceSpec   `json:"spec"`
	Handle    ServiceHandle `json:"handle"`
	ClusterIP string        `json:"cluster_ip,omitempty"`
}

// RuntimeMetadata is returned from the running agent and never persisted.
type RuntimeMetadata struct {
	OS          string            `json:"os"`
	Arch        string            `json:"arch"`
	Environment map[string]string `json:"env"`
}

type JobHandle struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// Record is non-secret management state. Revision is the storage CAS token and
// is not serialized into the ConfigMap payload.
type Record struct {
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	Owner         string          `json:"owner"`
	Profile       string          `json:"profile"`
	CreatedAt     time.Time       `json:"created_at"`
	Deadline      time.Time       `json:"deadline"`
	CleanedAt     time.Time       `json:"cleaned_at,omitempty"`
	State         State           `json:"state"`
	Job           JobHandle       `json:"job"`
	Services      []ServiceRecord `json:"services,omitempty"`
	LastCommandID string          `json:"last_command_id,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	Revision      string          `json:"-"`
}

// Store must implement compare-and-swap Update using Record.Revision. A
// conflict is returned as an error; Engine re-reads rather than overwriting
// another state transition.
type Store interface {
	Create(context.Context, Record) (Record, error)
	Get(context.Context, string) (Record, error)
	Update(context.Context, Record) (Record, error)
	List(context.Context) ([]Record, error)
	Delete(context.Context, string, string) error
}

// Jobs wraps a provider without exposing platform API types to Engine.
type Jobs interface {
	CreateJob(context.Context, JobSpec) (JobHandle, error)
	FindJob(context.Context, string) (JobHandle, error)
	WaitReady(context.Context, JobHandle) error
	Metadata(context.Context, JobHandle) (RuntimeMetadata, error)
	Exec(context.Context, JobHandle, supervisor.Request, func(supervisor.Event) error) error
	CopyIn(context.Context, JobHandle, string, io.Reader) error
	CopyOut(context.Context, JobHandle, string, io.Writer) error
	CancelCommand(context.Context, JobHandle, string) error
	RemoveJob(context.Context, JobHandle) error
}

// Services owns Kubernetes resources and network permissions for each service.
// Remove must recover and delete a partial create using the persisted intent.
type Services interface {
	CreateService(context.Context, ServiceSpec) (ServiceHandle, error)
	ResolveServiceAddress(context.Context, ServiceSpec, ServiceHandle) (string, error)
	StartService(context.Context, ServiceSpec, ServiceHandle, map[string]string, time.Duration) error
	WaitServiceReady(context.Context, ServiceSpec, ServiceHandle) (string, error)
	ServiceAlive(context.Context, ServiceSpec, ServiceHandle) (bool, error)
	RemoveService(context.Context, ServiceSpec, ServiceHandle) error
}
