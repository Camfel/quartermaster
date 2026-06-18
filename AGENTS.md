# Quartermaster Agent Instructions

> Rules, patterns, and lessons learned. Follow these when contributing to this codebase.

## Current Architecture (v2)

Before making changes, understand what exists:

| Package | Purpose |
|---|---|
| `cmd/qm` | Cobra CLI: `validate`, `status`, `service logs/restart`, `secret create/list` |
| `cmd/qm-daemon` | Entry point: component wiring, signal handling |
| `internal/daemon` | Core loop: reconcile, health checks, LKG rollback, status API, metrics |
| `pkg/reconciler` | GitOps reconciliation: observe→diff→act, topological sort, config hashing |
| `pkg/cri` | containerd client: pull, create, start, stop, delete, logs, stats, GPU injection |
| `pkg/network` | Bridge (`qm0`), IPAM, DNAT, DNS forwarder, VPN policy routing |
| `pkg/config` | YAML manifest loading/validation/saving, settings persistence |
| `pkg/types` | Data model: Stack, Service, Port, Volume, HealthCheck, Ingress, GPU |
| `pkg/secrets` | NaCl secretbox encryption, tmpfs file mounts |
| `pkg/health` | HTTP/TCP probes against containers |
| `pkg/git` | go-git polling — zero os/exec, no secrets in process listings |
| `pkg/metrics` | Prometheus/VictoriaMetrics-compatible metrics endpoint |
| `pkg/ingress` | Caddyfile + /etc/hosts generation |
| `pkg/hardware` | GPU detection via filesystem checks — zero os/exec |

**Build:** `make all` (fmt+vet+test+build), `make integration-test` (requires containerd).  
**CI:** `.github/workflows/ci.yml` — runs `make all` on push and PR.  
**Tests:** 11 packages, unit + integration. Zero os/exec in production code.

## Language & Dependencies

- **Go only.** No JavaScript, Python, shell scripts, or other languages in the core engine.
- **Standard library first.** Reach for `net/http`, `crypto`, `encoding/json` before adding dependencies.
- **Only use well-known, security-audited Go packages** when a dependency is unavoidable.
- **No `os/exec` for system operations.** Use Go libraries: `netlink` for networking, `go-iptables` for firewall, `go-git` for Git.
- **CLI uses Cobra.** Structured subcommands, auto-generated `--help`, shell completion. No hand-rolled `os.Args` parsing.

## Networking Rules

These were learned the hard way — violating any of them breaks the host's internet.

1. **`netlink.NewHandleAt(nsHandle)`, never `netns.Set()`.**  
   `netns.Set()` changes the calling goroutine's namespace, and Go's scheduler can leak operations to the host. Always use a scoped handle:
   ```go
   handle, _ := netlink.NewHandleAt(nsHandle)
   defer handle.Delete()
   handle.RouteAdd(...)  // safe — scoped to the namespace
   ```

2. **Policy routing, never PID-based namespace sharing.**  
   Do NOT use `/proc/<pid>/ns/net`. It breaks on daemon restart, PID reuse, and gateway recreation. Instead, give every container its own netns and use `ip rule from <ip> table 100` to route egress through the gateway.

3. **Verify iptables rules actually stuck.**  
   Containers (especially VPN gateways) reset their firewall during startup. Rules added during `StartContainer` may be wiped moments later. Use retry with verification (`ipt.List` + check) — not blind `ipt.Append`.

4. **Never add a default route on the host.**  
   The only default route on the host must point at the real gateway (`192.168.0.1`). If you see `default via 10.42.0.1 dev qm0`, you've leaked a container route to the host. Delete it immediately and fix the namespace scoping.

5. **DNS for bridge-networked containers must be bind-mounted.**  
   The `/etc/netns/<name>/resolv.conf` trick only works for host processes using `nsenter`. Containers have their own mount namespace and won't see it. Always add a read-only bind mount of the resolv.conf into the container at `/etc/resolv.conf`.

## Architecture Principles

6. **Interfaces at package boundaries.**  
   `ContainerClient`, `NetManager` — concrete implementations behind interfaces. Enables mocking, testing, and future runtime swaps.

7. **Single source of truth for every piece of state.**  
   Bridge IPs live in `BridgeManager.ips`, nowhere else. No `bridgeIPs` map on the CRI client. No `serviceIPs` map on the health checker. One owner, one mutex.

