package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
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

// runEnv prints the environment sorted, like `env | sort`: `mini env`.
func runEnv() int {
	vars := os.Environ()
	sort.Strings(vars)
	for _, v := range vars {
		fmt.Println(v)
	}
	return 0
}

// runLs lists a directory, including dot entries, with the targets of
// symbolic links and a slash after directories: `mini ls /etc/config`.
func runLs(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mini ls DIR")
		return 2
	}
	entries, err := os.ReadDir(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, e := range entries {
		switch {
		case e.Type()&os.ModeSymlink != 0:
			target, _ := os.Readlink(filepath.Join(args[0], e.Name()))
			fmt.Printf("%s -> %s\n", e.Name(), target)
		case e.IsDir():
			fmt.Println(e.Name() + "/")
		default:
			fmt.Println(e.Name())
		}
	}
	return 0
}

// runWrite writes TEXT and a newline to FILE, like `echo TEXT > FILE`:
// `mini write /data/greeting hello`.
func runWrite(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mini write FILE TEXT")
		return 2
	}
	if err := os.WriteFile(args[0], []byte(args[1]+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
