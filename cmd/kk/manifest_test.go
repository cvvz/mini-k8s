package main

import (
	"os"
	"strings"
	"testing"
)

func TestDemoManifest(t *testing.T) {
	file, err := os.Open("../../examples/demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	resources, err := readManifests(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 || resources[0].Kind != "Deployment" || resources[1].Kind != "Service" {
		t.Fatalf("demo must contain one Deployment and one Service: %+v", resources)
	}
}

const deploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
        - name: web
          image: mini-k8s:local
          command: [/mini]
          args: [demo]
`

func TestReadManifestsMultiDocument(t *testing.T) {
	input := deploymentYAML + "---\n\n---\napiVersion: v1\nkind: Service\nmetadata: {name: web}\nspec:\n  selector: {app: web}\n  ports: [{port: 80, targetPort: 8080}]\n"
	got, err := readManifests(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != "Deployment" || got[1].Kind != "Service" || got[0].Metadata.Namespace != "default" {
		t.Fatalf("unexpected resources: %+v", got)
	}
}

func TestReadManifestsRejectsUnsupportedInput(t *testing.T) {
	for name, input := range map[string]string{
		"empty":               "",
		"non-string key":      strings.Replace(deploymentYAML, "name: web", "42: web", 1),
		"unknown field":       deploymentYAML + "unexpected: true\n",
		"duplicate key":       strings.Replace(deploymentYAML, "replicas: 2", "replicas: 2\n  replicas: 3", 1),
		"typed env":           strings.Replace(deploymentYAML, "args: [demo]", "env: [{name: COUNT, value: 2}]", 1),
		"status even empty":   deploymentYAML + "status: {}\n",
		"uid even empty":      strings.Replace(deploymentYAML, "name: web", "name: web\n  uid: ''", 1),
		"resourceVersion":     strings.Replace(deploymentYAML, "name: web", "name: web\n  resourceVersion: 0", 1),
		"ownerUID":            strings.Replace(deploymentYAML, "name: web", "name: web\n  ownerUID: ''", 1),
		"node":                "apiVersion: v1\nkind: Node\nmetadata: {name: worker-1}\nspec: {}\n",
		"bad second document": deploymentYAML + "---\nmetadata: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readManifests(strings.NewReader(input)); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}
