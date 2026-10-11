package controller

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ListEnvironments emits durable, non-secret environment records as JSON.
func ListEnvironments(ctx context.Context, client kubernetes.Interface, namespace string, out io.Writer) error {
	store := &provider.ConfigMapStore{Client: client, Namespace: namespace}
	records, err := store.List(ctx)
	if err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return json.NewEncoder(out).Encode(records)
}

// InspectEnvironment returns one durable record. The ID is validated by the
// store before it is used as a Kubernetes resource name.
func InspectEnvironment(ctx context.Context, client kubernetes.Interface, namespace, id string, out io.Writer, retention ...time.Duration) error {
	store := &provider.ConfigMapStore{Client: client, Namespace: namespace}
	record, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, record.Job.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	podPresent := err == nil
	action, reason := engine.PlanReconciliationWithRetention(record, time.Now(), selectedRetention(retention))
	var retainedUntil *time.Time
	if record.State == engine.StateCleaned && !record.CleanedAt.IsZero() {
		until := record.CleanedAt.Add(selectedRetention(retention))
		retainedUntil = &until
	}
	return json.NewEncoder(out).Encode(struct {
		engine.Record
		PodPresent         bool                   `json:"pod_present"`
		PodIdentityMatches bool                   `json:"pod_identity_matches"`
		Action             engine.ReconcileAction `json:"cleanup_action"`
		Reason             string                 `json:"cleanup_reason"`
		RetainedUntil      *time.Time             `json:"retained_until,omitempty"`
	}{record, podPresent, podPresent && string(pod.UID) == record.Job.UID && pod.Labels["spawnpod.dev/environment"] == id && pod.Labels["spawnpod.dev/managed"] == "true", action, reason, retainedUntil})
}

func selectedRetention(values []time.Duration) time.Duration {
	if len(values) != 0 && values[0] > 0 {
		return values[0]
	}
	return engine.DefaultTombstoneRetention
}

type CleanupCandidate struct {
	ID     string                 `json:"id"`
	State  engine.State           `json:"state"`
	Action engine.ReconcileAction `json:"action"`
	Reason string                 `json:"reason"`
}

// PreviewCleanup computes the same record-level decisions as reconciliation.
// It cannot see in-flight operations held by a running controller.
func PreviewCleanup(ctx context.Context, client kubernetes.Interface, namespace string, now time.Time, out io.Writer, retention ...time.Duration) error {
	store := &provider.ConfigMapStore{Client: client, Namespace: namespace}
	records, err := store.List(ctx)
	if err != nil {
		return err
	}
	candidates := make([]CleanupCandidate, 0)
	for _, record := range records {
		action, reason := engine.PlanReconciliationWithRetention(record, now, selectedRetention(retention))
		if action != engine.ReconcileRetain {
			candidates = append(candidates, CleanupCandidate{ID: record.ID, State: record.State, Action: action, Reason: reason})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return json.NewEncoder(out).Encode(struct {
		Note       string             `json:"note"`
		Candidates []CleanupCandidate `json:"candidates"`
	}{Note: "Preview only; active controller operations may suppress a listed action.", Candidates: candidates})
}
