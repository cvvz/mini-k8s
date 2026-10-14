package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnonymousDockerConfigIsIsolatedAndPreservesEndpoint(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".mini-k8s"), 0700); err != nil {
		t.Fatal(err)
	}
	userDir := t.TempDir()
	userConfig := []byte(`{"credsStore":"desktop","currentContext":"desktop-linux"}`)
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), userConfig, 0600); err != nil {
		t.Fatal(err)
	}
	env, cleanup, err := anonymousDockerEnv(root, "unix:///Users/test/.docker/run/docker.sock", []string{
		"DOCKER_CONFIG=" + userDir, "DOCKER_CONTEXT=desktop-linux",
		"DOCKER_HOST=tcp://wrong:2375", "DOCKER_TLS_VERIFY=1",
		"DOCKER_CERT_PATH=/unused", "BUILDX_BUILDER=remote", "BUILDX_CONFIG=/remote-config",
		"BUILDKIT_HOST=tcp://remote:1234", "DOCKER_API_VERSION=1.47", "MINI_TOKEN=unchanged",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	values := map[string]string{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		values[key] = value
	}
	if values["DOCKER_HOST"] != "unix:///Users/test/.docker/run/docker.sock" || values["MINI_TOKEN"] != "unchanged" {
		t.Fatalf("wrong environment: host=%q", values["DOCKER_HOST"])
	}
	if values["DOCKER_API_VERSION"] != "1.47" {
		t.Fatal("explicit Docker API version was not preserved")
	}
	for _, key := range []string{"DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "BUILDX_BUILDER", "BUILDX_CONFIG", "BUILDKIT_HOST"} {
		if _, exists := values[key]; exists {
			t.Errorf("inherited conflicting environment %s", key)
		}
	}
	configDir := values["DOCKER_CONFIG"]
	if configDir == userDir || !strings.HasPrefix(configDir, filepath.Join(root, ".mini-k8s")+string(os.PathSeparator)) {
		t.Fatalf("configuration not isolated: %s", configDir)
	}
	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Auths       map[string]map[string]string `json:"auths"`
		CredsStore  string                       `json:"credsStore"`
		CredHelpers map[string]string            `json:"credHelpers"`
		Plugins     []string                     `json:"cliPluginsExtraDirs"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Auths) == 0 || config.CredsStore != "" || len(config.CredHelpers) != 0 {
		t.Fatal("config would allow Docker to select a credential helper")
	}
	for _, auth := range config.Auths {
		if len(auth) != 0 {
			t.Fatal("anonymous config contains credentials")
		}
	}
	if len(config.Plugins) == 0 || config.Plugins[0] != filepath.Join(userDir, "cli-plugins") {
		t.Fatal("lost Docker plugin discovery")
	}
	original, err := os.ReadFile(filepath.Join(userDir, "config.json"))
	if err != nil || string(original) != string(userConfig) {
		t.Fatal("user Docker configuration was modified")
	}
}
