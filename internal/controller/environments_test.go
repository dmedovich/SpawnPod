package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnvironmentInspectionAndCleanupPreview(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	store := &provider.ConfigMapStore{Client: client, Namespace: "ci"}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.Create(ctx, engine.Record{Version: 1, ID: id, Owner: "job", Profile: "default", State: engine.StateFailed, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ListEnvironments(ctx, client, "ci", &out); err != nil {
		t.Fatal(err)
	}
	var list []engine.Record
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("unexpected list: %v %s", err, out.String())
	}
	out.Reset()
	if err := InspectEnvironment(ctx, client, "ci", id, &out); err != nil {
		t.Fatal(err)
	}
	var item engine.Record
	if err := json.Unmarshal(out.Bytes(), &item); err != nil || item.ID != id {
		t.Fatalf("unexpected inspect: %v %s", err, out.String())
	}
	out.Reset()
	if err := PreviewCleanup(ctx, client, "ci", now, &out); err != nil {
		t.Fatal(err)
	}
	var preview struct {
		Candidates []CleanupCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil || len(preview.Candidates) != 1 || preview.Candidates[0].Action != engine.ReconcileRemove {
		t.Fatalf("unexpected preview: %v %s", err, out.String())
	}
	if _, err := store.Get(ctx, id); err != nil {
		t.Fatalf("preview modified record: %v", err)
	}
	record, _ := store.Get(ctx, id)
	record.State = engine.StateCleaned
	record.CleanedAt = time.Now().Add(-10 * time.Minute)
	// The fake client does not allocate resourceVersions.
	record.Revision = "1"
	cm, _ := client.CoreV1().ConfigMaps("ci").Get(ctx, "sp-"+id, metav1.GetOptions{})
	cm.ResourceVersion = "1"
	if _, err := client.CoreV1().ConfigMaps("ci").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, record); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := InspectEnvironment(ctx, client, "ci", id, &out, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"pod_present":false`) || !strings.Contains(out.String(), `"cleanup_action":"expire-tombstone"`) || !strings.Contains(out.String(), `"retained_until"`) {
		t.Fatalf("missing retention diagnosis: %s", out.String())
	}
	out.Reset()
	if err := PreviewCleanup(ctx, client, "ci", time.Now(), &out, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "expire-tombstone") {
		t.Fatalf("preview ignored configured TTL: %s", out.String())
	}
}
