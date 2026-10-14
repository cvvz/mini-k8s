// Command mini is a multi-call binary, like Kubernetes' former hyperkube:
// its first argument selects the component to run.
//
//	mini kube-apiserver | kube-controller-manager | kube-scheduler | node-init | kube-proxy | ingress-controller
//	mini demo | pause | fetch URL | sleep DURATION | cat FILE... | write FILE TEXT | alloc SIZE
//
// Invoked as mini-cni or loopback (through symlinks in /opt/cni/bin) it is
// the CNI plugin.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/network/cni"
)

// component runs one role until ctx ends; token authenticates to the API.
type component func(ctx context.Context, c *client.Client, token string)

var components = map[string]component{
	"kube-apiserver":          runAPIServer,
	"kube-controller-manager": runControllerManager,
	"kube-scheduler":          runScheduler,
	"node-init":               runNodeInit,
	"kube-proxy":              runKubeProxy,
	"ingress-controller":      runIngressController,
}

func usage() string {
	names := []string{"demo", "pause", "fetch URL", "sleep DURATION", "cat FILE...", "write FILE TEXT", "alloc SIZE"}
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	return "usage: mini " + strings.Join(names, "|")
}

func env(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}

func main() {
	if name := filepath.Base(os.Args[0]); name == "mini-cni" || name == "loopback" {
		os.Exit(cni.Main(os.Stdin, os.Stdout, os.Getenv))
	}
	if len(os.Args) < 2 {
		log.Fatal(usage())
	}
	switch os.Args[1] {
	case "demo":
		runDemo()
		return
	case "pause":
		runPause()
		return
	case "fetch":
		os.Exit(runFetch(os.Args[2:]))
	case "sleep":
		os.Exit(runSleep(os.Args[2:]))
	case "cat":
		os.Exit(runCat(os.Args[2:]))
	case "write":
		os.Exit(runWrite(os.Args[2:]))
	case "alloc":
		os.Exit(runAlloc(os.Args[2:]))
	}
	run, ok := components[os.Args[1]]
	if !ok || len(os.Args) != 2 {
		log.Fatalf("unknown component %q; %s", strings.Join(os.Args[1:], " "), usage())
	}
	token, err := loadToken()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	run(ctx, client.New(env("MINI_API", "http://apiserver:8080"), token), token)
}

// loadToken returns the API token from MINI_TOKEN or, for static Pods that
// mount it, from the file named by MINI_TOKEN_FILE.
func loadToken() (string, error) {
	if token := os.Getenv("MINI_TOKEN"); token != "" {
		return token, nil
	}
	path := os.Getenv("MINI_TOKEN_FILE")
	if path == "" {
		return "", errors.New("MINI_TOKEN or MINI_TOKEN_FILE is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// serve runs an HTTP server until ctx ends.
func serve(ctx context.Context, addr string, handler http.Handler) {
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("%s listening on %s", os.Args[1], addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
