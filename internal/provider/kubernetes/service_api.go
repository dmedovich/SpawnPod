package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ServiceHandle tracks the service Pod, ClusterIP and per-service policies.
// A missing UID represents a partial create or a resource already removed.
type ServiceHandle struct {
	Name             string
	PodUID           types.UID
	ServiceUID       types.UID
	EgressPolicyUID  types.UID
	IngressPolicyUID types.UID
}

func checkServiceIdentity(name, environmentID, serviceName string, labels map[string]string) error {
	if name != "sp-"+environmentID+"-"+serviceName || labels[ownerLabel] != "true" || labels[environmentLabel] != environmentID || labels[serviceLabel] != serviceName {
		return errors.New("service resource ownership mismatch")
	}
	return nil
}

// FindServiceResources recovers UIDs after an ambiguous API response. It never
// adopts a resource whose ownership labels differ from the requested identity.
func (p *Provider) FindServiceResources(ctx context.Context, environmentID, serviceName string) (ServiceHandle, error) {
	if err := p.validate(); err != nil {
		return ServiceHandle{}, err
	}
	if !idPattern.MatchString(environmentID) || !validServiceName(serviceName) {
		return ServiceHandle{}, errors.New("invalid service identity")
	}
	name := "sp-" + environmentID + "-" + serviceName
	handle := ServiceHandle{Name: name}
	egressName, ingressName := servicePolicyNames(environmentID, serviceName)
	egress, err := p.Client.NetworkingV1().NetworkPolicies(p.Namespace).Get(ctx, egressName, metav1.GetOptions{})
	if err == nil {
		if err := checkServicePolicyIdentity(egress, egressName, environmentID, serviceName); err != nil {
			return ServiceHandle{}, err
		}
		handle.EgressPolicyUID = egress.UID
	} else if !apierrors.IsNotFound(err) {
		return ServiceHandle{}, err
	}
	ingress, err := p.Client.NetworkingV1().NetworkPolicies(p.Namespace).Get(ctx, ingressName, metav1.GetOptions{})
	if err == nil {
		if err := checkServicePolicyIdentity(ingress, ingressName, environmentID, serviceName); err != nil {
			return ServiceHandle{}, err
		}
		handle.IngressPolicyUID = ingress.UID
	} else if !apierrors.IsNotFound(err) {
		return ServiceHandle{}, err
	}
	service, err := p.Client.CoreV1().Services(p.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if err := checkServiceIdentity(service.Name, environmentID, serviceName, service.Labels); err != nil {
			return ServiceHandle{}, err
		}
		handle.ServiceUID = service.UID
	} else if !apierrors.IsNotFound(err) {
		return ServiceHandle{}, err
	}
	pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if err := checkServiceIdentity(pod.Name, environmentID, serviceName, pod.Labels); err != nil {
			return ServiceHandle{}, err
		}
		handle.PodUID = pod.UID
	} else if !apierrors.IsNotFound(err) {
		return ServiceHandle{}, err
	}
	return handle, nil
}

// CreateServiceResources creates network permissions, then the ClusterIP
// Service, then its Pod. The
// caller must persist intent first and reconcile either resource after an
// error, since an API response can be lost after a successful create.
func (p *Provider) CreateServiceResources(ctx context.Context, spec ServiceSpec) (ServiceHandle, error) {
	if err := p.validate(); err != nil {
		return ServiceHandle{}, err
	}
	service, err := BuildService(spec)
	if err != nil {
		return ServiceHandle{}, err
	}
	pod, err := BuildServicePod(spec)
	if err != nil {
		return ServiceHandle{}, err
	}
	egress, ingress, err := BuildServicePolicies(spec)
	if err != nil {
		return ServiceHandle{}, err
	}
	handle := ServiceHandle{Name: service.Name}
	egress, err = p.Client.NetworkingV1().NetworkPolicies(p.Namespace).Create(ctx, egress, metav1.CreateOptions{})
	if err != nil {
		return handle, err
	}
	handle.EgressPolicyUID = egress.UID
	ingress, err = p.Client.NetworkingV1().NetworkPolicies(p.Namespace).Create(ctx, ingress, metav1.CreateOptions{})
	if err != nil {
		return handle, err
	}
	handle.IngressPolicyUID = ingress.UID
	service, err = p.Client.CoreV1().Services(p.Namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return handle, err
	}
	handle.ServiceUID = service.UID
	pod, err = p.Client.CoreV1().Pods(p.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return handle, err
	}
	handle.PodUID = pod.UID
	return handle, nil
}

