# Session Notes — 2026-06-02

## What we built

1. **`Camfel/media-stack`** — GitOps target repo for Quartermaster
   - 6 services: qBittorrent, Prowlarr, Sonarr, Radarr, Jellyfin, Dashboard
   - Protocol-aware port declarations (tcp+udp on same port for DHT)
   - Dependencies chain: qBittorrent → Prowlarr → Sonarr/Radarr → Jellyfin → Dashboard
   - Dashboard mounts daemon socket, runs as `quartermaster` UID

2. **`Camfel/quartermaster-gui`** — Web dashboard container
   - Zero external Go dependencies (pure stdlib)
   - Embedded templates + CSS (dark theme, GitHub palette)
   - Reads daemon status via Unix socket → renders HTML dashboard
   - Endpoints: `/` dashboard, `/api/status` JSON, `/health` probe
   - Multi-stage Dockerfile → `distroless/static:nonroot` (8.4 MB binary)
   - CD to `ghcr.io/camfel/quartermaster-gui:latest` via GitHub Actions
   - CI gate: test + govulncheck on PRs
   - Security: CodeQL + Trivy weekly
   - Dependabot: Go modules, Actions, Docker

3. **Quartermaster engine improvements** (local, not pushed)
   - `Port.Protocol` field (tcp/udp/sctp) with protocol-aware collision validation
   - Daemon socket 0600 → 0660 (group-readable for GUI container)
   - Host networking as default for containers (no CNI needed)
   - `make install` target hardened: acl, tmpfiles.d, user setup, permissions
   - `make uninstall` target
   - systemd unit with `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `CAP_DAC_OVERRIDE`

## Key Learnings

### Quartermaster / GitOps
- The reconciler detects changes by polling git → comparing commit hash →
  pulling → validating → computing config hashes → diffing against current state
- Port collision validator caught a real bug (duplicate 6881 before protocol field existed)
- Config hash includes all manifest fields — adding `Protocol` to Port changed all hashes
- The watcher polls every 30s; reconciliation is triggered by hash change, not by timer
- `depends_on` controls startup order but the reconciler creates containers synchronously
  in manifest order, then handles dependencies separately

### Least-privilege daemon
- Dedicated `quartermaster` user (no shell, no login, UID 999)
- Linux capabilities are needed: `CAP_SYS_ADMIN` for mounts, `CAP_NET_ADMIN` for net,
  `CAP_DAC_OVERRIDE` for file access
- `AmbientCapabilities` in systemd preserves caps across exec
- Containerd socket needs ACL (`setfacl`) — `acl` package must be pre-installed
- `/run/quartermaster/` is tmpfs → needs `tmpfiles.d` config for reboots
- `/etc/quartermaster/` and all sub-files must be owned by `quartermaster:quartermaster`

### Container networking
- Without CNI plugins, containerd isolates containers in their own network namespaces
- Port declarations in the manifest are validated but NOT mapped to host without CNI
- Host networking (`oci.WithHostNamespace(specs.NetworkNamespace)`) is the pragmatic
  choice for single-host homelab — all ports become directly accessible
- The `network` field distinguishes: `public` → host net, `internal` → isolated,
  `vpn` → sidecar namespace

### Docker / CI gotchas
- `go.sum` is NOT created when a Go module has zero external dependencies
- Docker `COPY go.mod go.sum ./` fails if `go.sum` is missing
- GitHub Actions default Docker driver doesn't support `gha` cache — needs
  `docker-container` driver or skip cache
- `distroless/static:nonroot` has no shell — can't `exec` into it for debugging
- Dependabot opened 6 PRs within seconds of pushing the config

### Socket permissions
- Unix socket mode 0600 means only the owner can connect
- 0660 allows group access — safe because the API is read-only (GET only)
- The GUI container runs as `quartermaster` UID to access the daemon socket
- Alternative: share the `quartermaster` group with the container

## TODO / Follow-ups

### Immediate
- [ ] Merge or close 6 Dependabot PRs on `Camfel/quartermaster-gui`
- [ ] Trigger `security` workflow manually to verify CodeQL + Trivy
- [ ] Push Quartermaster source to GitHub (currently local-only, all changes on disk)

### Quartermaster improvements
- [ ] Add health checks to all media-stack services (only Jellyfin has one)
- [ ] Add health-check grace period — Jellyfin migration triggers false unhealthy
      during startup (container starts but HTTP endpoint isn't ready yet)
- [ ] Consider CNI setup (or document host-networking as the official approach)
- [ ] The `qm status` command briefly showed "No containers managed" after restart
      — possible race condition between API startup and first reconciliation
- [ ] `make install` should verify containerd is running and socket exists
- [ ] Consider adding `qm dashboard` command that opens the GUI URL

### GUI enhancements
- [ ] Show port mappings on the dashboard
- [ ] Add "last reconcile error" to the dashboard banner
- [ ] Consider WebSocket or SSE instead of meta-refresh for live updates
- [ ] Add a `/api/health` endpoint that proxies the daemon health checks
- [ ] Mobile-responsive layout improvements

### Security / hardening
- [ ] Run `gosec` on Quartermaster source before pushing to GitHub
- [ ] The `master.key` file is 0400 but in a 0750 directory —
      the directory should perhaps be 0700
- [ ] Secrets directory permissions audit
- [ ] Consider adding a `--read-only` flag to the daemon for dry-run mode

### Media stack operations
- [ ] Document post-deployment setup for each Arr service
- [ ] VPN sidecar component for qBittorrent (already has `network: vpn` support)
- [ ] Add Bazarr (subtitle manager) to the media stack
- [ ] Add Jellyseerr (media request system) to the media stack
- [ ] Volume paths: `/mnt/media` and `/mnt/downloads` should be on persistent storage
