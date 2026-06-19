# Quartermaster Development Makefile
#
# Standard workflow:
#   make          → fmt + vet + test + build (CI gate)
#   make restart  → build + restart the systemd daemon (dev loop)
#   make install  → build + deploy to /usr/local/bin + systemd

.PHONY: all fmt vet test build clean install uninstall restart release \
        integration-test setup-deploy-key \
        dev-up dev-shell dev-test dev-integration-test dev-down

BIN_DIR      ?= bin
QM_BIN       ?= $(BIN_DIR)/qm
DAEMON_BIN   ?= $(BIN_DIR)/qm-daemon
INSTALL_DIR  ?= /usr/local/bin
CONFIG_DIR   ?= /etc/quartermaster
SYSTEMD_DIR  ?= /etc/systemd/system
QM_USER      ?= quartermaster
CONTAINERD_SOCK ?= /run/containerd/containerd.sock

# ── Version ────────────────────────────────────────────────────────────
VERSION ?= $(shell cat VERSION)
LDFLAGS ?= -X quartermaster/internal/daemon.apiVersion=$(VERSION) -X main.version=$(VERSION)

# ── Development pipeline ─────────────────────────────────────────────────

# Default: full CI gate.
all: fmt vet test build

# Format + vet only (fast pre-commit check).
check: fmt vet
	@echo "✓ Format + vet passed"

fmt:
	go fmt ./...

vet:
	go vet ./...

# Unit tests (excludes integration tests).
test:
	go test -v -count=1 ./pkg/...

# Unit tests with race detector enabled.
test-race:
	go test -race -count=1 ./pkg/...

# Optimised debug build (keeps symbol tables for profiling).
build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(QM_BIN) ./cmd/qm
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(DAEMON_BIN) ./cmd/qm-daemon
	@echo "✓ Binaries: $(QM_BIN)  $(DAEMON_BIN)"

# Stripped production build.
release:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w $(LDFLAGS)" -o $(QM_BIN) ./cmd/qm
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w $(LDFLAGS)" -o $(DAEMON_BIN) ./cmd/qm-daemon
	@echo "✓ Release binaries: $(QM_BIN)  $(DAEMON_BIN)"

# Build + restart the running systemd daemon (dev loop).
# Also recreates the dashboard container since the Unix socket
# bind mount goes stale when the daemon socket is recreated.
restart: build
	systemctl stop qm-daemon
	cp $(DAEMON_BIN) $(INSTALL_DIR)/qm-daemon
	cp $(QM_BIN) $(INSTALL_DIR)/qm
	systemctl start qm-daemon
	@echo "✓ Daemon restarted"

clean:
	rm -rf $(BIN_DIR)/
	go clean

tidy:
	go mod tidy

# ── System-wide installation ─────────────────────────────────────────────

# Full install: build, copy binaries, configure user, set permissions,
# install systemd unit.  Idempotent — safe to run repeatedly.
install: build
	@echo "==> Installing Quartermaster to $(INSTALL_DIR)..."
	install -m 755 $(QM_BIN) $(INSTALL_DIR)/qm
	install -m 755 $(DAEMON_BIN) $(INSTALL_DIR)/qm-daemon
	@echo "    Binaries installed."
	@echo "==> Ensuring acl is available..."
	@command -v setfacl >/dev/null 2>&1 || { \
		echo "    Installing acl package..."; \
		apt-get update -qq && apt-get install -y -qq acl 2>&1 | tail -1; \
	}
	@echo "==> Creating config directory $(CONFIG_DIR)..."
	mkdir -p $(CONFIG_DIR)
	@if ! id -u $(QM_USER) >/dev/null 2>&1; then \
		echo "==> Creating dedicated system user '$(QM_USER)'..."; \
		useradd -r -m -d /var/lib/$(QM_USER) -s /usr/sbin/nologin $(QM_USER); \
	else \
		echo "    User '$(QM_USER)' already exists."; \
	fi
	@echo "==> Ensuring $(QM_USER) data directories..."
	mkdir -p /var/lib/$(QM_USER)/repos
	mkdir -p /run/quartermaster
	chown -R $(QM_USER):$(QM_USER) /var/lib/$(QM_USER) /run/quartermaster
	chmod 750 /var/lib/$(QM_USER)
	@echo "==> Installing tmpfiles.d config for /run/quartermaster..."
	@echo 'd /run/quartermaster 0750 $(QM_USER) $(QM_USER) -' > /etc/tmpfiles.d/quartermaster.conf
	@echo "==> Setting ownership on $(CONFIG_DIR)..."
	chown $(QM_USER):$(QM_USER) $(CONFIG_DIR)
	chmod 750 $(CONFIG_DIR)
	@for f in $(CONFIG_DIR)/master.key $(CONFIG_DIR)/settings.json; do \
		if [ -f "$$f" ]; then \
			chown $(QM_USER):$(QM_USER) "$$f"; \
			chmod 640 "$$f"; \
		fi; \
	done
	@for d in $(CONFIG_DIR)/keys $(CONFIG_DIR)/secrets; do \
		if [ -d "$$d" ]; then \
			chown -R $(QM_USER):$(QM_USER) "$$d"; \
		fi; \
	done
	@echo "==> Granting containerd socket access..."
	@if [ -S $(CONTAINERD_SOCK) ]; then \
		setfacl -m u:$(QM_USER):rw $(CONTAINERD_SOCK) 2>/dev/null || \
		echo "    Warning: setfacl failed. Ensure $(QM_USER) can access $(CONTAINERD_SOCK)"; \
	else \
		echo "    Warning: $(CONTAINERD_SOCK) not found. Is containerd running?"; \
	fi
	@echo "==> Installing systemd unit..."
	@if [ ! -f $(CONFIG_DIR)/settings.json ]; then \
		echo '{}' > $(CONFIG_DIR)/settings.json; \
		echo "    Created empty $(CONFIG_DIR)/settings.json"; \
	fi
	chown $(QM_USER):$(QM_USER) $(CONFIG_DIR)/settings.json
	chmod 640 $(CONFIG_DIR)/settings.json
	cp docs/systemd/qm-daemon.service $(SYSTEMD_DIR)/qm-daemon.service
	systemctl daemon-reload
	@echo ""
	@echo "╔══════════════════════════════════════════════════════════════╗"
	@echo "║  Quartermaster installed!                                   ║"
	@echo "║                                                            ║"
	@echo "║  Start the daemon:                                         ║"
	@echo "║    sudo systemctl enable --now qm-daemon                    ║"
	@echo "║                                                            ║"
	@echo "║  Check status:                                             ║"
	@echo "║    sudo systemctl status qm-daemon                          ║"
	@echo "║    journalctl -u qm-daemon -f                              ║"
	@echo "║                                                            ║"
	@echo "║  Add a repo:                                               ║"
	@echo "║    qm repo add                                              ║"
	@echo "╚══════════════════════════════════════════════════════════════╝"
	@echo ""

