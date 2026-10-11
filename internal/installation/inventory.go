package installation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

func getObject(ctx context.Context, c kubernetes.Interface, o Object) (runtime.Object, error) {
	opt := metav1.GetOptions{}
	switch o.Kind {
	case "Namespace":
		return c.CoreV1().Namespaces().Get(ctx, o.Name, opt)
	case "ServiceAccount":
		return c.CoreV1().ServiceAccounts(o.Namespace).Get(ctx, o.Name, opt)
	case "Role":
		return c.RbacV1().Roles(o.Namespace).Get(ctx, o.Name, opt)
	case "RoleBinding":
		return c.RbacV1().RoleBindings(o.Namespace).Get(ctx, o.Name, opt)
	case "ClusterRole":
		return c.RbacV1().ClusterRoles().Get(ctx, o.Name, opt)
	case "ClusterRoleBinding":
		return c.RbacV1().ClusterRoleBindings().Get(ctx, o.Name, opt)
	case "NetworkPolicy":
		return c.NetworkingV1().NetworkPolicies(o.Namespace).Get(ctx, o.Name, opt)
	case "ValidatingAdmissionPolicy":
		return c.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, o.Name, opt)
	case "ValidatingAdmissionPolicyBinding":
		return c.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, o.Name, opt)
	case "LimitRange":
		return c.CoreV1().LimitRanges(o.Namespace).Get(ctx, o.Name, opt)
	case "ResourceQuota":
		return c.CoreV1().ResourceQuotas(o.Namespace).Get(ctx, o.Name, opt)
	case "ConfigMap":
		return c.CoreV1().ConfigMaps(o.Namespace).Get(ctx, o.Name, opt)
	case "Deployment":
		return c.AppsV1().Deployments(o.Namespace).Get(ctx, o.Name, opt)
	case "Service":
		return c.CoreV1().Services(o.Namespace).Get(ctx, o.Name, opt)
	default:
		return nil, fmt.Errorf("unsupported managed kind %s", o.Kind)
	}
}
func (r *Report) checkInventory(ctx context.Context, b *Bundle, c kubernetes.Interface, phase string) {
	for _, file := range manifestNames() {
		if phase == "switch" && file == "30-deployment.yaml" {
			continue
		}
		objects, err := DecodeObjects(b.Files[file])
		if err != nil {
			r.add("bundle.decode", err)
			continue
		}
		for _, want := range objects {
			// These are compared strictly by the shared semantic startup validator.
			switch want.GetKind() {
			case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "LimitRange", "ResourceQuota", "ConfigMap", "Deployment":
				continue
			}
			id := Object{Kind: want.GetKind(), Namespace: want.GetNamespace(), Name: want.GetName()}
			actual, err := getObject(ctx, c, id)
			if err == nil {
				encoded, _ := json.Marshal(actual)
				var live map[string]any
				_ = json.Unmarshal(encoded, &live)
				if want.GetKind() == "ClusterRole" && live["aggregationRule"] != nil {
					err = fmt.Errorf("unexpected aggregationRule may grant additional permissions")
				}
				for _, field := range []string{"rules", "roleRef", "subjects", "automountServiceAccountToken", "spec"} {
					if expected, ok := want.Object[field]; ok {
						a, _ := json.Marshal(live[field])
						e, _ := json.Marshal(expected)
						var av, ev any
						_ = json.Unmarshal(a, &av)
						_ = json.Unmarshal(e, &ev)
						mismatch := !reflect.DeepEqual(av, ev)
						if want.GetKind() == "Service" {
							mismatch = subsetDifference(field, av, ev) != ""
						}
						if mismatch {
							err = fmt.Errorf("managed field %s differs from bundle (values withheld)", field)
							break
						}
					}
				}
				if err == nil {
					meta, _ := live["metadata"].(map[string]any)
					desired, _ := want.Object["metadata"].(map[string]any)
					if field := subsetDifference("metadata.labels", meta["labels"], desired["labels"]); desired["labels"] != nil && field != "" {
						err = fmt.Errorf("%s differs from bundle", field)
					}
				}
			}
			r.add(fmt.Sprintf("inventory.%s/%s/%s", id.Kind, id.Namespace, id.Name), err)
		}
	}
}
