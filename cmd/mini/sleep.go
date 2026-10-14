package main

import (
	"fmt"
	"os"
	"time"
)

// runSleep waits for a duration and exits 0, like sleep(1), for images
// without a shell: `mini sleep 2s`.
func runSleep(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mini sleep DURATION")
		return 2
	}
	d, err := time.ParseDuration(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	time.Sleep(d)
	return 0
}
