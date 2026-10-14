package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/client"
)

// workerNames are the node containers Compose starts.
var workerNames = []string{"worker-1", "worker-2"}

// nodeContainer is the Compose container of a node, e.g. mini-k8s-worker-1-1.
func nodeContainer(node string) string { return "mini-k8s-" + node + "-1" }

// images are built from build/images/<dir>/Dockerfile by `kk cluster up`.
var images = []struct{ tag, dir string }{
	{"mini-k8s:local", "mini"},
	{"mini-k8s-pause:local", "pause"},
	{"mini-k8s-node:local", "node"},
	{"mini-k8s-etcd:3.5.17", "etcd"},
}

// nodeImages are preloaded into every node's containerd: the sandbox (pause)
// image and the image of the examples and e2e workloads.
var nodeImages = []string{"mini-k8s:local", "mini-k8s-pause:local"}

type cluster struct {
	run func(context.Context, string, ...string) (string, error)
	// pipe runs `from | to` and returns the output of to.
	pipe    func(ctx context.Context, from, to []string) (string, error)
	client  *client.Client
	out     io.Writer
	cleanup func() error
}

func projectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if isProject(cwd) {
		return cwd, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	if filepath.Base(filepath.Dir(executable)) == "bin" {
		root := filepath.Dir(filepath.Dir(executable))
		if isProject(root) {
			return root, nil
		}
	}
	return "", errors.New("run kk from the mini-k8s project root, or use the project's bin/kk")
}

func isProject(root string) bool {
	for _, name := range []string{"go.mod", "compose.yaml"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(module), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1] == "mini-k8s"
		}
	}
	return false
}

func loadToken(root string, create bool) (string, error) {
	dir := filepath.Join(root, ".mini-k8s")
	path := filepath.Join(dir, "token")
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("token directory: %w; run kk cluster up first", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("token directory must be a real directory")
	}
	if create {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return "", err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, writeErr := io.WriteString(file, hex.EncodeToString(secret[:])+"\n")
			closeErr := file.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				return "", fmt.Errorf("write token: %w", err)
			}
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w; run kk cluster up first", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("token must be a regular file, not a symlink")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("existing token is empty or invalid; refusing to replace it")
	}
	return token, nil
}

func localSocket(endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	if u.Scheme != "unix" || u.Host != "" || u.User != nil || !filepath.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("Docker endpoint %q is not a local Unix socket; TCP/SSH/remote engines are refused", endpoint)
	}
	return u.Path, nil
}

func checkDockerDaemon(ctx context.Context, run func(context.Context, string, ...string) (string, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := run(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return fmt.Errorf("Docker CLI preflight failed: %w; no build/start attempted", errors.Join(err, ctx.Err()))
	}
	if strings.TrimSpace(output) == "" {
		return errors.New("Docker CLI preflight returned no server version; no build/start attempted")
	}
	return nil
}

func commandRunner(root string, env []string) func(context.Context, string, ...string) (string, error) {
	return func(ctx context.Context, name string, args ...string) (string, error) {
		command := exec.CommandContext(ctx, name, args...)
		command.Dir = root
		command.Env = env
		data, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, data)
		}
		return string(data), nil
	}
}

func commandPipe(root string, env []string) func(context.Context, []string, []string) (string, error) {
	return func(ctx context.Context, from, to []string) (string, error) {
		producer := exec.CommandContext(ctx, from[0], from[1:]...)
		consumer := exec.CommandContext(ctx, to[0], to[1:]...)
		for _, c := range []*exec.Cmd{producer, consumer} {
			c.Dir, c.Env = root, env
		}
		var producerErr strings.Builder
		producer.Stderr = &producerErr
		stream, err := producer.StdoutPipe()
		if err != nil {
			return "", err
		}
		consumer.Stdin = stream
		var output strings.Builder
		consumer.Stdout, consumer.Stderr = &output, &output
		if err := producer.Start(); err != nil {
			return "", err
		}
		consumeErr := consumer.Run()
		if consumeErr != nil {
			_ = producer.Process.Kill()
		}
		produceErr := producer.Wait()
		if consumeErr != nil || produceErr != nil {
			return "", fmt.Errorf("%s | %s: %w\n%s%s", strings.Join(from, " "), strings.Join(to, " "),
				errors.Join(produceErr, consumeErr), producerErr.String(), output.String())
		}
		return output.String(), nil
	}
}

func withEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func newCluster(ctx context.Context, root, token string, out io.Writer) (cluster, error) {
	env := os.Environ()
	run := commandRunner(root, env)
	endpoint := os.Getenv("DOCKER_HOST")
	dockerContext := os.Getenv("DOCKER_CONTEXT")
	if dockerContext != "" || endpoint == "" {
		if dockerContext == "" {
			value, err := run(ctx, "docker", "context", "show")
			if err != nil {
				return cluster{}, err
			}
			dockerContext = strings.TrimSpace(value)
		}
		value, err := run(ctx, "docker", "context", "inspect", dockerContext, "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return cluster{}, err
		}
		endpoint = strings.TrimSpace(value)
	}
	// The cluster publishes its ports on 127.0.0.1, so only a local engine
	// yields a usable cluster.
	if _, err := localSocket(endpoint); err != nil {
		return cluster{}, err
	}
	env = withEnv(env, "MINI_TOKEN", token)
	var cleanup func() error
	var err error
	switch os.Getenv("MINI_DOCKER_NO_CREDENTIALS") {
	case "", "0":
	case "1":
		env, cleanup, err = anonymousDockerEnv(root, endpoint, env)
		if err != nil {
			return cluster{}, err
		}
		if _, err := fmt.Fprintln(out, "Docker anonymous mode: credential helpers disabled for this command; user config unchanged"); err != nil {
			return cluster{}, errors.Join(err, cleanup())
		}
	default:
		return cluster{}, errors.New("MINI_DOCKER_NO_CREDENTIALS must be 0 or 1")
	}
	run = commandRunner(root, env)
	if err := checkDockerDaemon(ctx, run); err != nil {
		if cleanup != nil {
			err = errors.Join(err, cleanup())
		}
		return cluster{}, err
	}
	return cluster{run: run, pipe: commandPipe(root, env), client: client.New(apiURL(), token), out: out, cleanup: cleanup}, nil
}

func (c cluster) up(ctx context.Context) error {
	commands := [][]string{{"make", "build", "prepare-etcd"}}
	for _, image := range images {
		commands = append(commands, []string{"docker", "build", "-t", image.tag, "-f", "build/images/" + image.dir + "/Dockerfile", "."})
	}
	commands = append(commands, []string{"docker", "compose", "up", "-d", "--pull", "never", "--remove-orphans"})
	for _, command := range commands {
		if err := c.step(ctx, command); err != nil {
			return err
		}
	}
	if err := c.loadImages(ctx, nodeImages); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := c.waitReady(waitCtx); err != nil {
		return err
	}
	_, err := fmt.Fprintln(c.out, "mini-k8s ready: API http://127.0.0.1:18000; workers worker-1, worker-2 Ready")
	return err
}

func (c cluster) step(ctx context.Context, command []string) error {
	if _, err := fmt.Fprintf(c.out, "Running %s\n", strings.Join(command, " ")); err != nil {
		return err
	}
	output, err := c.run(ctx, command[0], command[1:]...)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.out, output)
	return err
}

