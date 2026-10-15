// Package serviceaccount implements the ServiceAccount controller, like
// k8s.io/kubernetes/pkg/controller/serviceaccount: every active namespace
// has a "default" service account, recreated if it is deleted. (The API
// server also creates it with the namespace, so Pods can start at once.)
package serviceaccount

import (
	"context"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/workqueue"
)

// Controller keeps the default service accounts.
type Controller struct {
	client     *client.Client
	namespaces *informers.Informer[corev1.Namespace, *corev1.Namespace]
	accounts   *informers.Informer[corev1.ServiceAccount, *corev1.ServiceAccount]
	queue      *workqueue.Queue
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, namespaces *informers.Informer[corev1.Namespace, *corev1.Namespace],
	accounts *informers.Informer[corev1.ServiceAccount, *corev1.ServiceAccount]) *Controller {
	sc := &Controller{client: c, namespaces: namespaces, accounts: accounts, queue: workqueue.New()}
	namespaces.AddHandler(informers.Handler[*corev1.Namespace]{
		OnAdd:    func(ns *corev1.Namespace) { sc.queue.Add(ns.Name) },
		OnUpdate: func(_, ns *corev1.Namespace) { sc.queue.Add(ns.Name) },
	})
	accounts.AddHandler(informers.Handler[*corev1.ServiceAccount]{
		OnDelete: func(sa *corev1.ServiceAccount) {
			if sa.Name == corev1.DefaultServiceAccountName {
				sc.queue.Add(sa.Namespace)
			}
		},
	})
	return sc
}

// Run processes namespaces until ctx ends.
func (sc *Controller) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, sc.queue, "serviceaccount", workers, sc.sync)
}

func (sc *Controller) sync(ctx context.Context, namespace string) error {
	ns, ok := sc.namespaces.Get("", namespace)
	if !ok || ns.DeletionTimestamp != nil || ns.Status.Phase == corev1.NamespaceTerminating {
		return nil
	}
	if _, ok := sc.accounts.Get(namespace, corev1.DefaultServiceAccountName); ok {
		return nil
	}
	_, err := client.ServiceAccounts(sc.client, namespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: corev1.DefaultServiceAccountName, Namespace: namespace}})
	if client.IsAlreadyExists(err) || client.IsNotFound(err) {
		return nil
	}
	return err
}
