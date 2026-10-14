package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runFetch is a minimal HTTP client for images without a shell or curl:
// `mini fetch URL` prints the status code on the first line, then the body.
// It exits 1 only when no response arrives.
func runFetch(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mini fetch URL")
		return 2
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	_, _ = io.Copy(os.Stdout, io.LimitReader(resp.Body, 1<<20))
	return 0
}
