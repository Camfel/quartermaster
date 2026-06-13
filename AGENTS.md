# Quartermaster Agent Instructions

> Rules, patterns, and lessons learned. Follow these when contributing to this codebase.

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

## Hard-Earned Lessons (June 2026 Session)

30. **Every HTTP client must have a timeout.**  
    A missing timeout on `http.Client` blocked the entire reconciliation loop for minutes.  Use `&http.Client{Timeout: 5 * time.Second}` everywhere.

31. **Bind-mount directories, not files.**  
    File bind mounts go stale when the source is recreated (daemon restarts recreate the Unix socket).  Mount the parent directory instead so the container sees the new file.

32. **GitOps repos: components + config + secrets.**  
    Components repo = generic, reusable manifests.  Config repo = user-specific ConfigMaps and config files.  Secrets = `qm create-secret` only, never in git.  Three repos, three trust levels.

33. **Container startup wrappers need `command:` field.**  
    When a container needs to generate its config from ConfigMaps + secrets at startup (e.g., authelia, ddclient), use a `command:` override with `sed`.  This field must be supported in the `Service` type and wired into the OCI spec.

34. **DNS for host-networked containers.**  
    Host-networked containers use the host's `/etc/resolv.conf`, which may be missing or use Tailscale DNS that some containers can't resolve.  Always bind-mount a resolv.conf.

35. **Disable build output filtering when debugging.**  
    Build tools that mask output hide the actual `go build` errors.  When builds fail inexplicably, disable all filtering and run `go build` directly.

36. **`make restart` uses cached builds.**  
    When deploying urgent fixes, use `go build -o bin/qm-daemon && systemctl restart qm-daemon` to ensure the latest code is running, not a cached artifact.

37. **GPU passthrough needs nvidia-container-toolkit.**  
    Just adding `resources.gpu` to the manifest isn't enough — containerd needs the NVIDIA runtime configured.  Install `nvidia-container-toolkit`, run `nvidia-ctk runtime configure --runtime=containerd`, restart containerd.

38. **VPN policy routing must bypass LAN subnet.**  
    Table 100 routes ALL traffic through the VPN gateway except bridge-local (`10.42.0.0/24`).  LAN traffic (`192.168.0.0/24`) also needs a direct route so VPN containers can reach host-networked services like Sonarr/Radarr.

39. **DNAT PREROUTING doesn't apply to localhost traffic.**  
    Traffic from the host to itself goes through the loopback interface, bypassing PREROUTING.  For host-local access to DNAT'd ports, add OUTPUT chain rules.  External traffic (LAN, Tailscale) hits PREROUTING correctly.

40. **Service renames delete containers — config doesn't migrate.**  
    Renaming a service (e.g. `overseerr` → `jellyseerr`) causes the old container to be deleted and a new one created.  Config directories need to be manually migrated or the service starts fresh.

41. **Not every Docker image has resolv.conf.**  
    Host-networked containers use the host's `/etc/resolv.conf` which may not exist inside the container's filesystem.  Always check and bind-mount one if needed (caddy, ddclient, jellyseerr all hit this).

42. **Seerr/Ombi setup pages have frontend validation bugs.**  
    The "Continue" button can be greyed out even when the form is valid.  Use the browser console to POST directly to the API endpoint (`/api/v1/settings/initialize`) as a workaround.

43. **Prefer Usenet over torrents for homelab automation.**  
    Usenet (SABnzbd) is faster, doesn't need seeding, doesn't require port forwarding, and is DMCA-safe.  Route it through VPN for privacy.  Prowlarr connects to both torrent and Usenet indexers.

## Hard-Earned Lessons (June 2026 Session — Network v2)

44. **fwmark doesn't survive bridge forwarding on this kernel.**  
    The `iptables -t mangle -A OUTPUT -j MARK --set-mark 100` approach sets the nfmark in the container namespace, but the mark is lost when the packet crosses the veth into the host bridge.  Use **source-based policy routing** instead: `ip rule add from <container-ip> table 100` inside each container's namespace.  Add direct routes in table 100 for bridge (`10.42.0.0/24`), LAN (`192.168.0.0/24`), and Tailscale (`100.64.0.0/10`) subnets so responses to the host aren't tunnelled through the VPN.

45. **`serviceProfiles` is ephemeral — lost on daemon restart.**  
    After a restart, `runDeleteFlow` defaults to `profile="public"`, and `Detach` returned early without cleaning DNAT rules, veth pairs, or netns.  **Fix:** `Detach` must always attempt cleanup regardless of profile — check for resources defensively.  Also call `cleanStaleDNAT()` on startup to purge rules pointing to IPs not in `bridge-ips.json`.

46. **`ruleToDeleteArgs` was silently skipping protocol values.**  
    The switch case listed `"tcp"` and `"udp"` alongside flag names like `"-A"`, causing protocol values after `-p` to be dropped.  Delete args were missing the protocol value, so `iptables -D` silently failed.  Stale DNAT rules accumulated forever.  **Fix:** only skip actual flag names (`-A`, `PREROUTING`, `OUTPUT`, `-m`), never protocol values.

47. **DNAT first-match wins — port conflicts are silent.**  
    When two services use the same host port (e.g., qBittorrent and SABnzbd both on 8080), the first DNAT rule wins and the other service is unreachable.  No error is logged.  **Fix:** `ExposePorts` must delete stale rules for the same port before adding new ones.  `cleanStaleDNAT` catches remaining orphans on startup.

48. **Shared `/etc/hosts` breaks host-networked services from bridge containers.**  
    The hosts file mapped host-networked services to `127.0.0.1`, which is the bridge container's own loopback, not the host.  **Fix:** use `10.42.0.1` (bridge gateway) for host-networked services in the hosts file.  Both host and bridge containers can reach the host via that IP.

49. **VPN tunnels need TCP MSS clamping.**  
    ProtonVPN OpenVPN uses MTU ~1394.  Without MSS clamping, TCP handshakes complete but data transfer stalls (SYN-ACK makes it back, ACK+GET never do).  **Fix:** add `iptables -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1350` in gluetun's namespace via `ConfigureVPNGateway`.

50. **DNS forwarder should resolve unknown hostnames to bridge gateway.**  
    Host-networked services (sonarr, radarr, jellyfin, jellyseerr) don't have bridge IPs.  When a bridge container queries the DNS forwarder for `radarr`, it should return `10.42.0.1` (bridge GW → host) rather than forwarding to external DNS which doesn't know about internal names.

51. **`config.yaml` mounts override all defaults in Docker images.**  
    Mounting a partial `config.yaml` into CrowdSec broke its config paths.  Docker entrypoints merge configs — overriding the entire file removes defaults.  Mount only the specific files you need to override (e.g., `profiles.yaml`, `acquis.yaml`).

52. **`Command` field must be in the config hash.**  
    The `serviceConfigHash` function determines when a container needs recreation.  Without `Command` in the hash, changes to startup wrappers (sed, env substitution, health check loops) are silently ignored.  Add every mutable field to the hash.

53. **Rotating log directory must use service name, not container ID.**  
    Container IDs change on every recreate.  Using the ID as the log directory path meant logs were scattered across UUID-named directories.  Store a `containerID → serviceName` mapping and use the service name for the log path.

54. **`go build` cache can mask stale binaries during deploy.**  
    When `go build` says "0 units compiled", the binary may be cached.  Use `rm -f bin/qm-daemon && go build` to force a fresh compilation.  `make restart` also uses cached builds per rule #36.
