// Package registry wires the per-resource strategies into the API server,
// like the REST storage providers of k8s.io/kubernetes/pkg/controlplane.
package registry

import (
	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/registry/apps/controllerrevision"
	"mini-k8s/pkg/registry/apps/daemonset"
	"mini-k8s/pkg/registry/apps/deployment"
	"mini-k8s/pkg/registry/apps/replicaset"
	"mini-k8s/pkg/registry/batch/cronjob"
	"mini-k8s/pkg/registry/batch/job"
	"mini-k8s/pkg/registry/coordination/lease"
	"mini-k8s/pkg/registry/core/configmap"
	"mini-k8s/pkg/registry/core/endpoints"
	"mini-k8s/pkg/registry/core/event"
	"mini-k8s/pkg/registry/core/namespace"
	"mini-k8s/pkg/registry/core/node"
	"mini-k8s/pkg/registry/core/persistentvolume"
	"mini-k8s/pkg/registry/core/persistentvolumeclaim"
	"mini-k8s/pkg/registry/core/pod"
	"mini-k8s/pkg/registry/core/secret"
	"mini-k8s/pkg/registry/core/service"
	"mini-k8s/pkg/registry/networking/ingress"
	"mini-k8s/pkg/registry/storage/storageclass"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/storage/etcd3"
)

// Registry holds the strategy of every served resource and the background
// loop the Service allocators need.
type Registry struct {
	// Strategies is keyed by resource; pass it to apiserver.Config.
	Strategies map[string]apiserver.Strategy
	// ServiceAllocators hand out cluster IPs and node ports.
	ServiceAllocators *service.Allocators
	// ServiceRepair reconciles them with the stored Services.
	ServiceRepair *service.Repair
}

// New returns the registry for the objects stored in store under prefix
// (the API server's etcd prefix, "/registry" by default).
func New(store *etcd3.Store, prefix string) *Registry {
	if prefix == "" {
		prefix = "/registry"
	}
	allocators := service.NewAllocators(store, prefix)
	return &Registry{
		Strategies: map[string]apiserver.Strategy{
			scheme.Namespaces.Resource:             namespace.Strategy,
			scheme.Nodes.Resource:                  node.Strategy,
			scheme.Pods.Resource:                   pod.Strategy,
			scheme.Services.Resource:               service.NewStrategy(allocators),
			scheme.Endpoints.Resource:              endpoints.Strategy,
			scheme.Events.Resource:                 event.Strategy,
			scheme.ConfigMaps.Resource:             configmap.Strategy,
			scheme.Secrets.Resource:                secret.Strategy,
			scheme.PersistentVolumes.Resource:      persistentvolume.Strategy,
			scheme.PersistentVolumeClaims.Resource: persistentvolumeclaim.NewStrategy(store, prefix),
			scheme.StorageClasses.Resource:         storageclass.Strategy,
			scheme.Deployments.Resource:            deployment.Strategy,
			scheme.ReplicaSets.Resource:            replicaset.Strategy,
			scheme.DaemonSets.Resource:             daemonset.Strategy,
			scheme.ControllerRevisions.Resource:    controllerrevision.Strategy,
			scheme.Jobs.Resource:                   job.Strategy,
			scheme.CronJobs.Resource:               cronjob.Strategy,
			scheme.Ingresses.Resource:              ingress.Strategy,
			scheme.Leases.Resource:                 lease.Strategy,
		},
		ServiceAllocators: allocators,
		ServiceRepair:     service.NewRepair(store, prefix, allocators),
	}
}
