package kubernetes

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestCopyRejectsWrongPodUIDBeforeExec(t *testing.T) {
	id := strings.Repeat("a", 32)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sp-" + id, Namespace: "ci", UID: types.UID("new-uid")}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	provider := &Provider{Client: fake.NewClientset(pod), Config: &rest.Config{Host: "https://example.invalid"}, Namespace: "ci"}
	handle := engine.JobHandle{Name: pod.Name, UID: "old-uid"}
	if err := provider.CopyIn(context.Background(), handle, "/workspace", bytes.NewReader(nil)); err == nil {
		t.Fatal("CopyIn accepted replaced Pod")
	}
	if err := provider.CopyOut(context.Background(), handle, "/workspace/file", &bytes.Buffer{}); err == nil {
		t.Fatal("CopyOut accepted replaced Pod")
	}
}
