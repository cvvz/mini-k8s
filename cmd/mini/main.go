// Command mini is a multi-call binary, like Kubernetes' former hyperkube:
// its first argument selects the component to run.
//
//	mini kube-apiserver | kube-controller-manager | kube-scheduler | kubelet | gateway | demo
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"mini-k8s/pkg/client"
)

// component runs one role until ctx ends; token authenticates to the API.
type component func(ctx context.Context, c *client.Client, token string)

var components = map[string]component{
	"kube-apiserver":          runAPIServer,
	"kube-controller-manager": runControllerManager,
	"kube-scheduler":          runScheduler,
	"kubelet":                 runKubelet,
	"gateway":                 runGateway,
}

func usage() string {
	names := []string{"demo"}
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
	if len(os.Args) != 2 {
		log.Fatal(usage())
	}
	if os.Args[1] == "demo" {
		runDemo()
		return
	}
	run, ok := components[os.Args[1]]
	if !ok {
		log.Fatalf("unknown component %q; %s", os.Args[1], usage())
	}
	token := os.Getenv("MINI_TOKEN")
	if token == "" {
		log.Fatal("MINI_TOKEN is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	run(ctx, client.New(env("MINI_API", "http://apiserver:8080"), token), token)
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
