# CRI Design Specification

## 🎯 Purpose
The Container Runtime Interface (CRI) abstraction in Quartermaster serves as a decoupling layer between the high-level reconciliation logic (`pkg/reconciler`) and the low-level container runtime implementation (currently `containerd`).

The goal is to ensure the "Brain" of Quartermaster (the Reconciler) doesn't need to know *how* a container is started, only *that* it can be started, stopped, or deleted.

## 🏗️ Design Goals

### 1. Abstraction & Decoupling
The Reconciler should operate on high-level intent (e.g., "Ensure this service is running"). It should not be burdened with the intricacies of gRPC calls, namespace management, or specific runtime quirks.

### 2. Testability (Mockability)
By using an interface, we can inject a `MockContainerClient` into the Reconciler during unit tests. This allows us to:
- Simulate runtime failures (e.g., "image pull failed").
- Simulate slow shutdowns.
- Verify the Reconciler's logic without requiring a real `containerd` socket.

### 3. Future-Proofing
While we are currently committed to `containerd` for its performance and feature set in homelab environments, the interface allows us to support other runtimes (like `CRI-O` or even a specialized lightweight runner) in the future by simply implementing a new `ContainerClient`.

### 4. Idempotency & Resilience
The CRI implementation must handle the "messiness" of the real world. This includes:
- **Graceful Shutdowns:** Implementing a "SIGTERM $\rightarrow$ Wait $\rightarrow$ SIGKILL" pattern to ensure containers exit cleanly.
- **State Awareness:** Managing the transition between container states (Created $\rightarrow$ Running $\rightarrow$ Stopped $\rightarrow$ Deleted) to avoid "resource in use" errors.

## 🛠️ The Interface Definition

The core of this design is the `ContainerClient` interface located in `pkg/cri/interface.go`.

```go
type ContainerClient interface {
    // PullImage downloads an image from a registry.
    PullImage(ctx context.Context, ref string) (string, error)

    // CreateContainer sets up the container and its filesystem (snapshotter).
    CreateContainer(ctx context.Context, svc types.Service) (string, error)

    // StartContainer starts the task associated with a container.
    StartContainer(ctx context.Context, containerID string) error

    // StopContainer stops the running task.
    StopContainer(ctx context.Context, containerID string) error

    // DeleteContainer removes the container and its resources.
    DeleteContainer(ctx context.Context, containerID string) error

    // ListContainers returns a list of currently running containers.
    ListContainers(ctx context.Context) ([]ContainerInfo, error)
}
```

## 🔄 Lifecycle Management Strategy

To maintain a stable system, the CRI implementation follows a strict lifecycle for every service:

| Phase | Operation | Implementation Detail |
| :--- | :--- | :--- |
| **Provisioning** | `PullImage` | Ensure the image exists locally before attempting creation. |
| **Creation** | `CreateContainer` | Create the container object, set up OCI spec, and prepare the snapshotter. |
| **Activation** | `StartContainer` | Create and start the execution task. |
| **Deactivation** | `StopContainer` | **Crucial:** Send `SIGTERM` $\rightarrow$ Wait $\rightarrow$ (Optional) `SIGKILL` $\rightarrow$ Delete Task. |
| **Cleanup** | `DeleteContainer` | Remove the container object and clean up snapshots. |
| **Observation** | `ListContainers` | Query the runtime to build the "Actual State" map. |

## ⚠️ Known Challenges & Mitigation

### The "Zombie Task" Problem
**Problem:** Attempting to delete a container while its task is still technically in a `STOPPED` but not yet `DELETED` state in the runtime.
**Mitigation:** The `StopContainer` implementation must include a robust wait-and-retry mechanism to ensure the task is fully cleared from the runtime's process table before returning.

### Namespace Isolation
**Problem:** Preventing host processes from interfering with managed containers.
**Mitigation:** All CRI operations must be scoped to a specific, dedicated `containerd` namespace (e.g., `quartermaster`) to ensure logical isolation from the rest of the system.
