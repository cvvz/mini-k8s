package main

import (
	"os"
	"os/signal"
	"syscall"
)

// runPause is the Pod sandbox process: it holds the Pod's namespaces open
// and exits on SIGTERM or SIGINT, like registry.k8s.io/pause.
func runPause() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
}
