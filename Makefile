GO ?= go
GOARCH ?= $(shell $(GO) env GOHOSTARCH)

.PHONY: build prepare-etcd test vet e2e
build:
	mkdir -p bin
	$(GO) build -o bin/kk ./cmd/kk
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -o bin/mini-linux ./cmd/mini

prepare-etcd:
	@mkdir -p bin
	@set -eu; \
	if [ -f bin/etcd ]; then \
		chmod 0755 bin/etcd; \
		echo "Using cached bin/etcd"; \
		exit 0; \
	fi; \
	tmp=$$(mktemp -d bin/.etcd-download.XXXXXX); \
	trap 'rm -f "$$tmp/archive.tar.gz" "$$tmp/etcd"; rmdir "$$tmp"' EXIT HUP INT TERM; \
	echo "Downloading official etcd v3.5.17 for linux-$(GOARCH) via host curl"; \
	curl --fail --location --show-error --max-time 90 \
		"https://storage.googleapis.com/etcd/v3.5.17/etcd-v3.5.17-linux-$(GOARCH).tar.gz" \
		-o "$$tmp/archive.tar.gz"; \
	tar -xzf "$$tmp/archive.tar.gz" -C "$$tmp" --strip-components=1 \
		"etcd-v3.5.17-linux-$(GOARCH)/etcd"; \
	chmod 0755 "$$tmp/etcd"; \
	mv "$$tmp/etcd" bin/etcd

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Mutates the running project cluster (scale, kill, restart, down/up); see README.
e2e:
	MINI_E2E=1 $(GO) test ./internal/mini -run '^TestLiveCluster$$' -count=1 -v -timeout 5m
