package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerPreflightUsesSameCLIEnvironmentAsBuild(t *testing.T) {
	for _, scenario := range []string{"healthy", "unhealthy", "hung"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			script := `#!/bin/sh
set -eu
[ "$1" = info ]
[ "$2" = --format ]
[ "$3" = '{{.ServerVersion}}' ]
[ "$DOCKER_HOST" = unix:///test/docker.sock ]
[ "$DOCKER_CONFIG" = "$EXPECTED_CONFIG" ]
[ "$DOCKER_API_VERSION" = 1.47 ]
case "$SCENARIO" in
  healthy) printf '27.4.0\n' ;;
  unhealthy) echo 'daemon unavailable' >&2; exit 1 ;;
  hung) exec sleep 10 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			env := withEnv(os.Environ(), "DOCKER_HOST", "unix:///test/docker.sock")
			env = withEnv(env, "DOCKER_CONFIG", filepath.Join(dir, "anonymous-config"))
			env = withEnv(env, "DOCKER_API_VERSION", "1.47")
			env = append(env, "EXPECTED_CONFIG="+filepath.Join(dir, "anonymous-config"), "SCENARIO="+scenario)
			timeout := 5 * time.Second
			if scenario == "hung" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			err := checkDockerDaemon(ctx, commandRunner(dir, env))
			if scenario == "healthy" && err != nil {
				t.Fatal(err)
			}
			if scenario == "unhealthy" && (err == nil || !strings.Contains(err.Error(), "daemon unavailable")) {
				t.Fatalf("unhealthy CLI result not surfaced: %v", err)
			}
			if scenario == "hung" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected bounded CLI timeout: %v", err)
			}
		})
	}
}