# Uninstall Quartermaster: stops the service and removes all installed files.
uninstall:
	@echo "==> Stopping and disabling qm-daemon..."
	-systemctl stop qm-daemon 2>/dev/null || true
	-systemctl disable qm-daemon 2>/dev/null || true
	@echo "==> Removing systemd unit..."
	rm -f $(SYSTEMD_DIR)/qm-daemon.service
	-systemctl daemon-reload 2>/dev/null || true
	@echo "==> Removing binaries..."
	rm -f $(INSTALL_DIR)/qm $(INSTALL_DIR)/qm-daemon
	@echo ""
	@echo "Quartermaster uninstalled."
	@echo "Config and user kept at $(CONFIG_DIR) and $(QM_USER)."
	@echo "To remove them:  sudo rm -rf $(CONFIG_DIR) && sudo userdel $(QM_USER)"

# ── Integration tests ────────────────────────────────────────────────────

# Requires containerd running on the host.
integration-test: build
	@echo "=== Running Quartermaster end-to-end integration tests ==="
	@echo "Prerequisites: containerd must be running on this host"
	go test -v -tags=integration -count=1 -timeout 180s ./...

# ── Git operations ───────────────────────────────────────────────────────

# Set up an SSH deploy key for a GitHub repo (requires gh CLI).
setup-deploy-key:
	@./scripts/setup-deploy-key.sh --repo $(REPO)

# ── Dev container ────────────────────────────────────────────────────────

DEV_IMAGE    ?= docker.io/library/debian:bookworm-slim
DEV_NAME     ?= qm-dev

dev-up:
	@echo "==> Pulling $(DEV_IMAGE)..."
	ctr image pull $(DEV_IMAGE)
	@echo "==> Creating dev container '$(DEV_NAME)'..."
	@ctr container create \
		--net-host \
		--mount type=bind,src=/run/containerd/containerd.sock,dst=/run/containerd/containerd.sock,options=rbind:rw \
		--mount type=bind,src=$(shell pwd),dst=/workspace,options=rbind:rw \
		--mount type=bind,src=/tmp,dst=/tmp,options=rbind:rw \
		$(DEV_IMAGE) $(DEV_NAME) sleep infinity
	@echo "==> Starting dev container..."
	ctr task start -d $(DEV_NAME)
	@echo "==> Running setup script..."
	ctr task exec --exec-id setup $(DEV_NAME) bash /workspace/scripts/dev-setup.sh || true
	@echo ""
	@echo "Dev container ready.  Enter with: make dev-shell"

dev-shell:
	ctr task exec -t --exec-id shell-$$(date +%s) $(DEV_NAME) bash -c 'cd /workspace && export PATH=/usr/local/go/bin:$$PATH && exec bash'

dev-test:
	ctr task exec --exec-id test-$$(date +%s) $(DEV_NAME) bash -c 'cd /workspace && export PATH=/usr/local/go/bin:$$PATH && make all'

dev-integration-test:
	ctr task exec --exec-id itest-$$(date +%s) $(DEV_NAME) bash -c 'cd /workspace && export PATH=/usr/local/go/bin:$$PATH && make integration-test'

dev-down:
	-ctr task kill -s SIGKILL $(DEV_NAME) 2>/dev/null || true
	-ctr task delete $(DEV_NAME) 2>/dev/null || true
	-ctr container delete $(DEV_NAME) 2>/dev/null || true
	@echo "Dev container removed."
