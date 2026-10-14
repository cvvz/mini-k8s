package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mini-k8s/internal/mini"
)

func env(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}
func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: mini apiserver|scheduler|controller|kubelet|proxy|demo")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	token := os.Getenv("MINI_TOKEN")
	if token == "" && os.Args[1] != "demo" {
		log.Fatal("MINI_TOKEN is required")
	}
	c := mini.NewClient(env("MINI_API", "http://apiserver:8080"), token)
	var handler http.Handler
	switch os.Args[1] {
	case "apiserver":
		handler = mini.APIHandler(mini.NewStore(env("MINI_ETCD", "http://etcd:2379")), token)
	case "scheduler":
		mini.Loop(ctx, time.Second, "scheduler", func(ctx context.Context) error { return mini.Schedule(ctx, c) })
		return
	case "controller":
		mini.Loop(ctx, time.Second, "controller", func(ctx context.Context) error { return mini.ReconcileDeployments(ctx, c) })
		return
	case "kubelet":
		node := os.Getenv("MINI_NODE")
		if node == "" {
			log.Fatal("MINI_NODE is required")
		}
		d := mini.NewDocker("/var/run/docker.sock", env("MINI_CLUSTER", "mini-k8s"), node, env("MINI_NETWORK", "mini-k8s"))
		go mini.Loop(ctx, 2*time.Second, "heartbeat", func(ctx context.Context) error { return mini.Heartbeat(ctx, c, node) })
		go mini.Loop(ctx, time.Second, "kubelet", func(ctx context.Context) error { return mini.ReconcileNode(ctx, c, d, node) })
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", 401)
				return
			}
			if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/logs/") {
				http.NotFound(w, r)
				return
			}
			p, err := c.Get(r.Context(), "Pod", strings.TrimPrefix(r.URL.Path, "/logs/"))
			if err != nil {
				http.Error(w, err.Error(), 404)
				return
			}
			if p.Status.NodeName != node || p.Status.ContainerID == "" {
				http.Error(w, "no container on this node", 409)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			if err := d.Logs(r.Context(), p.Status.ContainerID, w); err != nil {
				log.Printf("logs: %v", err)
				http.Error(w, err.Error(), 502)
			}
		})
	case "proxy":
		p := mini.NewProxy()
		handler = p
		go mini.Loop(ctx, time.Second, "proxy", func(ctx context.Context) error { return p.Refresh(ctx, c) })
	case "demo":
		host, err := os.Hostname()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("demo web server started on %s:8080", host)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("%s %s", r.Method, r.URL.Path)
			fmt.Fprintf(w, "hello from %s\n", host)
		})
	default:
		log.Fatalf("unknown role %q", os.Args[1])
	}
	server := &http.Server{Addr: ":8080", Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("%s listening on :8080", os.Args[1])
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
