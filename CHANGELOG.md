# Changelog

All notable changes to Quartermaster.

## [Unreleased]

### Added
- Prometheus/VictoriaMetrics-compatible metrics endpoint (`/v1/metrics`)
- Gotify push notification integration for critical alerts
- Caddy reverse proxy with automatic TLS (Let's Encrypt)
- Tailscale component for VPN-free remote access
- Container logging with automatic rotation
- ConfigMap system for user-overridable component defaults
- `command:` field support for container startup wrappers
- Gateway IP tracking with automatic dependent recreation

### Changed
- CLI ported to Cobra: `qm service logs/restart`, `qm secret create/list`, `qm validate`
- VPN routing switched from fwmark to source-based policy routing
- DNS forwarder resolves unknown short hostnames to bridge gateway
- Metrics use `v2.Memory.Anon` (RSS) instead of `v2.Memory.Usage` (includes page cache)
- `/etc/hosts` entries use bridge gateway IP (`10.42.0.1`) for host-networked services

### Fixed
- Stale DNAT rule accumulation from `ruleToDeleteArgs` skipping protocol values
- Service renames now properly clean up old containers
- `Detach` always attempts cleanup regardless of profile parameter
- DNAT OUTPUT chain rules added for host-local access to forwarded ports

## [0.1.0] - 2026-05-15

### Added
- Core reconciliation loop: observe → diff → act
- containerd CRI client: pull, create, start, stop, delete
- YAML manifest loading, validation, and settings persistence
- Linux bridge (`qm0`) with IPAM, DNAT port forwarding
- NaCl secretbox encryption for secrets at rest
- HTTP/TCP health checks with configurable intervals
- Git polling via go-git (zero os/exec)
- LKG (Last Known Good) rollback on reconciliation failure
- Topological service ordering via Kahn's algorithm
- GPU detection and NVIDIA OCI spec injection
- Bridge state persistence across daemon restarts
- VPN policy routing for egress through gateway
