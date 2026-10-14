package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"mini-k8s/internal/mini"
)

func apiURL() string {
	if value := os.Getenv("MINI_API"); value != "" {
		return value
	}
	return "http://127.0.0.1:18000"
}

func execute(ctx context.Context, args []string) (result error) {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprint(os.Stdout, usage)
		return err
	}
	if len(args) == 0 {
		return errors.New(usage)
	}
	root, err := projectRoot()
	if err != nil {
		return err
	}
	isCluster := args[0] == "cluster"
	if isCluster && (len(args) != 2 || args[1] != "up" && args[1] != "status" && args[1] != "down") {
		return errors.New("usage: kk cluster up|status|down")
	}
	token, err := loadToken(root, isCluster && args[1] == "up")
	if err != nil {
		return err
	}
	client := mini.NewClient(apiURL(), token)
	if !isCluster {
		return (app{client: client, out: os.Stdout}).run(ctx, args)
	}
	if args[1] == "status" {
		if err := client.Health(ctx); err != nil {
			return fmt.Errorf("API unhealthy: %w", err)
		}
		if _, err := fmt.Fprintln(os.Stdout, "API healthy"); err != nil {
			return err
		}
		return (app{client: client, out: os.Stdout}).run(ctx, []string{"get", "nodes"})
	}
	c, err := newCluster(ctx, root, token, os.Stdout)
	if err != nil {
		return err
	}
	if c.cleanup != nil {
		defer func() {
			if err := c.cleanup(); err != nil {
				result = errors.Join(result, fmt.Errorf("clean up temporary Docker config: %w", err))
			}
		}()
	}
	if args[1] == "up" {
		return c.up(ctx)
	}
	if err := c.down(ctx); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, "mini-k8s stopped; etcd volume and token preserved")
	return err
}

func main() {
	if err := os.Setenv("PATH", "/opt/homebrew/bin:/Applications/Docker.app/Contents/Resources/bin:"+os.Getenv("PATH")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kk:", err)
		os.Exit(1)
	}
}
