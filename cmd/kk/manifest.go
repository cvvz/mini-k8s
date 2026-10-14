package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
	"mini-k8s/internal/mini"
)

func readManifests(reader io.Reader) ([]mini.Resource, error) {
	decoder := yaml.NewDecoder(reader)
	var resources []mini.Resource
	for document := 1; ; document++ {
		var value any
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("document %d: %w", document, err)
		}
		if value == nil {
			continue
		}
		value, err := jsonValue(value)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", document, err)
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("document %d: resource must be a mapping", document)
		}
		for key, value := range object {
			if strings.EqualFold(key, "status") {
				return nil, fmt.Errorf("document %d: status is server-managed", document)
			}
			if strings.EqualFold(key, "metadata") {
				if metadata, ok := value.(map[string]any); ok {
					for field := range metadata {
						switch strings.ToLower(field) {
						case "uid", "resourceversion", "owneruid":
							return nil, fmt.Errorf("document %d: metadata.%s is server-managed", document, field)
						}
					}
				}
			}
		}
		data, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", document, err)
		}
		resource, err := mini.Decode(data)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", document, err)
		}
		if resource.Kind == "Node" {
			return nil, fmt.Errorf("document %d: Nodes are managed by workers", document)
		}
		resources = append(resources, resource)
	}
	if len(resources) == 0 {
		return nil, errors.New("manifest contains no resources")
	}
	return resources, nil
}

func jsonValue(value any) (any, error) {
	switch value := value.(type) {
	case map[any]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			key, ok := key.(string)
			if !ok {
				return nil, errors.New("YAML mapping keys must be strings")
			}
			converted, err := jsonValue(item)
			if err != nil {
				return nil, err
			}
			result[key] = converted
		}
		return result, nil
	case map[string]any:
		for key, item := range value {
			converted, err := jsonValue(item)
			if err != nil {
				return nil, err
			}
			value[key] = converted
		}
	case []any:
		for i, item := range value {
			converted, err := jsonValue(item)
			if err != nil {
				return nil, err
			}
			value[i] = converted
		}
	}
	return value, nil
}