8. **Idempotency is mandatory.**  
   `Setup()` must be safe to call twice. `Attach()` must clean up stale namespaces before creating new ones. Every `RouteAdd` checks `os.IsExist`. Every `iptables.Append` checks `iptables.Exists` first.

9. **Critical state must survive daemon restarts.**  
   The `bridge-ips.json` file is written on every allocation and read on startup. After a restart, the daemon must recover the exact same state without rescanning containers or namespaces.

10. **Track gateway IP at container creation, detect staleness on reconcile.**  
    Store `quartermaster.gateway-ip` as a container label. On every reconciliation pass, compare it with `NetManager.LookupIP(gatewayName)`. If different → auto-recreate the dependent with fresh routes and DNS.

11. **Check task running state, not just container existence.**  
    A container object can exist while its task is dead. `ListContainers` must return `Running bool` + `PID uint32`. The reconciler treats `Running == false` as missing → recreate.

## Security

12. **Secrets are never environment variables.**  
    Mount them as read-only files at `/run/secrets/` on tmpfs. Never pass them via `-e` or `Env` in the OCI spec. `ps aux` must not leak them.

13. **Encryption at rest for all secrets.**  
    NaCl secretbox (XSalsa20-Poly1305). Master key at `/etc/quartermaster/master.key` with `0400` permissions.

14. **Least privilege for the daemon.**  
    The `quartermaster` system user runs with specific Linux capabilities (`CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, `CAP_DAC_OVERRIDE`), never full root. Containers run as non-root users whenever possible (`user: "uid:gid"` in manifests).

## Reconciliation

15. **Config hash includes all mutable fields.**  
    Image, env, ports, volumes, user, network, GPU — anything that would require a container recreate. Name changes don't affect the hash (name is identity, not config).

16. **Topological sort for startup order.**  
    Kahn's algorithm. Services with `depends_on` start after their dependencies.

17. **Fail gracefully — one service failure doesn't stop the loop.**  
    If `CreateContainer` fails for service A, log the error and continue to service B. Never `return err` and abandon the remaining services.

18. **LKG rollback on reconciliation failure.**  
    If the entire reconciliation pass fails, revert to the Last Known Good manifest and retry. The LKG is saved on every successful pass.

19. **Health check escalation.**  
    One failure → restart the container. Three consecutive failures → trigger LKG reconciliation. Health check interval and max failures are configurable.

## Testing

20. **Every package has unit tests.**  
    Mock the `ContainerClient` interface for the reconciler. Use `netlink` on loopback for network tests. Integration tests require containerd and use build tag `//go:build integration`.

21. **No test should require a specific host state.**  
    Tests create temp directories, bare Git repos, and use unique containerd namespaces. Always clean up in `defer`.

22. **Standard test commands — use these, not raw `go test`.**

    ```bash
    make all          # CI gate: fmt + vet + test + build (run before pushing)
    make check        # fast pre-commit: fmt + vet only
    make test         # all unit tests (pkg/...)
    make integration-test  # integration tests (requires containerd)
    ```

    Never push without `make all` passing. Never skip vet — `go vet` catches real bugs.

23. **Keep tests fast.**  
    Unit tests should complete in under 5 seconds. Integration tests get 180s timeout. If a test needs a real container runtime, it belongs behind the `integration` build tag, not in the unit suite.

24. **One test file per package, named `<package>_test.go`.**  
    Multiple test files in a package are fine for large packages (reconciler has 3). Group related tests with `t.Run`.

25. **Use `testify` for integration tests, stdlib `testing` for unit tests.**  
    `require.NoError` and `assert.Equal` are acceptable in integration tests for readability. Unit tests use plain `t.Error`/`t.Fatal` to avoid pulling in the dependency.

## Code Style

26. **Section comments with `───` separators** for logical blocks within functions.
27. **Log at the right level:** `log.Printf` for operational info, `log.Printf("Warning: ...")` for non-fatal issues, `fmt.Errorf` for errors returned to callers.
28. **Package documentation** at the top of the primary file (`// Package network — bridge.go`).
29. **ShortName helper** for interface naming (8-char truncation, consistent across all packages).

## Lessons Learned

See [LESSONS.md](LESSONS.md) for development notes, debugging war stories, and context
behind architectural decisions.  These are personal notes — not project rules or
requirements for contributors.
