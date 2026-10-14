// Package registry wires the per-resource strategies into the API server,
// like the REST storage providers of k8s.io/kubernetes/pkg/controlplane.
package registry

import (
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/registry/apps/deployment"
	"mini-k8s/pkg/registry/apps/replicaset"
	"mini-k8s/pkg/registry/core/event"
	"mini-k8s/pkg/registry/core/namespace"
	"mini-k8s/pkg/registry/core/node"
	"mini-k8s/pkg/registry/core/pod"
	"mini-k8s/pkg/registry/core/service"
	"mini-k8s/pkg/scheme"
)

// Strategies returns the strategy of every served resource, keyed by resource.
func Strategies() map[string]apiserver.Strategy {
	return map[string]apiserver.Strategy{
		scheme.Namespaces.Resource:  namespace.Strategy,
		scheme.Nodes.Resource:       node.Strategy,
		scheme.Pods.Resource:        pod.Strategy,
		scheme.Services.Resource:    service.Strategy,
		scheme.Events.Resource:      event.Strategy,
		scheme.Deployments.Resource: deployment.Strategy,
		scheme.ReplicaSets.Resource: replicaset.Strategy,
	}
}
