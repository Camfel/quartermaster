# Quartermaster Progress Report

## 🚀 Project Overview
**Quartermaster** is a GitOps-native, single-node container orchestrator designed for homelab environments. It manages workloads (game servers, LLMs, media stacks) by reconciling a Git-based manifest against a `containerd` runtime via direct gRPC (CRI).

## 📊 Current Stats
- **25 Go source files** across **12 packages**
- **59 passing tests** (unit + integration)
- **2 binaries**: `qm` (3.6 MB CLI), `qm-daemon` (38 MB daemon)
- **3 sample stacks**: media, LLM, game server

---

## 🏗️ Architecture

```
┌──────────────┐     ┌───────────────┐     ┌──────────────┐
│  Git Repo    │────▶│  Git Watcher  │────▶│   Daemon     │
│ (source of   │     │  (polling +   │     │  (signal     │
│  truth)      │     │   cooldown)   │     │   handler)   │
└──────────────┘     └───────────────┘     └──────┬───────┘
                                                  │
                                          ┌───────▼───────┐
                                          │  Reconciler   │
                                          │ (diff engine  │
                                          │  + ordering)  │
                                          └───────┬───────┘
                                                  │
                    ┌─────────────────────────────┼──────────────────────────┐
                    │                             │                          │
            ┌───────▼───────┐            ┌───────▼───────┐          ┌───────▼───────┐
            │  CRI Client    │            │  Config Mgr   │          │  Health Check │
            │  (containerd   │            │  (validate +  │          │  (HTTP/TCP    │
            │   gRPC)        │            │   LKG save)   │          │   probes)     │
            └───────┬───────┘            └───────────────┘          └───────────────┘
                    │
     ┌──────────────┼──────────────┬──────────────────┐
     │              │              │                  │
┌────▼────┐  ┌──────▼──────┐ ┌────▼─────┐   ┌───────▼──────┐
│ Secrets │  │  Hardware   │ │ Network  │   │  Containerd  │
│ Manager │  │  Detector   │ │ Manager  │   │  Runtime     │
│(encrypt │  │  (NVIDIA    │ │(profiles │   │              │
│ at rest)│  │   GPU)      │ │ + VPN)   │   │              │
└─────────┘  └─────────────┘ └──────────┘   └──────────────┘
```

---

## ✅ Phase 1: Core Engine (COMPLETE)

| Feature | Package | Details |
|---|---|---|
| Project foundation | `Makefile`, `go.mod` | Build, test, vet, format targets |
| Data model | `pkg/types/` | `Stack`, `Service`, `Resources`, `HealthCheck`, ports, volumes, secrets, GPU |
| Config manager | `pkg/config/` | YAML parsing, save/load, 15+ validation rules |
| CRI client | `pkg/cri/` | `ContainerClient` interface, containerd implementation, mock for testing |
| Reconciler | `pkg/reconciler/` | Create/delete/update containers, hash-based change detection |
| Daemon | `internal/daemon/` | Signal handling, reconciliation loop, health check ticker, LKG rollback |
| CLI | `cmd/qm/` | `up`, `validate`, `create-secret`, `list-secrets`, `version` |

## ✅ Phase 2: GitOps Layer (COMPLETE)

| Feature | Package | Details |
|---|---|---|
| Git Watcher | `pkg/git/` | Polls remote repo via `go-git` + `git fetch`, detects new commits |
| Cooldown | `pkg/git/` | Configurable minimum time between reconciliation triggers (anti-storm) |
| LKG Rollback | `internal/daemon/` | Saves manifest on success; reverts to LKG on reconciliation failure |
| Manifest Validation | `pkg/config/` | Port collisions, image format, restart policies, health checks, user format, dependencies, env vars, secrets, network profiles, duplicate names |
| Config Change Detection | `pkg/reconciler/` | SHA256 hash of mutable fields stored as container label; diff on reconcile |
| Dependency Ordering | `pkg/reconciler/` | Kahn's topological sort — services created in correct dependency order |

## ✅ Phase 3: Hardware & Secrets (COMPLETE)

| Feature | Package | Details |
|---|---|---|
| Secret Manager | `pkg/secrets/` | Reads secrets from filesystem, injects as read-only bind mounts at `/run/secrets` |
| Encryption at Rest | `pkg/secrets/` | NaCl secretbox (XSalsa20-Poly1305); master key in `/etc/quartermaster/master.key` (0400) |
| GPU Detection | `pkg/hardware/` | NVIDIA GPU detection via `nvidia-smi`; device paths, env vars for OCI spec |
| CLI: create-secret | `cmd/qm/` | `echo 'val' \| qm create-secret <name>` — encrypts and stores |
| CLI: list-secrets | `cmd/qm/` | Lists secret names (values never shown) |

## ✅ Phase 4: Advanced Networking (COMPLETE)

| Feature | Package | Details |
|---|---|---|
| Network Profiles | `pkg/network/` | `public`, `internal`, `vpn` — defined, validated, managed |
| VPN Sidecar | `pkg/network/` + `pkg/cri/` | Gateway container PID tracked; dependents share `/proc/<pid>/ns/net` |
| Namespace Sharing | `pkg/cri/` | `withNetworkNamespace()` OCI spec opt for containerd |

