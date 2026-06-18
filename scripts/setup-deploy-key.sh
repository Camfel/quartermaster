#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────
# Quartermaster — SSH deploy key setup helper
#
# Uses the GitHub CLI (gh) to create an Ed25519 deploy key, register it
# as a deploy key on a repo, seed the known_hosts file, and print the
# matching settings.json snippet.
#
# Usage:
#   ./setup-deploy-key.sh --repo owner/repo
#   ./setup-deploy-key.sh --repo owner/repo --allow-write
#   ./setup-deploy-key.sh --repo owner/repo --force
#
# IMPORTANT: Run with bash, not sh:
#   ./setup-deploy-key.sh --repo ...     ✓
#   bash setup-deploy-key.sh --repo ...  ✓
#   sh setup-deploy-key.sh --repo ...    ✗ (needs bash)
#
# Prerequisites:
#   - gh CLI installed and authenticated (gh auth login)
#   - ssh-keygen (usually pre-installed)
# ──────────────────────────────────────────────────────────────────────────
set -euo pipefail

# Guard against being run with plain sh.
if [ -z "${BASH_VERSION:-}" ]; then
    echo "ERROR: this script requires bash. Run: bash $0 $*" >&2
    exit 1
fi

# ── Defaults ──────────────────────────────────────────────────────────────
REPO=""
KEY_DIR="/etc/quartermaster/keys"
KEY_NAME=""          # derived from REPO if unset
FORCE=false
ALLOW_WRITE=false
TITLE="quartermaster" # deploy-key title shown in GitHub UI

# ── Parse args ────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        --repo)
            REPO="$2"; shift 2 ;;
        --key-dir)
            KEY_DIR="$2"; shift 2 ;;
        --key-name)
            KEY_NAME="$2"; shift 2 ;;
        --title)
            TITLE="$2"; shift 2 ;;
        --allow-write)
            ALLOW_WRITE=true; shift ;;
        --force)
            FORCE=true; shift ;;
        -h|--help)
            cat <<'HELP'
Quartermaster — SSH deploy key setup helper

Uses the GitHub CLI (gh) to create an Ed25519 deploy key, register it
on a repo, seed the known_hosts file, and print the settings snippet.

Usage:
  ./setup-deploy-key.sh --repo owner/repo
  ./setup-deploy-key.sh --repo owner/repo --allow-write
  ./setup-deploy-key.sh --repo owner/repo --force

Options:
  --repo          GitHub repo as owner/repo (required)
  --key-dir       Directory for keys (default: /etc/quartermaster/keys)
  --key-name      Key filename (default: deploy-owner-repo)
  --title         Deploy key title in GitHub UI (default: quartermaster)
  --allow-write   Grant write access to the deploy key
  --force         Overwrite existing key pair

Prerequisites: gh CLI installed and authenticated (gh auth login)
HELP
            exit 0 ;;
        *)
            echo "Unknown flag: $1" >&2
            echo "Run with --help for usage." >&2
            exit 1 ;;
    esac
done

# ── Validation ────────────────────────────────────────────────────────────
if [[ -z "$REPO" ]]; then
    echo "ERROR: --repo is required (e.g. --repo my-org/quartermaster-stacks)" >&2
    exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
    echo "ERROR: 'gh' CLI not found. Install: https://cli.github.com" >&2
    exit 1
fi

if ! gh auth status >/dev/null 2>&1; then
    echo "ERROR: 'gh' is not authenticated. Run: gh auth login" >&2
    exit 1
fi

# Derive a safe key name from the repo slug if not set.
if [[ -z "$KEY_NAME" ]]; then
    KEY_NAME="deploy-$(echo "$REPO" | tr '/' '-')"
fi

PRIVATE_KEY="${KEY_DIR}/${KEY_NAME}"
PUBLIC_KEY="${PRIVATE_KEY}.pub"
KNOWN_HOSTS="${KEY_DIR}/known_hosts"

# ── Create key directory ──────────────────────────────────────────────────
sudo mkdir -p "$KEY_DIR"
# Ensure the quartermaster daemon user can read the keys.
if id quartermaster >/dev/null 2>&1; then
    sudo chown quartermaster:quartermaster "$KEY_DIR" 2>/dev/null || true
fi
chmod 700 "$KEY_DIR"

# ── Generate SSH key ──────────────────────────────────────────────────────
if [[ -f "$PRIVATE_KEY" ]] && ! $FORCE; then
    echo "✓ SSH key already exists: ${PRIVATE_KEY}"
    echo "  Use --force to overwrite."
else
    echo "→ Generating Ed25519 SSH key: ${PRIVATE_KEY}"
    ssh-keygen -t ed25519 -C "quartermaster-${REPO}" -N "" -f "$PRIVATE_KEY" -q
    chmod 600 "$PRIVATE_KEY"
    chmod 644 "$PUBLIC_KEY"
    # Ensure the quartermaster daemon user can read the keys.
    if id quartermaster >/dev/null 2>&1; then
        sudo chown quartermaster:quartermaster "$PRIVATE_KEY" "$PUBLIC_KEY" 2>/dev/null || true
    fi
    echo "✓ Key generated"
fi

# ── Register deploy key on GitHub ─────────────────────────────────────────
GH_ARGS=(repo deploy-key add "$PUBLIC_KEY" --repo "$REPO" --title "$TITLE")
if $ALLOW_WRITE; then
    GH_ARGS+=(--allow-write)
    PERM_NOTE="read-write ⚠"
else
    PERM_NOTE="read-only"
fi

echo "→ Adding deploy key to ${REPO} (${PERM_NOTE}) ..."
if gh "${GH_ARGS[@]}" 2>&1; then
    echo "✓ Deploy key registered (${PERM_NOTE})"
else
    echo "ERROR: Failed to add deploy key. Check that you have admin access to ${REPO}." >&2
    exit 1
fi

# ── Seed known_hosts ──────────────────────────────────────────────────────
echo "→ Seeding known_hosts ..."
# Scan all key types — go-git may negotiate RSA, ECDSA, or Ed25519.
ssh-keyscan github.com 2>/dev/null | grep -v '^#' > "$KNOWN_HOSTS"
chmod 644 "$KNOWN_HOSTS"
echo "✓ known_hosts: ${KNOWN_HOSTS}"

# ── Print settings snippet ────────────────────────────────────────────────
REPO_URL="git@github.com:${REPO}.git"
STACK_PATH="/var/lib/quartermaster/repos/$(echo "$REPO" | tr '/' '-')"

cat <<JSON

╔══════════════════════════════════════════════════════════════════════╗
║  Deploy key ready!  Add this to your settings.json repos[] array:    ║
╚══════════════════════════════════════════════════════════════════════╝

    {
      "url": "${REPO_URL}",
      "branch": "main",
      "ssh_key_path": "${PRIVATE_KEY}",
      "ssh_known_hosts_path": "${KNOWN_HOSTS}",
      "local_path": "${STACK_PATH}",
      "stack_file": "stack.yaml",
      "poll_interval": "30s",
      "cooldown": "30s"
    }

Public key fingerprint:
  $(ssh-keygen -lf "$PUBLIC_KEY" | awk '{print $2, $4}')

To test SSH access:
  ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile=${KNOWN_HOSTS} -i ${PRIVATE_KEY} -T git@github.com

JSON
