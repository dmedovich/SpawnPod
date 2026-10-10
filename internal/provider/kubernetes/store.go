package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dmedovich/spawnpod/internal/engine"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const recordKey = "record.json"

// ConfigMapStore persists only non-secret environment management state.
// ResourceVersion provides compare-and-swap updates across controller restarts.
type ConfigMapStore struct {
	Client    kubernetes.Interface
	Namespace string
}

func (s *ConfigMapStore) validate() error {
	if s == nil || s.Client == nil || s.Namespace == "" {
		return errors.New("ConfigMap store is not configured")
	}
	return nil
}

func recordMap(record engine.Record) (*corev1.ConfigMap, error) {
	if (record.Version != 1 && record.Version != 2) || record.ID == "" || (record.Version == 1 && len(record.Services) != 0) {
		return nil, errors.New("unsupported or empty environment record")
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sp-" + record.ID, ResourceVersion: record.Revision, Labels: map[string]string{ownerLabel: "true", environmentLabel: record.ID}}, Data: map[string]string{recordKey: string(payload)}}, nil
}

func parseRecord(configMap *corev1.ConfigMap) (engine.Record, error) {
	if configMap.Labels[ownerLabel] != "true" {
		return engine.Record{}, errors.New("ConfigMap is not owned by spawnPod")
	}
	var record engine.Record
	if err := json.Unmarshal([]byte(configMap.Data[recordKey]), &record); err != nil {
		return engine.Record{}, fmt.Errorf("decode environment record: %w", err)
	}
	if (record.Version != 1 && record.Version != 2) || (record.Version == 1 && len(record.Services) != 0) || record.ID == "" || configMap.Name != "sp-"+record.ID || configMap.Labels[environmentLabel] != record.ID {
		return engine.Record{}, errors.New("environment record identity mismatch")
	}
	record.Revision = configMap.ResourceVersion
	return record, nil
}

func (s *ConfigMapStore) Create(ctx context.Context, record engine.Record) (engine.Record, error) {
	if err := s.validate(); err != nil {
		return engine.Record{}, err
	}
	cm, err := recordMap(record)
	if err != nil {
		return engine.Record{}, err
	}
	cm, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return engine.Record{}, err
	}
	return parseRecord(cm)
}

func (s *ConfigMapStore) Get(ctx context.Context, id string) (engine.Record, error) {
	if err := s.validate(); err != nil {
		return engine.Record{}, err
	}
	if !idPattern.MatchString(id) {
		return engine.Record{}, engine.ErrNotFound
	}
	cm, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, "sp-"+id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return engine.Record{}, engine.ErrNotFound
	}
	if err != nil {
		return engine.Record{}, err
	}
	return parseRecord(cm)
}

func (s *ConfigMapStore) Update(ctx context.Context, record engine.Record) (engine.Record, error) {
	if err := s.validate(); err != nil {
		return engine.Record{}, err
	}
	if record.Revision == "" {
		return engine.Record{}, engine.ErrConflict
	}
	current, err := s.Get(ctx, record.ID)
	if err != nil {
		return engine.Record{}, err
	}
	if current.Revision != record.Revision {
		return engine.Record{}, engine.ErrConflict
	}
	cm, err := recordMap(record)
	if err != nil {
		return engine.Record{}, err
	}
	cm, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return engine.Record{}, engine.ErrConflict
	}
	if err != nil {
		return engine.Record{}, err
	}
	return parseRecord(cm)
}

func (s *ConfigMapStore) List(ctx context.Context) ([]engine.Record, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	list, err := s.Client.CoreV1().ConfigMaps(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: ownerLabel + "=true"})
	if err != nil {
		return nil, err
	}
	records := make([]engine.Record, 0, len(list.Items))
	for i := range list.Items {
		record, err := parseRecord(&list.Items[i])
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// Delete removes a tombstone only when its resourceVersion still matches.
func (s *ConfigMapStore) Delete(ctx context.Context, id, revision string) error {
	if err := s.validate(); err != nil {
		return err
	}
	if !idPattern.MatchString(id) || revision == "" {
		return errors.New("invalid tombstone identity")
	}
	record, err := s.Get(ctx, id)
	if errors.Is(err, engine.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Revision != revision {
		return engine.ErrConflict
	}
	err = s.Client.CoreV1().ConfigMaps(s.Namespace).Delete(ctx, "sp-"+id, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &revision}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if apierrors.IsConflict(err) {
		return engine.ErrConflict
	}
	return err
}