## ✅ Beyond the Roadmap

| Feature | Package | Details |
|---|---|---|
| Health Check Monitoring | `pkg/health/` | HTTP and TCP probes against running containers |
| Auto-Restart | `internal/daemon/` | Failed health checks trigger container restart (stop + start) |
| Escalation to Rollback | `internal/daemon/` | 3 consecutive health failures → triggers LKG reconciliation |
| Change Detection | `pkg/reconciler/` | Hash-based diffing detects image/env/port/volume changes; recreates containers |

---

## 📂 Full Package Listing

```
cmd/qm/main.go                          # CLI: up, validate, create-secret, list-secrets, version
cmd/qm-daemon/main.go                   # Daemon entry point: wires all components

internal/daemon/daemon.go               # Reconciliation loop, health checks, LKG rollback

pkg/config/config.go                    # YAML parsing, validation (15+ rules), SaveStack
pkg/config/config_test.go               # 21 tests
pkg/config/testdata/valid.yaml          # Sample valid manifest
pkg/config/testdata/invalid.yaml        # Sample invalid manifest

pkg/cri/interface.go                    # ContainerClient interface + ContainerInfo
pkg/cri/containerd_client.go            # containerd gRPC implementation (secrets/GPU/network)
pkg/cri/mock_client.go                  # Mock for unit testing

pkg/git/watcher.go                      # Git polling with cooldown/debouncing
pkg/git/watcher_test.go                 # Integration test with bare repo

pkg/hardware/detector.go                # NVIDIA GPU detection + OCI helpers
pkg/hardware/detector_test.go           # 7 tests

pkg/health/checker.go                   # HTTP/TCP health probes for running containers
pkg/health/checker_test.go              # 6 tests

pkg/network/manager.go                  # Network profiles, VPN gateway registration
pkg/network/manager_test.go             # 6 tests

pkg/reconciler/reconciler.go            # Reconcile engine, hash diffing, topological sort
pkg/reconciler/reconciler_test.go       # Unit tests: create, delete, hash, sort
pkg/reconciler/wd_test.go               # Working directory test
pkg/reconciler/reconciler_integration_test.go  # Integration test (requires containerd)

pkg/secrets/manager.go                  # Secret resolution, mount prep, encrypted mode
pkg/secrets/manager_test.go             # 6 manager tests
pkg/secrets/crypto.go                   # NaCl secretbox encryption/decryption
pkg/secrets/crypto_test.go              # 7 crypto tests

pkg/types/types.go                      # Stack, Service, Port, Volume, HealthCheck, etc.
```

---

## 🧪 Test Summary: 59 Passing

| Package | Tests | Type |
|---|---|---|
| `pkg/config` | 21 | Unit (validation, save/load, roundtrip) |
| `pkg/git` | 3 | Integration (bare repo clone + poll) |
| `pkg/hardware` | 7 | Unit (GPU detection, devices, env) |
| `pkg/health` | 6 | Unit (probes, port resolution, intervals) |
| `pkg/network` | 6 | Unit (profiles, VPN registration, namespace paths) |
| `pkg/reconciler` | 9 | Unit + Integration (create, delete, hash, sort) |
| `pkg/secrets` | 13 | Unit (encrypt/decrypt, resolve, mount, key mgmt) |

---

## 🔐 Security Posture

| Concern | Implementation |
|---|---|
| Secrets on disk | NaCl secretbox encrypted; 0400 file perms; 0700 directory |
| Secrets in transit (to container) | tmpfs mount; read-only bind; never in env vars |
| Non-root host access | File permissions block other users |
| Per-service isolation | Each container only sees its declared secrets at `/run/secrets/` |
| Root compromise | Unavoidable on single host; master key readable by root |
| Broken manifest | Validation blocks before application |
| Bad image / broken deploy | LKG rollback on reconciliation failure |
| Container crash loop | Health check → restart → escalation to LKG rollback |
| Git repo unreachable | Daemon continues with last known state |

---

## 📅 Remaining Work

### Pre-Deployment
- [ ] Live verification on a Debian host with `containerd`
- [ ] `qm status` command (requires daemon gRPC/HTTP API)

### Nice-to-Have
- [ ] Daemon unit tests (currently tested indirectly via reconciler integration tests)
- [ ] Exec-based health checks (currently HTTP and TCP only)
- [ ] `apt` packaging for one-liner install
- [ ] Prometheus metrics endpoint
- [ ] Structured logging (JSON)
- [ ] AMD/Intel GPU detection

---

## 🚢 Quick Reference

```bash
# Build
make build

# Test
make test

# Validate a stack
./bin/qm validate ./samples/media-stack.yaml

# Preview a stack
./bin/qm up ./samples/media-stack.yaml

# Create an encrypted secret
echo 'mypassword' | ./bin/qm create-secret db-password

# List secrets (names only)
./bin/qm list-secrets

# Run the daemon (requires containerd)
sudo ./bin/qm-daemon
```
