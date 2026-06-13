# Quartermaster Progress Report

## 🚀 Project Overview
**Quartermaster** is a GitOps-native, single-node container orchestrator designed for homelab environments. It reconciles a Git-based manifest against a `containerd` runtime via direct gRPC.

## 🚀 Current Stats
- **30 Go source files** across **13 packages** (added `pkg/ingress`)
- **73+ passing tests** (unit + integration)
- **2 binaries**: `qm` (8.4 MB CLI), `qm-daemon` (21 MB daemon)
- **13+ services running in production**
- **CLI ported to Cobra** — `qm service <name> [logs|restart|expose]`, `qm user add`, shell completion
- **Three GitOps repos**: components (generic), config (user-specific), secrets (never in git)

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
            │  CRI Client   │            │  Config Mgr   │          │  Health Check │
            │  (containerd  │            │  (validate +  │          │  (HTTP/TCP    │
            │   gRPC)       │            │   LKG save)   │          │   probes)     │
            └───────┬───────┘            └───────────────┘          └───────────────┘
                    │
     ┌──────────────┼──────────────┬──────────────────┐
     │              │              │                  │
┌────▼────┐  ┌──────▼──────┐ ┌────▼─────┐   ┌───────▼──────┐
│ Secrets │  │  Hardware   │ │ Network  │   │  Containerd  │
│ Manager │  │  Detector   │ │ Manager  │   │  Runtime     │
│(encrypt │  │  (NVIDIA    │ │(bridge + │   │              │
│ at rest)│  │   GPU)      │ │ policy   │   │              │
└─────────┘  └─────────────┘ │ routing) │   └──────────────┘
                             └──────────┘
