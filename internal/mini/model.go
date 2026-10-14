package mini

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"time"
)

type Metadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion int64             `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	OwnerUID        string            `json:"ownerUID,omitempty"`
}
type Status struct {
	Phase       string    `json:"phase,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	NodeName    string    `json:"nodeName,omitempty"`
	ContainerID string    `json:"containerID,omitempty"`
	PodIP       string    `json:"podIP,omitempty"`
	Heartbeat   time.Time `json:"heartbeat,omitempty"`
	Restarts    int       `json:"restarts,omitempty"`
}
type Resource struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   Metadata        `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     Status          `json:"status,omitempty"`
}
type Env struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type Container struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Env     []Env    `json:"env,omitempty"`
}
type PodSpec struct {
	Containers []Container `json:"containers"`
}
type Selector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}
type Template struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec PodSpec `json:"spec"`
}
type DeploymentSpec struct {
	Replicas *int     `json:"replicas"`
	Selector Selector `json:"selector"`
	Template Template `json:"template"`
}
type ServicePort struct {
	Port       int `json:"port"`
	TargetPort int `json:"targetPort"`
}
type ServiceSpec struct {
	Selector map[string]string `json:"selector"`
	Ports    []ServicePort     `json:"ports"`
}

var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var validEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Strict(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected a single JSON value")
	}
	return nil
}
func Decode(data []byte) (Resource, error) {
	var r Resource
	if err := Strict(data, &r); err != nil {
		return r, err
	}
	return r, Validate(&r)
}
func Validate(r *Resource) error {
	if !validName.MatchString(r.Metadata.Name) {
		return errors.New("name must be a DNS label (1-63 characters)")
	}
	if r.Kind == "Node" {
		if r.Metadata.Namespace != "" {
			return errors.New("Node is not namespaced")
		}
	} else {
		if r.Metadata.Namespace == "" {
			r.Metadata.Namespace = "default"
		}
		if r.Metadata.Namespace != "default" {
			return errors.New("only namespace default is supported")
		}
	}
	version := "v1"
	if r.Kind == "Deployment" {
		version = "apps/v1"
	}
	if r.APIVersion != version {
		return fmt.Errorf("%s requires apiVersion %s", r.Kind, version)
	}
	switch r.Kind {
	case "Pod":
		var s PodSpec
		if err := Strict(r.Spec, &s); err != nil {
			return err
		}
		return validatePod(s)
	case "Deployment":
		var s DeploymentSpec
		if err := Strict(r.Spec, &s); err != nil {
			return err
		}
		if s.Replicas == nil || *s.Replicas < 0 || *s.Replicas > 20 {
			return errors.New("replicas must be explicit and between 0 and 20")
		}
		if len(s.Selector.MatchLabels) == 0 || !Matches(s.Selector.MatchLabels, s.Template.Metadata.Labels) {
			return errors.New("selector must be nonempty and match template labels")
		}
		return validatePod(s.Template.Spec)
	case "Service":
		var s ServiceSpec
		if err := Strict(r.Spec, &s); err != nil {
			return err
		}
		if len(s.Selector) == 0 || len(s.Ports) != 1 || s.Ports[0].Port != 80 || s.Ports[0].TargetPort < 1 || s.Ports[0].TargetPort > 65535 {
			return errors.New("Service requires selector and one port: 80 with numeric targetPort 1-65535; gateway is localhost:18080")
		}
	case "Node":
		var s struct{}
		return Strict(r.Spec, &s)
	default:
		return fmt.Errorf("unsupported kind %q", r.Kind)
	}
	return nil
}
func validatePod(s PodSpec) error {
	if len(s.Containers) != 1 {
		return errors.New("exactly one container is supported")
	}
	c := s.Containers[0]
	if !validName.MatchString(c.Name) || c.Image == "" {
		return errors.New("container requires a valid name and image")
	}
	for _, e := range c.Env {
		if !validEnv.MatchString(e.Name) {
			return fmt.Errorf("invalid environment variable %q", e.Name)
		}
	}
	return nil
}
func Matches(selector, labels map[string]string) bool {
	for k, v := range selector {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}
func CheckUpdate(old, next Resource) error {
	var a, b any
	if err := json.Unmarshal(old.Spec, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(next.Spec, &b); err != nil {
		return err
	}
	if old.Kind == "Deployment" {
		delete(a.(map[string]any), "replicas")
		delete(b.(map[string]any), "replicas")
	}
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(old.Metadata.Labels, next.Metadata.Labels) {
		return errors.New("spec and labels are immutable; only Deployment replicas can change")
	}
	return nil
}
func UID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func ChooseNode(nodes, pods []Resource, now time.Time) string {
	count := map[string]int{}
	for _, p := range pods {
		count[p.Status.NodeName]++
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Metadata.Name < nodes[j].Metadata.Name })
	best := ""
	for _, n := range nodes {
		if n.Status.Phase != "Ready" || now.Sub(n.Status.Heartbeat) > 15*time.Second {
			continue
		}
		name := n.Metadata.Name
		if best == "" || count[name] < count[best] {
			best = name
		}
	}
	return best
}
func OwnedPods(d Resource, pods []Resource) []Resource {
	var result []Resource
	for _, p := range pods {
		if p.Metadata.OwnerUID == d.Metadata.UID && d.Metadata.UID != "" {
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Metadata.Name < result[j].Metadata.Name })
	return result
}
