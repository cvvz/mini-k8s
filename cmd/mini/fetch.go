package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// runFetch GETs a URL and prints the status code and the body, for images
// without curl: `mini fetch http://web/` or, with a bearer token read from
// a file (a Pod's service account token),
// `mini fetch --token-file /var/run/secrets/kubernetes.io/serviceaccount/token URL`.
func runFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	tokenFile := fs.String("token-file", "", "send the token in this file as a bearer token")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mini fetch [--token-file FILE] URL")
		return 2
	}
	req, err := http.NewRequest(http.MethodGet, fs.Arg(0), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *tokenFile != "" {
		token, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	_, _ = io.Copy(os.Stdout, io.LimitReader(resp.Body, 1<<20))
	return 0
}
