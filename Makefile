# Cross-compile targets for the agentctl client binary.
#
# agentctl ships as a single static Go binary to two host families:
#   darwin-arm64 — the operator's Mac
#   linux-amd64  — agent VMs spun up via the project's Terraform/cloud-init
#
# CGO is disabled so the binaries don't pick up host libc dependencies;
# -ldflags="-s -w" strips symbol + DWARF tables to shrink them by ~30%.
# Both knobs are required for the binary to be drop-in deployable.

GATEWAY_DIR := gateway
BIN_DIR     := bin
PKG         := ./cmd/agentctl

VERSION ?= 0.1.3
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT)

.PHONY: agentctl-all agentctl-darwin-arm64 agentctl-linux-amd64 clean verify-cloud-init

# Render cloud-init/user-data.yaml.tpl with sample values + validate it parses
# as YAML. Catches the indent() whitespace-bug class before `tofu apply` pushes
# a broken user-data to the VM. See scripts/verify-cloud-init.sh for details.
verify-cloud-init:
	@./scripts/verify-cloud-init.sh

agentctl-all: agentctl-darwin-arm64 agentctl-linux-amd64

agentctl-darwin-arm64: $(BIN_DIR)
	cd $(GATEWAY_DIR) && \
		CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
		go build -trimpath -ldflags="$(LDFLAGS)" \
		-o ../$(BIN_DIR)/agentctl-darwin-arm64 $(PKG)

agentctl-linux-amd64: $(BIN_DIR)
	cd $(GATEWAY_DIR) && \
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags="$(LDFLAGS)" \
		-o ../$(BIN_DIR)/agentctl-linux-amd64 $(PKG)

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

clean:
	rm -rf $(BIN_DIR)
