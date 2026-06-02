# Quartermaster - Agent Context & Instruction Manifest

## 🎯 Project Vision
**Quartermaster** is a lightweight, GitOps-native, single-node container orchestrator designed for homelab environments (Debian-focused). It reconciles a Git-based manifest against a `containerd` runtime.

**Core Philosophy:**
- **GitOps-First:** Git is the single source of truth.
- **Least Privilege:** Minimal Linux capabilities; non-root workloads.
- **Simplicity:** One-liner installation; CLI-driven (`qm`).

---

## 🚦 Current State
**Phase 1 (Core Engine) is COMPLETE.**
The system can parse configs, manage a mock runtime, and run a reconciliation loop that creates/deletes services.

**Current Focus: Phase 2 (The GitOps Layer)**

---

## 🛠️ Immediate Mission: Phase 2 Implementation
Your goal is to transition the engine from "Local Static" (reading files from disk) to "GitOps" (reading files from a remote repo).

### 📋 Task List (In Order of Priority)

#### 1. Implement the Git Watcher/Poller
- **Goal:** Automatically detect changes in a remote Git repository.
- **Requirement:** Implement a background routine that polls a configured Git repo.
- **Technical Hint:** Use `github.com/go-git/go-git/v5`.
- **Logic:** When a new commit hash is detected, trigger the `Reconciler.Reconcile()` method with the new content.

#### 2. Implement the Last Known Good (LKG) Rollback
- **Goal:** Protect the host from "broken" configurations.
- **Requirement:** The `Daemon` must keep a record of the last *successfully applied* manifest.
- **Logic:** If a reconciliation pass fails OR if new containers fail their `HealthCheck` within a certain grace period, automatically revert the desired state to the LKG manifest and trigger a reconciliation pass.

#### 3. Strengthen Manifest Validation
- **Goal:** Prevent bad YAML from even reaching the Reconciler.
- **Requirement:** Enhance `pkg/config` to catch semantic errors (e.g., invalid image formats, port collisions, missing required fields) during the polling phase.

---

## 📂 Key Technical Context
| Component | Location | Responsibility |
| :--- | :--- | :--- |
| **The Brain** | `pkg/reconciler/` | The core reconciliation loop (Observe $\rightarrow$ Diff $\rightarrow$ Act). |
| **The Interface** | `pkg/cri/` | The abstraction layer for `containerd`. Includes `MockContainerClient`. |
| **The Data Model** | `pkg/types/` | The `Stack` and `Service` Go structs. |
| **The Config** | `pkg/config/` | Parsing and validating YAML. |
| **The Runtime** | `cmd/qm-daemon/` | The background systemd service that hosts the loop. |

---

## ⚠️ Constraints & Rules
1. **Do Not Break the CRI Abstraction:** Never leak `containerd` specific logic into the `Reconciler`. Use the `ContainerClient` interface.
2. **Idempotency is King:** Every action must be idempotent. Running the same reconciliation twice should result in no changes if the state is already correct.
3. **Fail Gracefully:** A failure in one service should not stop the entire reconciliation loop for other services.
4. **Maintain Tests:** Every new feature must include corresponding unit tests in its respective package.

---

## 📖 Reference Documents
- `DESIGN.md`: Architectural blueprint.
- `PROGRESS.md`: Milestone tracking.
- `CRI-DESIGN.md`: Specification for the runtime interface.
