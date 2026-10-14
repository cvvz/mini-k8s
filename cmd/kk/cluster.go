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
	"runtime"
	"strings"
	"time"

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
)

// workerNames are the logical nodes Compose starts.
var workerNames = []string{"worker-1", "worker-2"}

type cluster struct {
	run     func(context.Context, string, ...string) (string, error)
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
	socket, err := localSocket(endpoint)
	if err != nil {
		return cluster{}, err
	}
	if runtime.GOOS == "darwin" {
		// Docker Desktop bind mounts resolve inside its Linux VM.
		socket = "/var/run/docker.sock"
	}
	env = withEnv(env, "MINI_TOKEN", token)
	env = withEnv(env, "MINI_DOCKER_SOCKET", socket)
	var cleanup func() error
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
	return cluster{run: run, client: client.New(apiURL(), token), out: out, cleanup: cleanup}, nil
}

func (c cluster) up(ctx context.Context) error {
	for _, command := range [][]string{
		{"make", "build", "prepare-etcd"},
		{"docker", "build", "-t", "mini-k8s:local", "."},
		{"docker", "build", "-t", "mini-k8s-etcd:3.5.17", "-f", "Dockerfile.etcd", "."},
		{"docker", "compose", "up", "-d", "--pull", "never"},
	} {
		if _, err := fmt.Fprintf(c.out, "Running %s\n", strings.Join(command, " ")); err != nil {
			return err
		}
		output, err := c.run(ctx, command[0], command[1:]...)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(c.out, output); err != nil {
			return err
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := c.waitReady(waitCtx); err != nil {
		return err
	}
	_, err := fmt.Fprintln(c.out, "mini-k8s ready: API http://127.0.0.1:18000; workers worker-1, worker-2 Ready")
	return err
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
					if api.IsNodeReady(&nodes.Items[i], time.Now()) {
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

func (c cluster) down(ctx context.Context) error {
	if _, err := c.run(ctx, "docker", "compose", "stop", "worker-1", "worker-2", "controller", "scheduler"); err != nil {
		return err
	}
	output, err := c.run(ctx, "docker", "ps", "-aq", "--filter", "label=mini.cluster=mini-k8s", "--filter", "label=mini.pod")
	if err != nil {
		return err
	}
	if ids := strings.Fields(output); len(ids) != 0 {
		if _, err := c.run(ctx, "docker", append([]string{"rm", "-f"}, ids...)...); err != nil {
			return err
		}
	}
	_, err = c.run(ctx, "docker", "compose", "down")
	return err
}

// runCluster implements `kk cluster up|status|down`.
func runCluster(ctx context.Context, root string, a args) (result error) {
	if len(a.positional) != 1 || (a.positional[0] != "up" && a.positional[0] != "status" && a.positional[0] != "down") {
		return errors.New("usage: kk cluster up|status|down")
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
	if err := c.down(ctx); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, "mini-k8s stopped; etcd volume and token preserved")
	return err
}