```

---

## ✅ Phase 1: Core Engine (COMPLETE)

| Feature | Package | Details |
|---|---|---|
| Project foundation | `Makefile`, `go.mod` | Build, test, vet, format targets |
| Data model | `pkg/types/` | `Stack`, `Service`, `Resources`, `HealthCheck`, ports, volumes, secrets, GPU, `ConfigMap` |
| Config manager | `pkg/config/` | YAML parsing, save/load, 15+ validation rules |
| CRI client | `pkg/cri/` | `ContainerClient` interface, containerd implementation, mock for testing |
| Reconciler | `pkg/reconciler/` | Create/delete/update containers, hash-based change detection |
| Daemon | `internal/daemon/` | Signal handling, reconciliation loop, health check ticker, LKG rollback |
| CLI | `cmd/qm/` | **Ported to Cobra** — `up`, `validate`, `repo add/list`, `status`, `service <name> [logs|restart]`, `create-secret`, `list-secrets`, `enable`, `disable`, `components list`, `configmap set`, `completion`, `version` |

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

## ✅ Phase 4: Advanced Networking (COMPLETE — v2 Rewrite, June 2026)

The VPN networking was completely rewritten in June 2026. The original v2 design used fwmark-based policy routing, which proved unreliable on this kernel (nfmark doesn't survive bridge forwarding). The final design uses **source-based policy routing**.

| Feature | Package | Details |
|---|---|---|
| Network Profiles | `pkg/network/` | `public`, `internal`, `vpn` — defined, validated, managed |
| Bridge Manager | `pkg/network/bridge.go` | Linux bridge (`qm0`), veth pairs, IPAM (10.42.0.0/24), DNAT port forwarding |
| NetManager interface | `pkg/network/manager.go` | `Setup`, `Attach`, `Detach`, `ExposePorts`, `LookupIP`, `Recover`, `ConfigureVPNGateway`, `Teardown` |
| VPN Policy Routing | `pkg/network/bridge.go` | Source-based: `ip rule from <ip> table 100` with direct routes for bridge (10.42.0.0/24), LAN (192.168.0.0/24), Tailscale (100.64.0.0/10); default via VPN gateway |
| Gateway Configuration | `pkg/network/bridge.go` | FORWARD + MASQUERADE + TCP MSS clamping (set-mss 1350) applied to VPN gateway; async retry |
| DNS Forwarder | `pkg/network/dns.go` | In-process DNS on `10.42.0.1:53`; resolves bridge IPs, unknown hostnames → bridge GW, external via host DNS |
| DNAT Rule Management | `pkg/network/bridge.go` | `ExposePorts` cleans stale rules before adding; `cleanStaleDNAT` on startup; `removePorts` fixed to not skip protocol values |
| Stale Gateway Detection | `pkg/reconciler/` | `getStaleGatewayInfo` returns gateway name + new IP; live-updates DNS and routes without container recreates |
| Dead Task Detection | `pkg/cri/` + `pkg/reconciler/` | `ListContainers` returns `Running` + `PID`; dead tasks treated as missing → recreated |
| Bridge IP Persistence | `pkg/network/bridge.go` | IP map saved to `/var/lib/quartermaster/bridge-ips.json`; recovered on daemon restart |
| Namespace-Safe Netlink | `pkg/network/bridge.go` | `netlink.NewHandleAt()` instead of `netns.Set()` — prevents host routing table corruption |
| Persistent Logging | `pkg/cri/containerd_client.go` | Rotating log files at `/var/lib/quartermaster/logs/<service>/current.log`; configurable size/retention |
| Caddy Access Logging | `pkg/ingress/generator.go` | JSON access logs via stdout, captured by persistent logging |
| Authelia Hardening | `configuration.tmpl` | Rate limiting (5/2m/10m), session expiry (30m/8h), password policy (min 12 chars) |

**VPN egress verified in production** — ProtonVPN tunnel working, host internet unaffected, auto-recovery on gateway restart. 14 services running.

## ✅ Beyond the Roadmap

| Feature | Package | Details |
|---|---|---|
| Health Check Monitoring | `pkg/health/` | HTTP and TCP probes against running containers |
| Auto-Restart | `internal/daemon/` | Failed health checks trigger container restart (stop + start) |
| Escalation to Rollback | `internal/daemon/` | 3 consecutive health failures → triggers LKG reconciliation |
| Change Detection | `pkg/reconciler/` | Hash-based diffing detects image/env/port/volume changes; recreates containers |
| Status API | `internal/daemon/status.go` | Unix socket at `/run/quartermaster/daemon.sock` — `/v1/status`, `/v1/services/<name>`, `/v1/events` (WebSocket) |
| Component System | `cmd/qm/` + `internal/daemon/` | `qm enable/disable`, curated catalog, live reload |
| GUI Dashboard | separate repo | `ghcr.io/camfel/quartermaster-gui` — dark-theme dashboard |
| Tailscale Exposure | `pkg/network/` + `pkg/reconciler/` | Per-service `expose` config: `tailscale` (DNAT), `serve` (HTTPS + ts.net domain), `funnel` (public internet) |
| Caddy Ingress | `pkg/ingress/` | Auto-generated Caddyfile with reverse_proxy + forward_auth, Let's Encrypt TLS, path-based routing |
| Authelia Auth | component | File-based user auth, argon2id hashing, SSO via session cookies, `qm user add` CLI + dashboard UI |
| Seerr (Jellyseerr) | component | Jellyfin-native media requests, connected to Sonarr/Radarr |
| Recyclarr | component | Trash Guides auto-sync for quality profiles to Sonarr/Radarr |
| DDNS | component | Dynamic DNS via ddclient (Namecheap), ConfigMap + secret injection |
| GPU Passthrough | `pkg/hardware/` + component | NVIDIA GPU to Jellyfin for hardware transcoding, nvidia-container-toolkit |
| User Management API | `internal/daemon/` | POST /v1/authelia/users with argon2id hashing, dashboard UI form |
| Command Override | `pkg/types/` + `pkg/cri/` | Service `command:` field for startup wrappers (sed, env substitution) |

---

## 📂 Full Package Listing

```
cmd/qm/main.go                          # CLI (Cobra): up, validate, repo, status, secrets, enable/disable, components, configmap
cmd/qm-daemon/main.go                   # Daemon entry point: wires all components

internal/daemon/daemon.go               # Reconciliation loop, health checks, LKG rollback
internal/daemon/status.go               # HTTP/WebSocket status API on Unix socket
internal/daemon/events.go               # WebSocket event hub (pub/sub)

pkg/config/config.go                    # YAML parsing, validation (15+ rules), SaveStack
pkg/config/config_test.go               # 21 tests
pkg/config/settings.go                  # Settings.json load/save, component expansion
pkg/config/testdata/valid.yaml          # Sample valid manifest
pkg/config/testdata/invalid.yaml        # Sample invalid manifest

