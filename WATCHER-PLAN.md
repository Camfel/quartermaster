# Technical Specification: The Quartermaster Watcher

## 🎯 Objective
The **Watcher** is the component responsible for transforming Quartermaster from a periodic, polling-based sync loop into a reactive, event-driven **GitOps engine**. 

Its primary purpose is to monitor the "Source of Truth" (the Git repository) and trigger the `Reconciler` only when a change is detected, ensuring high reactivity while minimizing unnecessary CPU and network overhead.

## 🏗️ Architecture & Integration
The Watcher will reside within the `internal/daemon` package (or a new `pkg/watcher` package) and will act as the "trigger" for the `Reconciler`.

### Data Flow
1. **Watcher** $\rightarrow$ (Detects Change) $\rightarrow$ **Signal/Event** $\rightarrow$ **Daemon** $\rightarrow$ **Reconciler** $\rightarrow$ **Container Runtime**.

### Design Constraints
- **Decoupling:** The Watcher should not know *how* to reconcile; it should only know *when* to signal that a reconciliation is required.
- **Idempotency:** The Watcher must ensure that even if multiple changes happen in rapid succession, the Reconciler is not overwhelmed (Debouncing/Cooldown).
- **Resilience:** If Git is unreachable, the Watcher must fail silently and allow the system to continue running the "Last Known Good" state.

## ✅ Functional Requirements

### 1. Git Monitoring (Polling-based)
Since we are targeting homelab environments where webhooks are often difficult to configure (due to NAT/Firewalls), the primary mechanism will be a smart polling loop.
- Periodically perform a `git fetch`.
- Compare the current `HEAD` with the previously recorded `HEAD`.
- If `HEAD` has changed, trigger a reconciliation.

### 2. Change Detection Logic
- The Watcher must detect not just new commits, but any change to the `stack.yaml` files within the tracked directory.
- It should be able to distinguish between a "shallow" change (e.g., a README update) and a "meaningful" change (e.g., a change to a service spec) if possible, though a full reconciliation on any commit is an acceptable baseline.

### 3. Debouncing & Cooldown (The "Anti-Storm" Mechanism)
To prevent "Reconciliation Storms" (e.g., a user pushing 10 commits in 5 seconds), the Watcher must implement a cooldown period.
- **Cooldown:** Once a reconciliation is triggered, the Watcher must wait for a configurable duration (e.g., 30 seconds) before allowing another trigger, even if more changes are detected.

## 🛠️ Implementation Roadmap

### Step 1: Interface Definition
Define the `Watcher` interface.
```go
type Watcher interface {
    // Start begins the watching loop. It takes a channel to send signals to.
    Start(ctx context.Context, signalChan chan<- struct{}) error
}
```

### Step 2: Git Implementation
Implement `GitWatcher` using a library like `go-git` or by executing `git` commands via `os/exec`.
- Implement the logic to track the last seen Commit Hash.
- Implement the periodic polling loop.

### Step 3: Daemon Integration
Integrate the Watcher into `cmd/qm-daemon`.
- The Daemon should initialize the Watcher.
- The Daemon should listen on a channel for signals from the Watcher.
- When a signal is received, the Daemon triggers `Reconciler.Reconcile()`.

### Step 4: Configuration
Add Watcher settings to the global configuration:
- `watch_interval`: How often to poll Git (e.g., `5m`).
- `watch_cooldown`: The minimum time between reconciliations (e.g., `30s`).

## ⚠️ Edge Cases & Error Handling

| Scenario | Expected Behavior |
| :--- | :--- |
| **Git Repository Unreachable** | Log a warning. Do **not** trigger reconciliation. Do **not** crash the daemon. |
| **Malformed YAML in New Commit** | The Reconciler will catch the error. The Watcher should log the failure and wait for the next valid commit. |
| **Rapid-fire Commits** | The Cooldown mechanism must ensure only one reconciliation occurs for the entire batch. |
| **Network Jitter** | The Watcher must handle transient network errors gracefully during `git fetch`. |
