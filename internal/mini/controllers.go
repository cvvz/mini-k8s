package mini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

func Loop(ctx context.Context, interval time.Duration, name string, tick func(context.Context) error) {
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		if err := tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("%s: %v", name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
func Schedule(ctx context.Context, c *Client) error {
	nodes, err := c.List(ctx, "Node")
	if err != nil {
		return err
	}
	pods, err := c.List(ctx, "Pod")
	if err != nil {
		return err
	}
	for i := range pods {
		p := pods[i]
		if p.Status.NodeName != "" {
			continue
		}
		p.Status.NodeName = ChooseNode(nodes, pods, time.Now())
		p.Status.Phase = "Pending"
		p.Status.Reason = ""
		if p.Status.NodeName == "" {
			p.Status.Reason = "Unschedulable: no Ready nodes"
		}
		if err := c.Update(ctx, p, true); err != nil {
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
				continue
			}
			return err
		}
		pods[i] = p
	}
	return nil
}
func ReconcileDeployments(ctx context.Context, c *Client) error {
	ds, err := c.List(ctx, "Deployment")
	if err != nil {
		return err
	}
	pods, err := c.List(ctx, "Pod")
	if err != nil {
		return err
	}
	owners := map[string]bool{}
	for _, d := range ds {
		owners[d.Metadata.UID] = true
	}
	for _, p := range pods {
		if p.Metadata.OwnerUID != "" && !owners[p.Metadata.OwnerUID] {
			if err := c.Delete(ctx, p); err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
				return err
			}
		}
	}
	for _, d := range ds {
		var spec DeploymentSpec
		if err := json.Unmarshal(d.Spec, &spec); err != nil {
			return err
		}
		mine := OwnedPods(d, pods)
		for i := len(mine); i < *spec.Replicas; i++ {
			name := d.Metadata.Name
			if len(name) > 40 {
				name = name[:40]
			}
			body, err := json.Marshal(spec.Template.Spec)
			if err != nil {
				return err
			}
			p := Resource{APIVersion: "v1", Kind: "Pod", Metadata: Metadata{
				Name: name + "-" + UID()[:10], Namespace: "default",
				Labels: spec.Template.Metadata.Labels, OwnerUID: d.Metadata.UID,
			}, Spec: body}
			if _, err := c.Create(ctx, p); err != nil {
				return err
			}
		}
		for i := *spec.Replicas; i < len(mine); i++ {
			if err := c.Delete(ctx, mine[i]); err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
				return err
			}
		}
	}
	return nil
}
func Heartbeat(ctx context.Context, c *Client, node string) error {
	r, err := c.Get(ctx, "Node", node)
	if errors.Is(err, ErrNotFound) {
		r, err = c.Create(ctx, Resource{APIVersion: "v1", Kind: "Node", Metadata: Metadata{Name: node}, Spec: json.RawMessage(`{}`)})
	}
	if err != nil {
		return err
	}
	r.Status = Status{Phase: "Ready", Heartbeat: time.Now().UTC()}
	return c.Update(ctx, r, true)
}
func ReconcileNode(ctx context.Context, c *Client, d *Docker, node string) error {
	pods, err := c.List(ctx, "Pod")
	if err != nil {
		return err
	}
	if err := d.Cleanup(ctx, pods); err != nil {
		return err
	}
	var failures []error
	for _, p := range pods {
		if p.Status.NodeName != node {
			continue
		}
		status, err := d.Ensure(ctx, p)
		if err != nil {
			status = p.Status
			status.Phase = "Failed"
			status.Reason = err.Error()
			failures = append(failures, fmt.Errorf("%s: %w", p.Metadata.Name, err))
		}
		p.Status = status
		if err := c.Update(ctx, p, true); err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
