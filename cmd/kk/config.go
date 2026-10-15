package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/validation"
)

const (
	createConfigMapUsage = "usage: kk create configmap NAME [--from-literal=KEY=VALUE] [--from-file=[KEY=]PATH] ..."
	createSecretUsage    = "usage: kk create secret generic NAME [--type TYPE] [--from-literal=KEY=VALUE] [--from-file=[KEY=]PATH] ... | " +
		"kk create secret tls NAME --cert FILE --key FILE"
)

// createConfig implements kk create configmap and kk create secret
// generic|tls, like kubectl create.
func (a *app) createConfig(ctx context.Context, p args) error {
	pos := p.positional
	if pos[0] != "secret" {
		if len(pos) != 2 {
			return errors.New(createConfigMapUsage)
		}
		data, err := collectData(p, "ConfigMap", pos[1])
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: pos[1], Namespace: a.namespace}}
		for k, v := range data {
			if utf8.Valid(v) {
				if cm.Data == nil {
					cm.Data = map[string]string{}
				}
				cm.Data[k] = string(v)
			} else {
				if cm.BinaryData == nil {
					cm.BinaryData = map[string][]byte{}
				}
				cm.BinaryData[k] = v
			}
		}
		if _, err := client.ConfigMaps(a.client, a.namespace).Create(ctx, cm); err != nil {
			return createError("configmap", err)
		}
		_, err = fmt.Fprintf(a.out, "configmap/%s created\n", cm.Name)
		return err
	}
	if len(pos) != 3 || (pos[1] != "generic" && pos[1] != "tls") {
		return errors.New(createSecretUsage)
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pos[2], Namespace: a.namespace}, Type: p.value("type", "")}
	if pos[1] == "tls" {
		cert, key := p.value("cert", ""), p.value("key", "")
		if cert == "" || key == "" {
			return errors.New(createSecretUsage)
		}
		certPEM, err := os.ReadFile(cert)
		if err != nil {
			return err
		}
		keyPEM, err := os.ReadFile(key)
		if err != nil {
			return err
		}
		if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
			return err
		}
		s.Type = corev1.SecretTypeTLS
		s.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
	} else {
		data, err := collectData(p, "Secret", s.Name)
		if err != nil {
			return err
		}
		s.Data = data
	}
	if _, err := client.Secrets(a.client, a.namespace).Create(ctx, s); err != nil {
		return createError("secret", err)
	}
	_, err := fmt.Fprintf(a.out, "secret/%s created\n", s.Name)
	return err
}

// createError words API errors like kubectl create configmap|secret.
func createError(kind string, err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status.Message != "" {
		return fmt.Errorf("failed to create %s: %s", kind, apiErr.Status.Message)
	}
	return err
}

// collectData reads --from-file (a file, or every regular file of a
// directory, keyed by base name unless KEY= is given) and --from-literal
// entries; keys must be valid and unique.
func collectData(p args, kind, name string) (map[string][]byte, error) {
	data := map[string][]byte{}
	add := func(key string, value []byte) error {
		if msgs := validation.IsConfigMapKey(key); len(msgs) > 0 {
			return fmt.Errorf("%q is not a valid key name for a %s: %s", key, kind, strings.Join(msgs, "; "))
		}
		if _, dup := data[key]; dup {
			return fmt.Errorf("cannot add key %q, another key by that name already exists in Data for %s %q", key, kind, name)
		}
		data[key] = value
		return nil
	}
	for _, spec := range p.values["from-file"] {
		key, path, named := strings.Cut(spec, "=")
		if !named {
			key, path = "", spec
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			if named {
				return nil, fmt.Errorf("cannot give a key name for a directory path: %s", spec)
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if !e.Type().IsRegular() {
					continue
				}
				content, err := os.ReadFile(filepath.Join(path, e.Name()))
				if err != nil {
					return nil, err
				}
				if err := add(e.Name(), content); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !named {
			key = filepath.Base(path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := add(key, content); err != nil {
			return nil, err
		}
	}
	for _, literal := range p.values["from-literal"] {
		key, value, ok := strings.Cut(literal, "=")
		if !ok {
			return nil, fmt.Errorf("invalid literal source %s, expected key=value", literal)
		}
		if err := add(key, []byte(value)); err != nil {
			return nil, err
		}
	}
	return data, nil
}
