package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func anonymousDockerEnv(root, endpoint string, env []string) ([]string, func() error, error) {
	configHome := ""
	for _, item := range env {
		if strings.HasPrefix(item, "DOCKER_CONFIG=") {
			configHome = strings.TrimPrefix(item, "DOCKER_CONFIG=")
		}
	}
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}
		configHome = filepath.Join(home, ".docker")
	}
	configHome, err := filepath.Abs(configHome)
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(root, ".mini-k8s"), "docker-anonymous-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	config := struct {
		Auths   map[string]map[string]string `json:"auths"`
		Plugins []string                     `json:"cliPluginsExtraDirs"`
	}{
		// An empty auth entry prevents Docker from auto-detecting a default helper.
		Auths: map[string]map[string]string{"https://index.docker.io/v1/": {}},
		Plugins: []string{
			filepath.Join(configHome, "cli-plugins"),
			"/Applications/Docker.app/Contents/Resources/cli-plugins",
		},
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	result := make([]string, 0, len(env)+2)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_AUTH_CONFIG", "BUILDX_BUILDER", "BUILDX_CONFIG", "BUILDKIT_HOST":
			continue
		}
		result = append(result, item)
	}
	result = append(result, "DOCKER_CONFIG="+dir, "DOCKER_HOST="+endpoint)
	return result, cleanup, nil
}
