#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────
# Quartermaster dev-container setup
# Run INSIDE the container after it starts to install the toolchain.
# ──────────────────────────────────────────────────────────────────────────
set -euo pipefail

echo "==> Updating package lists..."
apt-get update -qq

echo "==> Installing base toolchain..."
apt-get install -y -qq \
    git \
    make \
    curl \
    wget \
    ca-certificates \
    gnupg \
    vim \
    jq \
    sudo \
    containerd \
    2>&1 | tail -1

# ── Go 1.25 ────────────────────────────────────────────────────────────
GO_VERSION="1.25.10"
if ! command -v go >/dev/null 2>&1 || ! go version | grep -q "$GO_VERSION"; then
    echo "==> Installing Go ${GO_VERSION}..."
    curl -sSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C /usr/local -xz
    echo 'export PATH=/usr/local/go/bin:$PATH' >> /etc/profile.d/go.sh
    echo 'export GOPATH=/root/go' >> /etc/profile.d/go.sh
    export PATH=/usr/local/go/bin:$PATH
fi

# ── gh CLI (optional, for qm repo add) ───────────────────────────────
if ! command -v gh >/dev/null 2>&1; then
    echo "==> Installing GitHub CLI..."
    curl -sSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | \
        gpg --dearmor -o /usr/share/keyrings/githubcli-archive-keyring.gpg
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list
    apt-get update -qq && apt-get install -y -qq gh 2>&1 | tail -1
fi

echo ""
echo "==> Dev environment ready =="
go version
git --version | head -1
echo "containerd socket: $(test -S /run/containerd/containerd.sock && echo '✓ mounted' || echo '✗ missing')"
echo "QM source: $(test -f /workspace/go.mod && echo '✓ mounted' || echo '✗ missing')"
echo ""
echo "Quick start:"
echo "  cd /workspace"
echo "  make build"
echo "  make test"
echo "  sudo make integration-test"