pkg/cri/interface.go                    # ContainerClient interface + ContainerInfo (with Running, PID, GatewayIP)
pkg/cri/containerd_client.go            # containerd gRPC implementation (secrets/GPU/network/DNS mount)
pkg/cri/mock_client.go                  # Mock for unit testing

pkg/git/watcher.go                      # Git polling with cooldown/debouncing
pkg/git/watcher_test.go                 # Integration test with bare repo

pkg/hardware/detector.go                # NVIDIA GPU detection + OCI helpers
pkg/hardware/detector_test.go           # 7 tests

pkg/health/checker.go                   # HTTP/TCP health probes for running containers
pkg/health/checker_test.go              # 6 tests

pkg/network/manager.go                  # NetManager interface, profile types, ResolveProfile
pkg/network/bridge.go                   # BridgeManager: bridge, veth, IPAM, DNAT, VPN policy routing, gateway config, recovery
pkg/network/manager_test.go             # Unit tests for profiles + IP allocation

pkg/reconciler/reconciler.go            # Reconcile engine, hash diffing, topological sort, stale gateway detection, dead task detection
pkg/reconciler/reconciler_test.go       # Unit tests: create, delete, hash, sort
pkg/reconciler/wd_test.go               # Working directory test
pkg/reconciler/reconciler_integration_test.go  # Integration test (requires containerd)

pkg/secrets/manager.go                  # Secret resolution, mount prep, encrypted mode
pkg/secrets/manager_test.go             # 6 manager tests
pkg/secrets/crypto.go                   # NaCl secretbox encryption/decryption
pkg/secrets/crypto_test.go              # 7 crypto tests

pkg/types/types.go                      # Stack, Service, Port, Volume, EnvVar, HealthCheck, ConfigMap, etc.
```

---

## 🧪 Test Summary: 73 Passing

| Package | Tests | Type |
|---|---|---|
| `pkg/config` | 21 | Unit (validation, save/load, roundtrip) |
| `pkg/git` | 3 | Integration (bare repo clone + poll) |
| `pkg/hardware` | 7 | Unit (GPU detection, devices, env) |
| `pkg/health` | 6 | Unit (probes, port resolution, intervals) |
| `pkg/network` | 6 | Unit (profiles, IP allocation, short name) |
| `pkg/reconciler` | 9 | Unit + Integration (create, delete, hash, sort) |
| `pkg/secrets` | 13 | Unit (encrypt/decrypt, resolve, mount, key mgmt) |
| `internal/daemon` | 4 | Integration (end-to-end, health check restart, secret injection, volume mount, GPU) |

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
| Dead container task | Detected by reconciler; treated as missing → recreated |
| Stale VPN gateway | Gateway IP label compared each reconcile; auto-recreate dependents |

---

## 📅 Remaining Work

### Near-term
- [ ] `qm service logs <name>` — stream container logs (API exists, needs CLI + Cobra wiring)
- [ ] `qm service restart <name>` — restart a service (API exists, needs CLI wiring)
- [ ] `qm service exec <name> <cmd>` — execute commands inside containers
- [ ] Graceful startup period — wait N seconds before first health probe
- [ ] Exec-based health checks — `type: exec` with command array

### Medium-term
- [ ] Prometheus metrics endpoint — `/v1/metrics`
- [ ] Rolling updates — recreate one container at a time with health verification
- [ ] Event history API — store last N events for GUI timeline
- [ ] Ingress/DNS/TLS component — Caddy with auto Let's Encrypt
- [ ] Webhook support — Git push → instant reconcile

### Nice-to-Have
- [ ] `apt` packaging for one-liner install
- [ ] Structured logging (JSON)
- [ ] AMD/Intel GPU detection
- [ ] Backup component
- [ ] Multi-node support (long-term)

---

## 🚢 Quick Reference

```bash
# Build
make build

# Test
make test

# CLI
qm validate ./samples/media-stack.yaml
qm status
qm enable dashboard
qm components list
qm repo add

# Secrets
echo 'mypassword' | qm create-secret db-password
qm list-secrets

# ConfigMap
qm configmap set vpn-config type=wireguard provider=protonvpn

# Daemon
sudo qm-daemon
```
