package acceptance

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmedovich/spawnpod/internal/installation"
	"github.com/dmedovich/spawnpod/internal/sourceproxy"
	yaml "go.yaml.in/yaml/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Only test infra gets a loopback route and a TLS frontend to local Forgejo.
// Production examples remain public-IP-only and do not contain this sidecar.
func (c *cluster) sourceTLSFixture(t *testing.T, manifest []byte, host, address string) ([]byte, string, string) {
	t.Helper()
	origin, err := url.Parse(os.Getenv("SPAWNPOD_SOURCE_FORGEJO_ORIGIN"))
	if err != nil || origin.Scheme != "http" || origin.User != nil || origin.Port() != "3001" || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		t.Fatal("source transport gate requires an explicit local node HTTP forwarder on port 3001")
	}
	nodeIP, err := netip.ParseAddr(origin.Hostname())
	if err != nil || !nodeIP.Is4() {
		t.Fatal("source fixture requires literal IPv4 node origin")
	}
	nodes, err := c.admin.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	verifiedNode := false
	for _, node := range nodes.Items {
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && a.Address == nodeIP.String() {
				verifiedNode = true
			}
		}
	}
	if !verifiedNode {
		t.Fatal("TLS fixture origin is not this local cluster's node")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "SpawnPod disposable RC7 fixture"}, DNSNames: []string{"forgejo.rc7.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	ns := c.cfg.Kubernetes.LeaseNamespace
	_, err = c.admin.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rc7-tls-fixture", Namespace: ns}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": certPEM, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateDER})}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(c.evidence, "fixture-ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../source-tls-fixture.js")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.admin.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rc7-tls-fixture-code", Namespace: ns}, Data: map[string]string{"fixture.js": string(script)}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := installation.DecodeObjects(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var output []byte
	for _, object := range objects {
		data, err := json.Marshal(object.Object)
		if err != nil {
			t.Fatal(err)
		}
		var modified any = object.Object
		switch object.GetKind() {
		case "ConfigMap":
			var cm corev1.ConfigMap
			if err := json.Unmarshal(data, &cm); err != nil {
				t.Fatal(err)
			}
			routes := struct {
				Listen         string              `yaml:"listen"`
				MaxConnections int                 `yaml:"maxConnections"`
				Routes         []sourceproxy.Route `yaml:"routes"`
			}{"0.0.0.0:3128", 128, []sourceproxy.Route{{Host: host, Port: 443, Address: address}, {Host: "forgejo.rc7.test", Port: 3443, Address: "127.0.0.1", AllowLoopback: true}}}
			encoded, err := yaml.Marshal(routes)
			if err != nil {
				t.Fatal(err)
			}
			cm.Data["config.yaml"] = string(encoded)
			modified = cm
		case "NetworkPolicy":
			var policy networkingv1.NetworkPolicy
			if err := json.Unmarshal(data, &policy); err != nil {
				t.Fatal(err)
			}
			protocol := corev1.ProtocolTCP
			port := intstr.FromInt32(3001)
			policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: netip.PrefixFrom(nodeIP, 32).String()}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}}})
			modified = policy
		case "Deployment":
			var deployment appsv1.Deployment
			if err := json.Unmarshal(data, &deployment); err != nil {
				t.Fatal(err)
			}
			pod := &deployment.Spec.Template.Spec
			pod.Volumes = append(pod.Volumes,
				corev1.Volume{Name: "fixture-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "rc7-tls-fixture"}}},
				corev1.Volume{Name: "fixture-code", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "rc7-tls-fixture-code"}}}})
			security := pod.Containers[0].SecurityContext.DeepCopy()
			resources := pod.Containers[0].Resources.DeepCopy()
			pod.Containers = append(pod.Containers, corev1.Container{Name: "tls-fixture", Image: c.cfg.Profiles["source"].Image, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"node", "/fixture/fixture.js", origin.String()}, SecurityContext: security, Resources: *resources, VolumeMounts: []corev1.VolumeMount{{Name: "fixture-tls", MountPath: "/tls", ReadOnly: true}, {Name: "fixture-code", MountPath: "/fixture", ReadOnly: true}}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"node", "-e", "const s=require('net').connect(3443,'127.0.0.1',()=>{s.end();process.exit(0)});s.on('error',()=>process.exit(1))"}}}, TimeoutSeconds: 5, PeriodSeconds: 2}})
			modified = deployment
		}
		encoded, err := json.Marshal(modified)
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, encoded...)
		output = append(output, []byte("\n---\n")...)
	}
	return output, caPath, serial.Text(16)
}
