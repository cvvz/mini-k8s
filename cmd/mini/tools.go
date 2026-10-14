package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"mini-k8s/pkg/api/resource"
)

// runCat prints files, like cat(1), for images without a shell:
// `mini cat /sys/fs/cgroup/memory.max`.
func runCat(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mini cat FILE...")
		return 2
	}
	status := 0
	for _, path := range args {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			status = 1
			continue
		}
		_, _ = io.Copy(os.Stdout, f)
		f.Close()
	}
	return status
}

// runAlloc allocates and touches SIZE of memory ("64Mi"), then holds it until
// SIGTERM, to exercise memory limits: `mini alloc 128Mi`.
func runAlloc(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mini alloc SIZE")
		return 2
	}
	q, err := resource.ParseQuantity(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	const chunk = 1 << 20
	var held [][]byte
	for allocated := int64(0); allocated < q.Value(); allocated += chunk {
		b := make([]byte, chunk)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1 // touch every page so it is really resident
		}
		held = append(held, b)
	}
	fmt.Printf("holding %s\n", args[0])
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	runtime.KeepAlive(held)
	return 0
}
