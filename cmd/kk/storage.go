package main

import (
	"context"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
)

// podsUsingClaim lists the Pods that mount a claim, for kk describe pvc.
func (a *app) podsUsingClaim(ctx context.Context, pvc *corev1.PersistentVolumeClaim) ([]string, error) {
	pods, err := client.Pods(a.client, pvc.Namespace).List(ctx, client.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range pods.Items {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvc.Name {
				names = append(names, p.Name)
				break
			}
		}
	}
	return names, nil
}
