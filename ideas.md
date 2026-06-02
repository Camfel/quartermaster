# Quartermaster Ideas & Future Roadmap

> Expanded from `ideas.md` — potential features, components, and directions.
> None are committed; this is a brainstorming document.

---

## Component catalog improvements

Status: **v1 shipped** (`Camfel/quartermaster-components`, `qm enable/disable`).

- [ ] **Versioning strategy** — currently pinned to directories (`v1.0/`). Consider:
  - Tag/release-based versioning where `qm enable dashboard --version v1.2` pulls a git tag
  - `latest` alias that always resolves to the newest version
  - Semantic version constraints (`>=1.0,<2.0`)
  - Component manifest with metadata (description, author, icon, dependencies on other components)
- [ ] **Component search/discovery** — `qm components search <query>` to search the catalog
- [ ] **Component validation** — validate component stacks against the current Quartermaster version before enabling
- [ ] **One-click install from GUI** — the dashboard shows available components with toggle switches (needs GUI API updates)
- [ ] **User-contributed components** — allow PRs to the catalog repo with review/CI checks

---

## GUI container enhancements

Status: **v1 shipped** (`Camfel/quartermaster-gui`).

- [ ] **Live updates** — replace `<meta http-equiv="refresh">` with WebSocket or SSE for real-time status
- [ ] **Service restart button** — `POST /v1/services/<name>/restart` on the daemon, button in the GUI
- [ ] **Health history** — show health check timeline (passed/failed over time)
- [ ] **Resource metrics** — CPU/Memory/GPU usage per container (requires cadvisor or containerd metrics)
- [ ] **Component toggle** — enable/disable components from the GUI with one click
- [ ] **Mobile-responsive layout** — current CSS is basic; needs responsive breakpoints
- [ ] **Dark/light theme toggle**
- [ ] **Notification feed** — reconciliation errors, health failures, watcher events as a live stream

---

## Multi-provider Git support

- [ ] **Codeberg support** — the `pkg/git/watcher.go` already uses go-git which supports any Git URL. Add:
  - SSH deploy key setup helper for Codeberg (`make setup-deploy-key --provider codeberg`)
  - Documented settings.json examples for Codeberg
- [ ] **Self-hosted Git** — Gitea, GitLab on-prem. Test and document.
- [ ] **Multiple deploy keys** — currently one key per repo; support per-repo SSH key configuration
- [ ] **Webhook support** — optional webhook receiver instead of polling (faster, less load)

---

## Load balancer / ingress component

Status: **skeleton exists** (`ingress/v0.1/stack.yaml`).

- [ ] **Dynamic routing** — ingress watches Quartermaster services and auto-generates nginx/Caddy config
- [ ] **DNS integration** — `service.dashboard.example.com` → routes to dashboard on port 8090
- [ ] **Automatic TLS** — Let's Encrypt via Caddy or cert-manager
- [ ] **Dynamic DNS** — update a public DNS record when the host IP changes (DuckDNS, Cloudflare, etc.)
- [ ] **TCP/UDP proxying** — beyond HTTP, support raw TCP/UDP forwarding for game servers, etc.
- [ ] **Rate limiting and basic auth** per service

---

## New potential components

### Container registry (`container-repo`)
- [ ] Local OCI registry for caching images, reducing external pulls
- [ ] Acts as a pull-through cache for Docker Hub, ghcr.io, etc.
- [ ] Can serve custom images (e.g. self-built Quartermaster binaries)

### Backup (`backup`)
- [ ] Automated backup of `/opt/*/config` directories (Arr configs, Jellyfin metadata)
- [ ] Support targets: local disk, S3-compatible, rsync, restic
- [ ] Scheduled or on-demand
- [ ] Restore workflow from the GUI

### llama.cpp (`llama-cpp`)
- [ ] Run a local LLM on the homelab GPU
- [ ] OpenAI-compatible API endpoint
- [ ] Optional: expose to the agent component for automated troubleshooting

---

## Agent component

The most ambitious idea — an AI "co-pilot" for Quartermaster.

### Pentest / security scanning
- [ ] Scan the host and running containers for misconfigurations (open ports, weak permissions)
- [ ] Run as a GitHub Actions CI check on PRs to the Quartermaster repo itself
- [ ] Generate a security report on the dashboard

### LLM integration
- [ ] Connect to a local LLM (via the llama-cpp component) or a remote one (OpenAI API)
- [ ] Natural language queries: "show me all services that failed health checks today"
- [ ] Automated troubleshooting: agent reads logs, suggests fixes, can restart services

### Component generation
- [ ] Point the agent at any GitHub repo and it creates a `stack.yaml` (component)
- [ ] Reads Dockerfile/docker-compose.yml → generates Quartermaster manifest
- [ ] Could use an LLM or a template-based approach
- [ ] Output: PR to the components repo

### Auto-remediation
- [ ] Runs on a schedule or is triggered by health check failures
- [ ] Attempts to fix common issues:
  - Restart crashed containers
  - Free disk space (prune old images)
  - Re-pull images that failed health checks
  - Roll back to LKG if multiple services fail
- [ ] Logs everything it does for audit
- [ ] Rate-limited: won't restart the same service more than N times per hour

### Agent as a component
- [ ] Packaged as a Quartermaster component itself
- [ ] `qm enable agent` — includes an LLM backend + the agent logic
- [ ] Optional: agent has its own dashboard panel showing its action history

---

## Quartermaster core improvements

- [ ] **CNI / container networking** — proper bridge networking with port forwarding instead of host networking
- [ ] **Graceful startup period** — health checks should wait N seconds before first probe (Jellyfin migration issue)
- [ ] **Rolling updates** — update one container at a time with health verification instead of all at once
- [ ] **Event history API** — store last N reconciliation events, health events for the GUI
- [ ] **Multi-node** — remote agents that report back to a central Quartermaster (long-term)
- [ ] **Snapshot/restore** — save the entire desired state + LKG to a tarball for disaster recovery
- [ ] **Secrets UI** — manage secrets from the GUI instead of CLI only
- [ ] **`qm logs <service>`** — stream container logs via the daemon socket
- [ ] **`qm exec <service>`** — execute commands inside a container
- [ ] **Plugin system** — third-party extensions (custom health checks, custom watchers, custom actions)
- [ ] **Prometheus metrics endpoint** — `/v1/metrics` for Grafana dashboards
