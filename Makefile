GO ?= go
GOARCH ?= $(shell $(GO) env GOHOSTARCH)

.PHONY: build prepare-etcd test vet e2e e2e-quick
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

# Ginkgo e2e suite against the running project cluster; see test/e2e/README.md.
# E2E_LABELS is a Ginkgo label filter (e.g. '!Disruptive'), E2E_FOCUS a spec
# regex. E2E_PROCS Ginkgo processes run the specs in parallel; Serial specs
# run on their own once the others are done.
E2E_PROCS ?= 4
E2E_ARGS := -v --procs=$(E2E_PROCS) --timeout=30m
ifneq ($(E2E_LABELS),)
E2E_ARGS += --label-filter='$(E2E_LABELS)'
endif
ifneq ($(E2E_FOCUS),)
E2E_ARGS += --focus='$(E2E_FOCUS)'
endif

e2e:
	MINI_E2E=1 $(GO) tool ginkgo $(E2E_ARGS) ./test/e2e

# Non-disruptive specs only: never restarts or stops cluster components.
e2e-quick:
	$(MAKE) e2e E2E_LABELS='!Disruptive'
