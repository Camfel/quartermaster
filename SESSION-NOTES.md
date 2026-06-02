# Session Notes — 2026-06-02

## What we built (complete)

### 1. `Camfel/media-stack` — GitOps target repo
- 5 managed services + 1 component (dashboard)
- qBittorrent, Prowlarr, Sonarr, Radarr, Jellyfin
- Protocol-aware ports (tcp/udp on same DHT port)
- Dependencies chain, GPU passthrough, health checks
- Dashboard extracted to component (`qm enable dashboard`)

### 2. `Camfel/quartermaster-gui` — Web dashboard container
- Zero external Go deps, stdlib only
- Embedded templates + dark-theme CSS
- Unix socket client to daemon `/v1/status`
- Endpoints: `/`, `/api/status`, `/health`
- **Image pipeline**: `chainguard/go` → `chainguard/static` (both Wolfi, 0 CVEs)
- **CI/CD**: test + govulncheck on PRs; build + push on main → ghcr.io
- **Security**: Trivy weekly + on code change
- **Dependabot**: auto-bumps Go modules, Actions, Docker base images
- Image: `ghcr.io/camfel/quartermaster-gui:latest` (4.2 MiB)

### 3. `Camfel/quartermaster-components` — Curated catalog
- `dashboard/v1.0/stack.yaml` — web GUI
- `media-stack/v1.0/stack.yaml` — Arr suite + Jellyfin
- `vpn/v1.0/stack.yaml` — Gluetun VPN gateway
- `ingress/v0.1/stack.yaml` — nginx reverse proxy

### 4. `Camfel/quartermaster` — Orchestrator engine
- Full source pushed to GitHub
- CLI: `up`, `validate`, `repo add/list`, `status`, `create-secret`, `list-secrets`, `enable`, `disable`, `components list`, `version`
- **Component system**: `qm enable/disable <name>` with daemon live-reload via `POST /v1/reload`
- **New daemon API**: `/v1/components`, `/v1/reload`

### Engine improvements in this session
- `Port.Protocol` field (tcp/udp/sctp) + protocol-aware collision validation
- Daemon socket `0600 → 0660` (group-readable for co-located services)
- Host networking as default (`oci.WithHostNamespace`)
- `make install` target: acl, tmpfiles.d, quartermaster user, systemd unit
- `make uninstall` target
- systemd unit with `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `CAP_DAC_OVERRIDE`, `AmbientCapabilities`
- Default settings path fix (root → `/etc/quartermaster/settings.json`)
- Watcher `RepoURL()` getter for reload logic
- Config reload on SIGHUP-equivalent via API

## Key Learnings

### Quartermaster / GitOps
- Reconciler: poll git → compare hash → pull → validate → config hash → diff against state
- Port collision caught real bug (duplicate 6881 before protocol field)
- Config hash includes all manifest fields — adding Protocol changed all hashes
- Watcher polls every ~30s; reconciliation triggered by hash change
- `depends_on` controls startup order

### Least-privilege daemon
- Dedicated `quartermaster` user (UID 999, no shell)
- Linux caps needed: `CAP_SYS_ADMIN` (mounts), `CAP_NET_ADMIN` (net), `CAP_DAC_OVERRIDE` (files)
- `AmbientCapabilities` in systemd preserves caps across exec
- Containerd socket needs ACL (`setfacl` with `acl` package)
- `/run/quartermaster/` is tmpfs → `tmpfiles.d` config for reboots
- `/etc/quartermaster/` and sub-files must be `quartermaster:quartermaster`

### Container networking
- Without CNI, containers are isolated → host networking is pragmatic for single-host
- Port declarations are validated but not mapped to host without CNI or host-net

### Docker / CI gotchas
- No `go.sum` for zero-dep modules → Docker `COPY go.mod go.sum` fails
- GitHub Actions `docker` driver doesn't support `gha` cache type
- `distroless` has no shell → can't `exec` into it
- Dependabot opened 6 PRs instantly on first push

### Component system
- Settings path mismatch: CLI defaulted to `~/.qm/settings.json`, daemon uses `/etc/quartermaster/settings.json`
- Fix: `DefaultSettingsPath()` now returns `/etc/quartermaster/settings.json` when running as root
- Reload flow: CLI → settings.json → POST /v1/reload → daemon re-reads → update stackFiles → reconcile
- Components merge with user stacks; user services win on name conflicts

## Running stack (this host)

| Service | Port | Image |
|---------|------|-------|
| dashboard | 8090 | ghcr.io/camfel/quartermaster-gui:latest |
| jellyfin | 8096 | lscr.io/linuxserver/jellyfin:latest |
| sonarr | 8989 | lscr.io/linuxserver/sonarr:latest |
| radarr | 7878 | lscr.io/linuxserver/radarr:latest |
| prowlarr | 9696 | lscr.io/linuxserver/prowlarr:latest |
| qbittorrent | 8080 | lscr.io/linuxserver/qbittorrent:latest |

## TODO / Follow-ups

Remaining from earlier session — expanded in `ideas.md`:
- [ ] CNI / container networking (non-host mode)
- [ ] Health-check grace period for slow-starting services
- [ ] WebSocket/SSE live updates for the GUI
- [ ] GUI component toggle buttons (API wired, GUI UI missing)
- [ ] Prometheus metrics endpoint
- [ ] qm logs / qm exec commands
- [ ] Backup component
- [ ] Agent component (auto-remediation, pentest)
- [ ] Rolling updates instead of all-at-once
- [ ] Secrets UI in the dashboard
