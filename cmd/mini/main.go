// Command mini runs one mini-k8s component, selected by its first argument.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mini-k8s/internal/apiserver"
	"mini-k8s/internal/client"
	"mini-k8s/internal/controller"
	"mini-k8s/internal/informer"
	"mini-k8s/internal/kubelet"
	"mini-k8s/internal/proxy"
	"mini-k8s/internal/record"
	"mini-k8s/internal/scheduler"
	"mini-k8s/internal/storage"
)

const usage = "usage: mini apiserver|scheduler|controller-manager|kubelet|proxy|demo"

func env(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal(usage)
	}
	role := os.Args[1]
	if role == "demo" {
		runDemo()
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	token := os.Getenv("MINI_TOKEN")
	if token == "" {
		log.Fatal("MINI_TOKEN is required")
	}
	c := client.New(env("MINI_API", "http://apiserver:8080"), token)
	host, _ := os.Hostname()
	switch role {
	case "apiserver":
		runAPIServer(ctx, token)
	case "scheduler":
		rec := record.New(c, "default-scheduler", host)
		pods := informer.New(client.Pods(c, ""), client.ListOptions{}, 0)
		nodes := informer.New(client.Nodes(c), client.ListOptions{}, 0)
		s := scheduler.New(c, pods, nodes, rec)
		go rec.Run(ctx)
		go pods.Run(ctx)
		go nodes.Run(ctx)
		informer.WaitForSync(ctx, pods.HasSynced, nodes.HasSynced)
		log.Print("scheduler started")
		s.Run(ctx)
	case "controller-manager", "controller":
		runControllerManager(ctx, c, host)
	case "kubelet":
		runKubelet(ctx, c, token)
	case "proxy":
		g := proxy.NewGateway(c)
		go g.Run(ctx)
		serve(ctx, ":8080", g)
	default:
		log.Fatalf("unknown role %q; %s", role, usage)
	}
}

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

func runAPIServer(ctx context.Context, token string) {
	s := apiserver.New(apiserver.Config{Store: storage.New(env("MINI_ETCD", "http://etcd:2379")), Token: token})
	for {
		err := s.Bootstrap(ctx)
		if err == nil {
			break
		}
		log.Printf("apiserver: waiting for etcd: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	serve(ctx, ":8080", s)
}

func runControllerManager(ctx context.Context, c *client.Client, host string) {
	rec := record.New(c, "controller-manager", host)
	pods := informer.New(client.Pods(c, ""), client.ListOptions{}, 30*time.Second)
	deployments := informer.New(client.Deployments(c, ""), client.ListOptions{}, 30*time.Second)
	namespaces := informer.New(client.Namespaces(c), client.ListOptions{}, 30*time.Second)
	dc := controller.NewDeploymentController(c, deployments, pods, rec)
	nc := controller.NewNamespaceController(c, namespaces)
	gc := controller.NewGarbageCollector(c, controller.GCResources())
	go rec.Run(ctx)
	for _, run := range []func(context.Context){pods.Run, deployments.Run, namespaces.Run} {
		go run(ctx)
	}
	informer.WaitForSync(ctx, pods.HasSynced, deployments.HasSynced, namespaces.HasSynced)
	go nc.Run(ctx, 2)
	go gc.Run(ctx)
	log.Print("controller-manager started")
	dc.Run(ctx, 4)
}

func runKubelet(ctx context.Context, c *client.Client, token string) {
	node := os.Getenv("MINI_NODE")
	if node == "" {
		log.Fatal("MINI_NODE is required")
	}
	rt, err := kubelet.NewRuntime("/var/run/docker.sock", env("MINI_CLUSTER", "mini-k8s"), node, env("MINI_NETWORK", "mini-k8s"))
	if err != nil {
		log.Fatal(err)
	}
	address, err := podNetworkAddress()
	if err != nil {
		log.Fatal(err)
	}
	rec := record.New(c, "kubelet", node)
	k := kubelet.New(c, rt, rec, node, address, token)
	go rec.Run(ctx)
	go k.Run(ctx)
	serve(ctx, ":10250", k.Handler())
}

// podNetworkAddress returns this container's IPv4 address on the cluster network.
func podNetworkAddress() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address found")
}

// runDemo is a tiny HTTP app used by examples and e2e tests.
//
//	DEMO_SHUTDOWN_DELAY  wait this long after SIGTERM before exiting
//	DEMO_IGNORE_SIGTERM  set to 1 to ignore SIGTERM (exercises SIGKILL)
func runDemo() {
	host, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	server := &http.Server{Addr: ":8080", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.RequestURI())
		fmt.Fprintf(w, "hello from %s\n", host)
	})}
	go func() {
		for sig := range signals {
			if os.Getenv("DEMO_IGNORE_SIGTERM") == "1" {
				log.Printf("received %v, ignoring it", sig)
				continue
			}
			log.Printf("received %v, shutting down", sig)
			if d, err := time.ParseDuration(os.Getenv("DEMO_SHUTDOWN_DELAY")); err == nil {
				time.Sleep(d)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = server.Shutdown(ctx)
			cancel()
			return
		}
	}()
	log.Printf("demo web server started on %s:8080", host)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Print("demo web server stopped")
}
