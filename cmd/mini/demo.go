package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// runDemo is a tiny HTTP app used by examples and e2e tests.
//
//	DEMO_PORT            listen port (default 8080)
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
	server := &http.Server{Addr: ":" + port, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
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
	log.Printf("demo web server started on %s:%s", host, port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Print("demo web server stopped")
}