// ResolveServiceAddress waits for the API-assigned ClusterIP before the job
// Pod is created. HostAliases cannot be changed on an existing Pod.
func (p *Provider) ResolveServiceAddress(ctx context.Context, spec ServiceSpec, handle ServiceHandle) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if _, err := validateServiceSpec(spec); err != nil {
		return "", err
	}
	if handle.Name != serviceResourceName(spec) || handle.ServiceUID == "" {
		return "", errors.New("ClusterIP Service identity is required")
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		service, err := p.Client.CoreV1().Services(p.Namespace).Get(ctx, handle.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if err := checkServiceDefinition(service, spec, handle.ServiceUID); err != nil {
			return "", err
		}
		if service.Spec.ClusterIP != "" && service.Spec.ClusterIP != corev1.ClusterIPNone {
			address, err := netip.ParseAddr(service.Spec.ClusterIP)
			if err != nil || !address.IsGlobalUnicast() {
				return "", errors.New("ClusterIP Service has invalid address")
			}
			return address.String(), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func checkServiceDefinition(service *corev1.Service, spec ServiceSpec, uid types.UID) error {
	if service.UID != uid || checkServiceIdentity(service.Name, spec.EnvironmentID, spec.Name, service.Labels) != nil || service.Spec.Type != corev1.ServiceTypeClusterIP || len(service.Spec.Selector) != len(serviceLabels(spec)) || len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != spec.Port || service.Spec.Ports[0].NodePort != 0 {
		return errors.New("ClusterIP Service identity or port changed")
	}
	for key, value := range serviceLabels(spec) {
		if service.Spec.Selector[key] != value {
			return errors.New("ClusterIP Service selector changed")
		}
	}
	return nil
}

// ServiceAlive checks terminal loss of an already ready service. A transient
// API error is returned to the caller so it does not remove a healthy Pod.
func (p *Provider) ServiceAlive(ctx context.Context, spec ServiceSpec, handle ServiceHandle) (bool, error) {
	if err := p.validate(); err != nil {
		return false, err
	}
	if handle.Name != serviceResourceName(spec) || handle.PodUID == "" || handle.ServiceUID == "" {
		return false, errors.New("invalid service identity")
	}
	pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, handle.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pod.UID != handle.PodUID || checkServiceIdentity(pod.Name, spec.EnvironmentID, spec.Name, pod.Labels) != nil {
		return false, errors.New("service Pod identity changed")
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return false, nil
	}
	service, err := p.Client.CoreV1().Services(p.Namespace).Get(ctx, handle.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := checkServiceDefinition(service, spec, handle.ServiceUID); err != nil {
		return false, err
	}
	return true, nil
}

// WaitServiceReady requires the recorded Pod and Service UIDs, assigned
// ClusterIP, and a healthy TCP readiness probe. Endpoint routing still needs
// verification in a real cluster.
func (p *Provider) WaitServiceReady(ctx context.Context, spec ServiceSpec, handle ServiceHandle) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if _, err := validateServiceSpec(spec); err != nil {
		return "", err
	}
	if handle.Name != serviceResourceName(spec) || handle.PodUID == "" || handle.ServiceUID == "" {
		return "", errors.New("service resource UIDs are required")
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		service, err := p.Client.CoreV1().Services(p.Namespace).Get(ctx, handle.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if err := checkServiceDefinition(service, spec, handle.ServiceUID); err != nil {
			return "", err
		}
		pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, handle.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if pod.UID != handle.PodUID || checkServiceIdentity(pod.Name, spec.EnvironmentID, spec.Name, pod.Labels) != nil {
			return "", errors.New("service Pod identity changed")
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return "", fmt.Errorf("service Pod finished before readiness: %s", pod.Status.Phase)
		}
		if service.Spec.ClusterIP != "" && service.Spec.ClusterIP != corev1.ClusterIPNone {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					return service.Spec.ClusterIP, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// RemoveServiceResources checks current ownership and UIDs before deletion.
// The Pod is removed first to stop the service process, followed by the
// Service and policies. Missing resources are treated as already removed.
func (p *Provider) RemoveServiceResources(ctx context.Context, environmentID, serviceName string, recorded ServiceHandle) error {
	current, err := p.FindServiceResources(ctx, environmentID, serviceName)
	if err != nil {
		return err
	}
	if recorded.Name != "" && recorded.Name != current.Name {
		return errors.New("recorded service name mismatch")
	}
	if recorded.PodUID != "" && current.PodUID != "" && recorded.PodUID != current.PodUID {
		return errors.New("recorded service Pod UID changed")
	}
	if recorded.ServiceUID != "" && current.ServiceUID != "" && recorded.ServiceUID != current.ServiceUID {
		return errors.New("recorded ClusterIP Service UID changed")
	}
	if recorded.EgressPolicyUID != "" && current.EgressPolicyUID != "" && recorded.EgressPolicyUID != current.EgressPolicyUID {
		return errors.New("recorded service egress policy UID changed")
	}
	if recorded.IngressPolicyUID != "" && current.IngressPolicyUID != "" && recorded.IngressPolicyUID != current.IngressPolicyUID {
		return errors.New("recorded service ingress policy UID changed")
	}
	zero := int64(0)
	if current.PodUID != "" {
		err := p.Client.CoreV1().Pods(p.Namespace).Delete(ctx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.PodUID}, GracePeriodSeconds: &zero})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if current.ServiceUID != "" {
		err := p.Client.CoreV1().Services(p.Namespace).Delete(ctx, current.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &current.ServiceUID}})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	egressName, ingressName := servicePolicyNames(environmentID, serviceName)
	if current.EgressPolicyUID != "" {
		uid := current.EgressPolicyUID
		err := p.Client.NetworkingV1().NetworkPolicies(p.Namespace).Delete(ctx, egressName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if current.IngressPolicyUID != "" {
		uid := current.IngressPolicyUID
		err := p.Client.NetworkingV1().NetworkPolicies(p.Namespace).Delete(ctx, ingressName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		found, err := p.FindServiceResources(ctx, environmentID, serviceName)
		if err != nil {
			return err
		}
		if found.PodUID == "" && found.ServiceUID == "" && found.EgressPolicyUID == "" && found.IngressPolicyUID == "" {
			return nil
		}
		if (found.PodUID != "" && found.PodUID != current.PodUID) || (found.ServiceUID != "" && found.ServiceUID != current.ServiceUID) || (found.EgressPolicyUID != "" && found.EgressPolicyUID != current.EgressPolicyUID) || (found.IngressPolicyUID != "" && found.IngressPolicyUID != current.IngressPolicyUID) {
			return errors.New("service resource replaced during deletion")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
