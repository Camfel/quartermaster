# Lessons Learned

> Personal development notes and hard-earned lessons from building Quartermaster.
> These are not rules or requirements for contributors — they're context for
> understanding why certain decisions were made. Every project and developer
> works differently.

---

## June 2026 Session

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
    Table 100 routes ALL traffic through the VPN gateway except bridge-local (`10.42.0.0/24`).  LAN traffic (`192.168.0.0/24`) also needs a direct route so VPN containers can reach host-networked services.

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

## June 2026 Session — Network v2

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
    Host-networked services don't have bridge IPs.  When a bridge container queries the DNS forwarder for a host-networked service name, return `10.42.0.1` (bridge GW → host) rather than forwarding to external DNS which doesn't know about internal names.

51. **`config.yaml` mounts override all defaults in Docker images.**
    Mounting a partial `config.yaml` into CrowdSec broke its config paths.  Docker entrypoints merge configs — overriding the entire file removes defaults.  Mount only the specific files you need to override (e.g., `profiles.yaml`, `acquis.yaml`).

52. **`Command` field must be in the config hash.**
    The `serviceConfigHash` function determines when a container needs recreation.  Without `Command` in the hash, changes to startup wrappers (sed, env substitution, health check loops) are silently ignored.  Add every mutable field to the hash.

53. **Rotating log directory must use service name, not container ID.**
    Container IDs change on every recreate.  Using the ID as the log directory path meant logs were scattered across UUID-named directories.  Store a `containerID → serviceName` mapping and use the service name for the log path.

54. **`go build` cache can mask stale binaries during deploy.**
    When `go build` says "0 units compiled", the binary may be cached.  Use `rm -f bin/qm-daemon && go build` to force a fresh compilation.  `make restart` also uses cached builds per rule #36.

## June 2026 Session — Services & Fixes

55. **`http.extraHeader` doesn't work for git auth.**
    `git -c http.extraHeader=Authorization: Bearer <token>` silently returns "invalid credentials" (exit 128) even with a valid token.  Git ignores `http.extraHeader` for the Authorization header.  **Fix:** use URL-embedded auth: `https://x-access-token:TOKEN@github.com/...` in the fetch URL.  When fetching from a URL instead of a remote name, use an explicit refspec (`+refs/heads/main:refs/remotes/origin/main`) to update the remote tracking ref.

56. **cgroups v2 `memory.Usage` includes page cache — use `memory.Anon` for RSS.**
    Containers that read large files (e.g., 34 GB MKV during import) show inflated "memory" because `v2.Memory.Usage` includes the file page cache.  The kernel reclaims page cache instantly under memory pressure — it's not real usage.  **Fix:** use `v2.Memory.Anon` for the `qm_container_memory_bytes` metric.  The `Anon` field is the anonymous memory (actual RSS) without file-backed pages.

57. **Use bridge gateway IPs (10.42.0.1) not hostnames for inter-service app config.**
    Host-networked containers use the bridge DNS forwarder (`10.42.0.1:53`) which correctly resolves services to `10.42.0.1`.  But VPN-networked containers inherit the host's `/etc/resolv.conf` (pointing to the LAN router), which doesn't know about internal hostnames.  **Fix:** configure all inter-service URLs with `10.42.0.1` instead of hostnames.  This works from every network context.

58. **BuildKit + containerd worker for image building — no Docker needed.**
    Download buildkit release tarball, start `buildkitd --containerd-worker=true --containerd-worker-namespace=quartermaster`, build with `buildctl`.  Images appear directly in containerd's local store.  To make the daemon use a locally-built image without attempting a registry pull, the image must exist in containerd before `StartContainer`.

59. **Caddy Let's Encrypt needs port 80 reachable from the internet.**
    The HTTP-01 challenge requires inbound port 80.  If the router forwards 80/443 and DNS resolves to the public IP, certs are obtained automatically (Caddy handles renewal).  For local-only services, bypass Caddy (`http://<host>:PORT`) or use `tls internal`.  Caddy redirects HTTP→HTTPS; if no valid cert exists, browsers hang on the redirect.

60. **Steam game servers need 5-10 minutes for initial PlayFab/Steamworks auth.**
    Valheim (and other Steam game servers) retry authentication 20+ times before binding the game port.  During this time, the TCP healthcheck fails and the daemon restarts the container — resetting the auth cycle.  **Fix:** disable the healthcheck on first start, or set the interval ≥ 600s.  Re-enable once the server is stable.

61. **Some Docker images have PID file race conditions on tmpfs `/var/run`.**
    `lloesche/valheim-server` creates `/var/run/valheim` in the Dockerfile but loses it when `/var/run` is a tmpfs (common with containerd).  The `check_lock()` function fails because the pidfile directory doesn't exist, causing an infinite supervisord restart loop.  **Fix:** add `mkdir -p /var/run/valheim` at runtime in the bootstrap script.
