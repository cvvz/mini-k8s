package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"

	"mini-k8s/internal/mini"
)

const usage = `Usage:
  kk cluster up|status|down
  kk apply -f FILE
  kk delete -f FILE
  kk delete pod|deployment|service NAME
  kk get nodes|pods|deployments|services [NAME] [-o json]
  kk describe pod NAME
  kk logs POD
  kk scale deployment NAME --replicas N

Run from the project root or use the project's bin/kk executable.
`

type app struct {
	client *mini.Client
	out    io.Writer
}

var resourceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (a app) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "delete":
		if len(args) == 3 && !strings.HasPrefix(args[1], "-") {
			kind, err := mini.Kind(args[1])
			if err != nil {
				return err
			}
			if err := a.remove(ctx, kind, args[2]); err != nil {
				return fmt.Errorf("%s/%s: %w", kind, args[2], err)
			}
			_, err = fmt.Fprintf(a.out, "%s/%s deleted\n", kind, args[2])
			return err
		}
		fallthrough
	case "apply":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		file := flags.String("f", "", "manifest file")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *file == "" || flags.NArg() != 0 {
			return fmt.Errorf("usage: kk %s -f FILE", args[0])
		}
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		resources, err := readManifests(f)
		if err != nil {
			return err
		}
		for _, resource := range resources {
			action := "deleted"
			if args[0] == "apply" {
				action, err = a.apply(ctx, resource)
			} else {
				err = a.remove(ctx, resource.Kind, resource.Metadata.Name)
			}
			if err != nil {
				return fmt.Errorf("%s/%s: %w", resource.Kind, resource.Metadata.Name, err)
			}
			if _, err := fmt.Fprintf(a.out, "%s/%s %s\n", resource.Kind, resource.Metadata.Name, action); err != nil {
				return err
			}
		}
		return nil
	case "get":
		return a.get(ctx, args[1:])
	case "describe":
		if len(args) != 3 {
			return errors.New("usage: kk describe pod NAME")
		}
		kind, err := mini.Kind(args[1])
		if err != nil || kind != "Pod" {
			return errors.New("describe supports only pod")
		}
		if err := checkName(args[2]); err != nil {
			return err
		}
		resource, err := a.client.Get(ctx, kind, args[2])
		if err != nil {
			return err
		}
		return writeJSON(a.out, resource)
	case "logs":
		if len(args) != 2 {
			return errors.New("usage: kk logs POD")
		}
		if err := checkName(args[1]); err != nil {
			return err
		}
		return a.client.Logs(ctx, args[1], a.out)
	case "scale":
		return a.scale(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func checkName(name string) error {
	if !resourceName.MatchString(name) {
		return fmt.Errorf("invalid resource name %q: expected a DNS label", name)
	}
	return nil
}

func (a app) remove(ctx context.Context, kind, name string) error {
	if kind == "Node" {
		return errors.New("Nodes are managed by workers")
	}
	if err := checkName(name); err != nil {
		return err
	}
	current, err := a.client.Get(ctx, kind, name)
	if err != nil {
		return err
	}
	return a.client.Delete(ctx, current)
}

func (a app) apply(ctx context.Context, resource mini.Resource) (string, error) {
	current, err := a.client.Get(ctx, resource.Kind, resource.Metadata.Name)
	if errors.Is(err, mini.ErrNotFound) {
		_, err = a.client.Create(ctx, resource)
		return "created", err
	}
	if err != nil {
		return "", err
	}
	if err := mini.CheckUpdate(current, resource); err != nil {
		return "", err
	}
	// Keep all server-owned fields and the optimistic concurrency version.
	current.Spec = resource.Spec
	if err := a.client.Update(ctx, current, false); err != nil {
		return "", err
	}
	return "configured", nil
}

func (a app) get(ctx context.Context, args []string) error {
	var positional []string
	output := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-o":
			i++
			if i == len(args) {
				return errors.New("-o requires json")
			}
			output = args[i]
		case strings.HasPrefix(args[i], "-o="):
			output = strings.TrimPrefix(args[i], "-o=")
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %s", args[i])
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 1 || len(positional) > 2 || output != "" && output != "json" {
		return errors.New("usage: kk get KIND [NAME] [-o json]")
	}
	kind, err := mini.Kind(positional[0])
	if err != nil {
		return err
	}
	if len(positional) == 2 {
		if err := checkName(positional[1]); err != nil {
			return err
		}
		resource, err := a.client.Get(ctx, kind, positional[1])
		if err != nil {
			return err
		}
		if output == "json" {
			return writeJSON(a.out, resource)
		}
		return printResources(a.out, []mini.Resource{resource})
	}
	resources, err := a.client.List(ctx, kind)
	if err != nil {
		return err
	}
	if resources == nil {
		resources = []mini.Resource{}
	}
	if output == "json" {
		return writeJSON(a.out, resources)
	}
	return printResources(a.out, resources)
}

func (a app) scale(ctx context.Context, args []string) error {
	if len(args) < 3 {
		return errors.New("usage: kk scale deployment NAME --replicas N")
	}
	kind, err := mini.Kind(args[0])
	if err != nil || kind != "Deployment" {
		return errors.New("scale supports only deployment")
	}
	if err := checkName(args[1]); err != nil {
		return err
	}
	flags := flag.NewFlagSet("scale", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	replicas := flags.Int("replicas", -1, "desired replicas")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *replicas < 0 || *replicas > 20 {
		return errors.New("--replicas must be explicit and between 0 and 20")
	}
	resource, err := a.client.Get(ctx, kind, args[1])
	if err != nil {
		return err
	}
	old := resource
	var spec mini.DeploymentSpec
	if err := mini.Strict(resource.Spec, &spec); err != nil {
		return err
	}
	spec.Replicas = replicas
	resource.Spec, err = json.Marshal(spec)
	if err != nil {
		return err
	}
	if err := mini.Validate(&resource); err != nil {
		return err
	}
	if err := mini.CheckUpdate(old, resource); err != nil {
		return err
	}
	if err := a.client.Update(ctx, resource, false); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "Deployment/%s scaled to %d\n", resource.Metadata.Name, *replicas)
	return err
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printResources(out io.Writer, resources []mini.Resource) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "KIND\tNAME\tPHASE\tNODE\tREPLICAS"); err != nil {
		return err
	}
	for _, resource := range resources {
		replicas := "-"
		if resource.Kind == "Deployment" {
			var spec mini.DeploymentSpec
			if err := mini.Strict(resource.Spec, &spec); err != nil {
				return err
			}
			if spec.Replicas != nil {
				replicas = strconv.Itoa(*spec.Replicas)
			}
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", resource.Kind, resource.Metadata.Name, resource.Status.Phase, resource.Status.NodeName, replicas); err != nil {
			return err
		}
	}
	return table.Flush()
}