// loadImages copies local Docker images into the containerd of every node,
// like `kind load docker-image`. A node's containerd may still be starting,
// so each import is retried for a while.
func (c cluster) loadImages(ctx context.Context, refs []string) error {
	for _, node := range workerNames {
		if _, err := fmt.Fprintf(c.out, "Loading %s into %s\n", strings.Join(refs, " "), node); err != nil {
			return err
		}
		from := append([]string{"docker", "save"}, refs...)
		to := []string{"docker", "exec", "-i", nodeContainer(node), "ctr", "-n", "k8s.io", "images", "import", "-"}
		deadline := time.Now().Add(time.Minute)
		for {
			_, err := c.pipe(ctx, from, to)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("load images into %s: %w", node, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

func (c cluster) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last error
	for {
		last = c.client.Health(ctx)
		if last == nil {
			nodes, err := client.Nodes(c.client).List(ctx, client.ListOptions{})
			last = err
			if err == nil {
				ready := map[string]bool{}
				for i := range nodes.Items {
					if corev1.IsNodeReady(&nodes.Items[i], time.Now()) {
						ready[nodes.Items[i].Name] = true
					}
				}
				missing := []string{}
				for _, w := range workerNames {
					if !ready[w] {
						missing = append(missing, w)
					}
				}
				if len(missing) == 0 {
					return nil
				}
				last = fmt.Errorf("waiting for fresh Ready heartbeats from %s", strings.Join(missing, ", "))
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster readiness failed: %w (last check: %v); inspect docker compose logs", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// down stops the reconcilers and the nodes first, so nothing recreates Pods
// while the rest is removed; Pods die with their node containers. With
// volumes it also deletes the etcd data and the nodes' disks.
func (c cluster) down(ctx context.Context, volumes bool) error {
	stop := append(append([]string{"compose", "stop"}, workerNames...), "controller", "scheduler")
	if _, err := c.run(ctx, "docker", stop...); err != nil {
		return err
	}
	args := []string{"compose", "down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	_, err := c.run(ctx, "docker", args...)
	return err
}

// runCluster implements `kk cluster up|status|down`.
func runCluster(ctx context.Context, root string, a args) (result error) {
	if len(a.positional) != 1 || (a.positional[0] != "up" && a.positional[0] != "status" && a.positional[0] != "down") {
		return errors.New("usage: kk cluster up|status|down [--volumes]")
	}
	sub := a.positional[0]
	token, err := loadToken(root, sub == "up")
	if err != nil {
		return err
	}
	if sub == "status" {
		c := client.New(apiURL(), token)
		if err := c.Health(ctx); err != nil {
			return fmt.Errorf("API unhealthy: %w", err)
		}
		if _, err := fmt.Fprintln(os.Stdout, "API healthy"); err != nil {
			return err
		}
		return newApp(c, os.Stdout, os.Stdin, args{}).run(ctx, "get", args{positional: []string{"nodes"}, values: map[string][]string{}, bools: map[string]bool{}})
	}
	c, err := newCluster(ctx, root, token, os.Stdout)
	if err != nil {
		return err
	}
	if c.cleanup != nil {
		defer func() {
			if err := c.cleanup(); err != nil {
				result = errors.Join(result, fmt.Errorf("clean up temporary Docker config: %w", err))
			}
		}()
	}
	if sub == "up" {
		return c.up(ctx)
	}
	volumes := a.boolean("volumes", false)
	if err := c.down(ctx, volumes); err != nil {
		return err
	}
	if volumes {
		_, err = fmt.Fprintln(os.Stdout, "mini-k8s stopped; etcd data and node disks deleted, token preserved")
	} else {
		_, err = fmt.Fprintln(os.Stdout, "mini-k8s stopped; etcd data, node disks and token preserved")
	}
	return err
}

// runImage implements `kk image load IMAGE...`.
func runImage(ctx context.Context, root string, a args) (result error) {
	if len(a.positional) < 2 || a.positional[0] != "load" {
		return errors.New("usage: kk image load IMAGE...")
	}
	token, err := loadToken(root, false)
	if err != nil {
		return err
	}
	c, err := newCluster(ctx, root, token, os.Stdout)
	if err != nil {
		return err
	}
	if c.cleanup != nil {
		defer func() {
			if err := c.cleanup(); err != nil {
				result = errors.Join(result, fmt.Errorf("clean up temporary Docker config: %w", err))
			}
		}()
	}
	return c.loadImages(ctx, a.positional[1:])
}
