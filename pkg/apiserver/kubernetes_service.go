package apiserver

import (
	"context"
	"log"
	"reflect"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/intstr"
)

// KubernetesService describes the default/kubernetes Service through
// which Pods reach the API server.
type KubernetesService struct {
	// ClusterIP is the first address of the Service CIDR (10.96.0.1).
	ClusterIP string
	// Address and Port are where this API server listens.
	Address string
	Port    int32
}

// EnsureKubernetesService creates or repairs default/kubernetes and its
// Endpoints, like kube-apiserver's bootstrap controller and endpoint
// reconciler. The Service port is 443, named https, as in Kubernetes;
// mini's API is plain HTTP.
func (s *Server) EnsureKubernetesService(ctx context.Context, k KubernetesService) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: corev1.NamespaceDefault,
			Labels: map[string]string{"component": "apiserver", "provider": "kubernetes"}},
		Spec: corev1.ServiceSpec{ClusterIP: k.ClusterIP, Ports: []corev1.ServicePort{
			{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(int(k.Port))}}},
	}
	if err := s.ensure(ctx, scheme.Services, svc, func(cur metav1.Object) bool {
		c := cur.(*corev1.Service)
		return reflect.DeepEqual(c.Labels, svc.Labels) && reflect.DeepEqual(c.Spec.Ports, svc.Spec.Ports) && c.Spec.Selector == nil
	}); err != nil {
		return err
	}
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: corev1.NamespaceDefault},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: k.Address}},
			Ports:     []corev1.EndpointPort{{Name: "https", Port: k.Port, Protocol: corev1.ProtocolTCP}},
		}},
	}
	return s.ensure(ctx, scheme.Endpoints, ep, func(cur metav1.Object) bool {
		return reflect.DeepEqual(cur.(*corev1.Endpoints).Subsets, ep.Subsets)
	})
}

// ensure creates want, or updates the stored object when current reports it
// differs (keeping server-owned fields such as the cluster IP).
func (s *Server) ensure(ctx context.Context, res scheme.ResourceInfo, want metav1.Object, current func(metav1.Object) bool) error {
	m := want.GetObjectMeta()
	info := requestInfo{res: res, namespace: m.Namespace, name: m.Name}
	cur, err := s.get(ctx, res, m.Namespace, m.Name)
	if isNotFound(err) {
		_, err = s.createFlow(ctx, info, want, false)
		return err
	}
	if err != nil || current(cur) {
		return err
	}
	m.ResourceVersion = cur.GetObjectMeta().ResourceVersion
	_, err = s.updateFlow(ctx, info, want, false)
	return err
}

// RunKubernetesService keeps default/kubernetes current until ctx ends.
func (s *Server) RunKubernetesService(ctx context.Context, k KubernetesService, interval time.Duration) {
	for {
		if err := s.EnsureKubernetesService(ctx, k); err != nil && ctx.Err() == nil {
			log.Printf("kube-apiserver: kubernetes Service: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
