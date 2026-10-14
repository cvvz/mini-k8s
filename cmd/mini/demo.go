package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// runDemo is a tiny HTTP app used by examples and e2e tests. Every path
// answers "hello from <hostname>", except the health endpoints:
//
//	/healthz     200, or 500 after /fail-live (for liveness probes)
//	/ready       200, or 503 after /fail-ready (for readiness probes)
//	/recover     makes both healthy again
//
// Environment:
//
//	DEMO_PORT            listen port (default 8080)
//	DEMO_START_DELAY     wait this long before listening (a slow start)
//	DEMO_SHUTDOWN_DELAY  wait this long after SIGTERM before exiting
//	DEMO_IGNORE_SIGTERM  set to 1 to ignore SIGTERM (exercises SIGKILL)
func runDemo() {
	host, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	port := env("DEMO_PORT", "8080")
	var liveFailed, readyFailed atomic.Bool
	health := func(w http.ResponseWriter, failed *atomic.Bool, code int) {
		if failed.Load() {
			w.WriteHeader(code)
			fmt.Fprintln(w, "unhealthy")
			return
		}
		fmt.Fprintln(w, "ok")
	}
	server := &http.Server{Addr: ":" + port, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "kube-probe/") {
			log.Printf("%s %s from %s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
		}
		switch r.URL.Path {
		case "/healthz":
			health(w, &liveFailed, http.StatusInternalServerError)
		case "/ready":
			health(w, &readyFailed, http.StatusServiceUnavailable)
		case "/fail-live":
			liveFailed.Store(true)
			fmt.Fprintln(w, "liveness probes will fail")
		case "/fail-ready":
			readyFailed.Store(true)
			fmt.Fprintln(w, "readiness probes will fail")
		case "/recover":
			liveFailed.Store(false)
			readyFailed.Store(false)
			fmt.Fprintln(w, "healthy again")
		default:
			fmt.Fprintf(w, "hello from %s\n", host)
		}
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
	if d, err := time.ParseDuration(os.Getenv("DEMO_START_DELAY")); err == nil {
		log.Printf("demo web server starting in %s", d)
		time.Sleep(d)
	}
	log.Printf("demo web server started on %s:%s", host, port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Print("demo web server stopped")
}
