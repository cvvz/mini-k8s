// Command kk is the mini-k8s command-line client.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"mini-k8s/pkg/client"
)

func apiURL() string {
	if value := os.Getenv("MINI_API"); value != "" {
		return value
	}
	return "http://127.0.0.1:18000"
}

const usage = `kk controls a mini-k8s cluster.

Cluster lifecycle (run from the project root or via its bin/kk):
  kk cluster up|status|down [--volumes]   --volumes also deletes etcd data and node disks
  kk image load IMAGE...                  copy local Docker images into every node

Resources:
  kk get TYPE[,TYPE...] [NAME...] [-o wide|json|yaml|name] [-l SELECTOR] [--field-selector F] [-w]
  kk describe TYPE NAME
  kk apply -f FILE|DIR|-          create or update (validated with a dry run first)
  kk create -f FILE|DIR|-
  kk create namespace NAME
  kk delete -f FILE | TYPE NAME... | TYPE -l SELECTOR | TYPE --all
            [--grace-period N] [--force] [--cascade background|foreground|orphan] [--wait=false]
  kk logs POD [-c CONTAINER] [-f] [--tail N] [-p]
  kk exec POD [-c CONTAINER] -- COMMAND [ARG...]   run a command to completion (no stdin/tty)
  kk scale deployment|replicaset NAME --replicas N
  kk rollout status|history|undo|pause|resume|restart deployment/NAME
            [--timeout DURATION] [--to-revision N]
  kk cordon|uncordon NODE
  kk drain NODE [--force] [--delete-emptydir-data] [--ignore-daemonsets] [--grace-period N] [--timeout D]
  kk taint nodes NODE key[=value]:Effect ... | key[:Effect]- ...   [--overwrite]
  kk label TYPE NAME key=value ... | key- ...                  [--overwrite]
  kk api-resources
  kk version

Global flags:
  -n, --namespace NS     namespace (default "default")
  -A, --all-namespaces   list across all namespaces

TYPE accepts kubectl-style names: pods/pod/po, deployments/deploy, replicasets/rs,
services/svc, nodes/no, namespaces/ns, events/ev. TYPE/NAME is also accepted.
`

func execute(ctx context.Context, argv []string) error {
	command := findCommand(argv)
	if command == "" || command == "help" {
		if len(argv) > 0 && (argv[0] == "help" || argv[0] == "-h" || argv[0] == "--help") {
			_, err := fmt.Fprint(os.Stdout, usage)
			return err
		}
		return errors.New(usage)
	}
	specs, ok := commandFlags[command]
	if !ok {
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}
	parsed, err := parseArgs(argv, append(append([]flagSpec{}, globalFlags...), specs...))
	if err != nil {
		return err
	}
	parsed.positional = parsed.positional[1:] // drop the command itself
	root, err := projectRoot()
	if err != nil {
		return err
	}
	switch command {
	case "cluster":
		return runCluster(ctx, root, parsed)
	case "image":
		return runImage(ctx, root, parsed)
	}
	token, err := loadToken(root, false)
	if err != nil {
		return err
	}
	a := newApp(client.New(apiURL(), token), os.Stdout, os.Stdin, parsed)
	return a.run(ctx, command, parsed)
}

// formatError renders API errors like kubectl does.
func formatError(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status.Reason != "" {
		return fmt.Sprintf("Error from server (%s): %s", apiErr.Status.Reason, apiErr.Status.Message)
	}
	return "kk: " + err.Error()
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:]); err != nil {
		var exit exitCodeError
		if errors.As(err, &exit) {
			// Like kubectl exec: report and pass on the command's exit code.
			fmt.Fprintln(os.Stderr, exit.Error())
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, formatError(err))
		os.Exit(1)
	}
}
