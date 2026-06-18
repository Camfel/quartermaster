# Quartermaster Design Document

## 1. Vision
**Quartermaster** is a lightweight, GitOps-native, single-node container orchestrator designed for homelab environments. Unlike heavy-weight orchestrators like Kubernetes, Quartermaster is optimized for a single Debian host, managing high-performance workloads like game servers (Satisfactory, Enshrouded), local LLMs (llama-cpp), and media stacks (Jellyfin/Arr suite) with minimal overhead.

## 2. Core Philosophy
- **GitOps-First:** The Git repository is the single source of truth. The state is reconciled by observing the diff between the Git manifest and the actual state of the host.
- **Least Privilege:** Security is paramount. The orchestrator runs with specific Linux capabilities rather than full root, and workloads are encouraged to run as non-privileged users with read-only secrets.
- **Simplicity for the User:** A "one-liner" installation experience via `apt`, providing a seamless CLI (`qm`) interface.

## 3. Architecture

### 3.1 The Control Loop (Reconciliation)
Quartermaster operates on a continuous reconciliation loop:
1. **Observe:** Poll the configured Git repository and query `containerd` via gRPC (CRI) for the current state.
2. **Diff:** Compare the *Desired State* (Git YAML) against the *Actual State* (Running Containers).
3. **Act:** Execute the minimal set of actions (`Create`, `Update`, `Delete`, `No-op`) to align reality with the manifest.

### 3.2 Components
- **`qm` (CLI):** A Cobra-based command-line tool for user interaction (e.g., `qm status`, `qm enable vpn`).
- **`qm-daemon` (The Brain):** A background systemd service that runs the reconciliation loop.
- **CRI Client:** A gRPC-based module that communicates directly with `containerd` to manage container lifecycles.
- **Config Manager:** A validator that ensures all incoming YAML manifests are structurally and logically sound before application.
- **Secret Manager:** Handles the injection of sensitive data from `/etc/quartermaster/secrets/` into containers as read-only files.
- **Network Manager:** Manages network profiles using a Linux bridge (`qm0`), IPAM, DNAT port forwarding, and policy routing for VPN egress.

## 4. Data Model (The Manifest)

The `quartermaster.yaml` defines the desired state. Key features include:
- **Service Definitions:** Image, ports, volumes, and environment variables.
- **Resource Constraints:** Explicit GPU (NVIDIA) and hardware requirements.
- **Dependencies:** `depends_on` logic to ensure services start in the correct order.
- **Identity:** `user` mapping (UID/GID) for filesystem and process security.
- **Health Checks:** Liveness and Readiness probes (HTTP, TCP, Exec).
- **Network Profiles:** Ability to route traffic through specific profiles (e.g., `vpn-gateway`).

## 5. Security Model

### 5.1 Host Security (The Daemon)
- **Dedicated User:** The daemon runs as a dedicated `quartermaster` system user.
- **Capabilities over Root:** Instead of full root, the daemon is granted specific Linux capabilities via systemd:
    - `CAP_NET_ADMIN` (Networking)
    - `CAP_SYS_ADMIN` (Mounts/Namespaces)
    - `CAP_DAC_OVERRIDE` (File access)

### 5.2 Workload Security (The Containers)
- **Non-Root by Default:** Users are encouraged to define `user: "uid:gid"` in manifests.
- **Immutable Secrets:** Secrets are never injected as environment variables (to prevent leakage via `ps`); they are mounted as read-only files in a `tmpfs` volume.
- **Rollback Protection:** If a new configuration results in a failed health check or a broken state, the daemon automatically reverts to the **Last Known Good (LKG)** state.

## 6. Roadmap

### Phase 1: The Local-Static Engine (MVP)
- Implement the CRI gRPC client for `containerd`.
- Implement the core reconciliation loop using local YAML files.
- Basic support for image pulling, starting, and stopping containers.

### Phase 2: The GitOps Layer
- Integrate Git polling and the "Watcher" logic.
- Implement the Validation engine to prevent "Broken Manifest" deployments.
- Implement the LKG (Last Known Good) rollback mechanism.

### Phase 3: Hardware & Secrets
- Implement GPU detection and OCI spec injection for NVIDIA.
- Implement the Secret Manager for secure file injection.

### Phase 4: Advanced Networking
- Linux bridge (`qm0`) with veth pairs and IPAM
- DNAT port forwarding for non-host containers
- VPN policy routing: `ip rule from <ip> table 100` for egress through gateway
- Gateway configuration: FORWARD + MASQUERADE rules
- Stale gateway detection and auto-recreation of dependents
- Namespace-safe netlink operations (NewHandleAt)

## 7. Failure Modes & Mitigations
| Failure | Mitigation |
| :--- | :--- |
| Typo in YAML | `ConfigManager` catches error during validation; no changes applied. |
| Bad Image Tag | Reconciler detects failure to pull; triggers LKG rollback. |
| Container Crash Loop | Health checks detect failure; Reconciler attempts restart/rollback. |
| Unauthorized Access | Daemon runs with minimal capabilities; Secrets are read-only. |
| Git Repo Down | Daemon continues running last known successful configuration. |
