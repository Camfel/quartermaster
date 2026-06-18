// Package cri provides the container runtime interface for containerd.
// ContainerClient defines the abstraction; ContainerdClient implements it via
// containerd's gRPC CRI API.  A MockContainerClient is provided for unit tests.
package cri

import (
	"context"
)

import (
	"quartermaster/pkg/types"
)

// ContainerInfo represents the current state of a running container as seen by the runtime.
type ContainerInfo struct {
	ID         string
	Name       string
	Image      string
	ConfigHash string // SHA256 hash of the service spec, used for change detection
	Running    bool   // true if the container's task is running
	PID        uint32 // task PID, or 0 if not running
	GatewayIP  string // VPN gateway IP this container was created with, or empty
}

// ContainerStats holds per-container resource-usage metrics retrieved from
// the container runtime (CPU and memory only — no disk or network).
type ContainerStats struct {
	CPUUsageSeconds  float64
	MemoryUsageBytes uint64
	MemoryLimitBytes uint64
}

// ContainerClient defines the interface for interacting with a container runtime.
// This abstraction allows us to mock the runtime for unit testing the reconciler.
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

	// GetContainerPID returns the PID of the container's main task, or 0 if not running.
	GetContainerPID(ctx context.Context, containerID string) (uint32, error)

	// ContainerLogs returns the trailing logs for a running container.
	// tail is the number of lines to fetch (e.g. "100").  Use "all" for the full log.
	ContainerLogs(ctx context.Context, containerID string, tail string) (string, error)

	// ContainerStats returns CPU and memory usage for a running container.
	ContainerStats(ctx context.Context, containerID string) (*ContainerStats, error)
}
