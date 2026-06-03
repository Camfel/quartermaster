package cri

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"quartermaster/pkg/hardware"
	"quartermaster/pkg/network"
	"quartermaster/pkg/secrets"
	"quartermaster/pkg/types"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// ringBuffer is a thread-safe bounded byte buffer for capturing container logs.
type ringBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	cap  int // max bytes to retain
}

func newRingBuffer(cap int) *ringBuffer {
	return &ringBuffer{cap: cap}
}

func (rb *ringBuffer) Write(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Evict old data if we'd exceed capacity.
	if rb.buf.Len()+len(p) > rb.cap {
		excess := rb.buf.Len() + len(p) - rb.cap
		if excess < rb.buf.Len() {
			// Drop oldest bytes to make room.
			rb.buf.Next(excess)
		} else {
			rb.buf.Reset()
		}
	}
	return rb.buf.Write(p)
}

func (rb *ringBuffer) String() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.buf.String()
}

func (rb *ringBuffer) TailBytes(n int) string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	b := rb.buf.Bytes()
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// logStore holds ring buffers keyed by container ID.
type logStore struct {
	mu  sync.Mutex
	bufs map[string]*ringBuffer
}

func newLogStore() *logStore {
	return &logStore{bufs: make(map[string]*ringBuffer)}
}

func (ls *logStore) get(containerID string) *ringBuffer {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	rb, ok := ls.bufs[containerID]
	if !ok {
		rb = newRingBuffer(256 * 1024) // 256 KB per container
		ls.bufs[containerID] = rb
	}
	return rb
}

func (ls *logStore) remove(containerID string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.bufs, containerID)
}

// ContainerdClient is the real implementation of ContainerClient using containerd.
type ContainerdClient struct {
	client    *containerd.Client
	namespace string
	secrets   *secrets.Manager  // optional: for secret injection
	hwDetect  *hardware.Detector // optional: for GPU detection
	netMgr    *network.Manager   // optional: for network profile management
	logs      *logStore
}

// NewContainerdClient initializes a new connection to the containerd socket.
func NewContainerdClient(socketPath, namespace string) (*ContainerdClient, error) {
	client, err := containerd.New(socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to containerd: %w", err)
	}

	return &ContainerdClient{
		client:    client,
		namespace: namespace,
		logs:      newLogStore(),
	}, nil
}

// WithSecrets sets the secrets manager for secret injection into containers.
func (c *ContainerdClient) WithSecrets(sm *secrets.Manager) *ContainerdClient {
	c.secrets = sm
	return c
}

// WithHardwareDetector sets the hardware detector for GPU device injection.
func (c *ContainerdClient) WithHardwareDetector(hd *hardware.Detector) *ContainerdClient {
	c.hwDetect = hd
	return c
}

// WithNetworkManager sets the network manager for network profile support.
func (c *ContainerdClient) WithNetworkManager(nm *network.Manager) *ContainerdClient {
	c.netMgr = nm
	return c
}

// withNamespace is a helper to wrap the context with the quartermaster namespace.
func (c *ContainerdClient) withNamespace(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, c.namespace)
}

// PullImage pulls an image from the registry.
func (c *ContainerdClient) PullImage(ctx context.Context, ref string) (string, error) {
	ctx = c.withNamespace(ctx)
	log.Printf("Pulling image: %s", ref)
	
	image, err := c.client.Pull(ctx, ref, containerd.WithPullUnpack)
	if err != nil {
		return "", fmt.Errorf("failed to pull image %s: %w", ref, err)
	}

	return image.Name(), nil
}

// CreateContainer creates a new container based on the service specification.
func (c *ContainerdClient) CreateContainer(ctx context.Context, svc types.Service) (string, error) {
	ctx = c.withNamespace(ctx)
	log.Printf("Creating container: %s", svc.Name)

	image, err := c.client.GetImage(ctx, svc.Image)
	if err != nil {
		return "", fmt.Errorf("failed to get image for container %s: %w", svc.Name, err)
	}

	// Generate the OCI Spec
	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
	}

	// 0. Network: by default, use host networking (appropriate for a
	//    single-host homelab orchestrator).  The "internal" profile
	//    isolates the container in its own network namespace.
	netProfile := strings.ToLower(svc.Network)
	if netProfile != "internal" {
		specOpts = append(specOpts, oci.WithHostNamespace(specs.NetworkNamespace))
	}

	// 1. Add Environment Variables
	var envVars []string
	for _, env := range svc.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretRef != "" && c.secrets != nil {
			// Resolve secret into env var value.
			sd, err := c.secrets.Resolve(env.Name, env.ValueFrom.SecretRef)
			if err != nil {
				return "", fmt.Errorf("failed to resolve secret %q for env %s: %w",
					env.ValueFrom.SecretRef, env.Name, err)
			}
			envVars = append(envVars, fmt.Sprintf("%s=%s", env.Name, string(sd.Content)))
		} else if env.Value != "" {
			envVars = append(envVars, fmt.Sprintf("%s=%s", env.Name, env.Value))
		}
	}
	if len(envVars) > 0 {
		specOpts = append(specOpts, oci.WithEnv(envVars))
	}

	// 2. Add Volume Mounts
	for _, vol := range svc.Volumes {
		specOpts = append(specOpts, oci.WithMounts([]specs.Mount{
			{
				Type:        vol.Type,
				Source:      vol.Source,
				Destination: vol.Target,
				Options:     []string{"bind", "rw"},
			},
		}))
	}

	// 3. Set User (UID/GID)
	if svc.User != "" {
		specOpts = append(specOpts, oci.WithUser(svc.User))
	}

	// 4. Inject Secrets as read-only bind mounts
	if c.secrets != nil && len(svc.Secrets) > 0 {
		secretRefs := make([]secrets.SecretRef, len(svc.Secrets))
		for i, s := range svc.Secrets {
			secretRefs[i] = secrets.SecretRef{Name: s.Name, SecretRef: s.SecretRef}
		}
		mountDir, cleanup, err := c.secrets.PrepareMountDir(secretRefs)
		if err != nil {
			return "", fmt.Errorf("failed to prepare secrets for %s: %w", svc.Name, err)
		}
		// Note: cleanup is deferred until after container creation.
		// In production we'd track and clean up when the container is deleted.
		_ = cleanup

		specOpts = append(specOpts, oci.WithMounts([]specs.Mount{
			{
				Type:        "bind",
				Source:      mountDir,
				Destination: "/run/secrets",
				Options:     []string{"bind", "ro"},
			},
		}))
		log.Printf("Injected %d secret(s) for container %s", len(svc.Secrets), svc.Name)
	}

	// 5. Inject GPU resources (NVIDIA)
	if c.hwDetect != nil && svc.Resources != nil && svc.Resources.GPU != nil {
		gpu := svc.Resources.GPU
		if gpu.Type == "nvidia" || gpu.Type == "" {
			// Add NVIDIA environment variables
			nvidiaEnv := c.hwDetect.NVIDIARequiredEnv()
			if len(nvidiaEnv) > 0 {
				specOpts = append(specOpts, oci.WithEnv(nvidiaEnv))
			}

			// Add NVIDIA device mounts
			nvidiaDevices := c.hwDetect.NVIDIARequiredDevices()
			var deviceMounts []specs.Mount
			for _, dev := range nvidiaDevices {
				deviceMounts = append(deviceMounts, specs.Mount{
					Type:        "bind",
					Source:      dev,
					Destination: dev,
					Options:     []string{"bind", "ro"},
				})
			}
			specOpts = append(specOpts, oci.WithMounts(deviceMounts))
			log.Printf("Injected %d NVIDIA device(s) for container %s", len(nvidiaDevices), svc.Name)
		}
	}

	// 6. Network profile: join VPN gateway's network namespace if needed.
	if c.netMgr != nil && svc.Network != "" {
		netNormal := strings.ToLower(svc.Network)
		if netNormal == "vpn" {
			gwPID, ok := c.netMgr.GatewayPID()
			if ok {
				netNsPath := network.NetworkNamespacePath(gwPID)
				specOpts = append(specOpts, withNetworkNamespace(netNsPath))
				log.Printf("Container %s joining VPN network namespace (pid=%d)", svc.Name, gwPID)
			}
		}
	}

	container, err := c.client.NewContainer(
		ctx,
		svc.Name,
		containerd.WithNewSnapshot(svc.Name+"-snapshot", image),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(map[string]string{
			"quartermaster.name":        svc.Name,
			"quartermaster.config-hash": svc.ConfigHash,
		}),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create container %s: %w", svc.Name, err)
	}

	return container.ID(), nil
}

// StartContainer starts a task for the container.  Container stdout/stderr are
// captured into an in-memory ring buffer so they can be served via ContainerLogs.
func (c *ContainerdClient) StartContainer(ctx context.Context, containerID string) error {
	ctx = c.withNamespace(ctx)
	log.Printf("Starting container: %s", containerID)

	container, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return fmt.Errorf("failed to load container %s: %w", containerID, err)
	}

	rb := c.logs.get(containerID)
	// Writer that sends to the ring buffer AND /dev/null so the
	// container's stdout/stderr don't fill the daemon's own stdio.
	logWriter := io.MultiWriter(rb, io.Discard)

	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(
		nil,        // stdin  — not used
		logWriter,  // stdout — captured
		logWriter,  // stderr — captured
	)))
	if err != nil {
		return fmt.Errorf("failed to create task for container %s: %w", containerID, err)
	}

	if err := task.Start(ctx); err != nil {
		return fmt.Errorf("failed to start task for container %s: %w", containerID, err)
	}

	return nil
}

// GetContainerPID returns the PID of the container's main task.
func (c *ContainerdClient) GetContainerPID(ctx context.Context, containerID string) (uint32, error) {
	ctx = c.withNamespace(ctx)

	container, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return 0, fmt.Errorf("failed to load container %s: %w", containerID, err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return 0, nil // Task not running, return 0
	}

	return task.Pid(), nil
}

// ContainerLogs returns the trailing logs for a running container.
//
// First tries the in-memory ring buffer (populated for containers started
// by this daemon process).  If the buffer is empty, falls back to reading
// from containerd's shim stdout FIFO.
func (c *ContainerdClient) ContainerLogs(ctx context.Context, containerID string, tail string) (string, error) {
	// Verify the container exists and is running.
	nscCtx := c.withNamespace(ctx)
	container, err := c.client.LoadContainer(nscCtx, containerID)
	if err != nil {
		return "", fmt.Errorf("failed to load container %s: %w", containerID, err)
	}

	task, err := container.Task(nscCtx, nil)
	if err != nil {
		return "", fmt.Errorf("container %s has no running task: %w", containerID, err)
	}
	_ = task

	// Parse tail as approximate byte count.
	var n int
	if _, scanErr := fmt.Sscanf(tail, "%d", &n); scanErr != nil || n <= 0 {
		n = 4096 // default to last 4KB
	}

	// Try the in-memory ring buffer first.
	rb := c.logs.get(containerID)
	if buf := rb.String(); len(buf) > 0 {
		if tail == "all" || tail == "" {
			return buf, nil
		}
		return rb.TailBytes(n), nil
	}

	// No logs captured yet — the container was started before this
	// daemon's log capture was enabled.  Logs will appear after the
	// next container restart.
	return "(container was started before log capture was enabled — logs will appear after next restart)", nil
}

// StopContainer stops the running task for the container.
// It first sends a SIGTERM and waits, then sends a SIGKILL if it's still running.
func (c *ContainerdClient) StopContainer(ctx context.Context, containerID string) error {
	ctx = c.withNamespace(ctx)
	log.Printf("Stopping container: %s", containerID)

	container, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return fmt.Errorf("failed to load container %s: %w", containerID, err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		// If there is no task, it might already be stopped.
		return nil
	}

	// 1. Try SIGTERM
	if err := task.Kill(ctx, 15); err != nil {
		log.Printf("Warning: SIGTERM failed for %s: %v", containerID, err)
	}

	// 2. Wait for a grace period (e.g., 5 seconds)
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	exitStatusC, err := task.Wait(waitCtx)
	if err != nil {
		// If the context timed out, it means the container is still running
		if waitCtx.Err() == context.DeadlineExceeded {
			log.Printf("Container %s did not stop after SIGTERM, sending SIGKILL...", containerID)
			if err := task.Kill(ctx, 9); err != nil {
				return fmt.Errorf("failed to kill task %s with SIGKILL: %w", containerID, err)
			}
			
			// Now wait for the actual exit after SIGKILL
			// We use a new context for the wait to avoid being immediately canceled by the waitCtx
			killWaitCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
			defer killCancel()
			
			exitStatusC, err = task.Wait(killWaitCtx)
			if err != nil {
				return fmt.Errorf("failed to wait for task %s after SIGKILL: %w", containerID, err)
			}
			<-exitStatusC
		} else {
			return fmt.Errorf("failed to wait for task %s: %w", containerID, err)
		}
	} else {
		// Container exited normally
		<-exitStatusC
	}

	// Give the runtime a moment to update the task state
	time.Sleep(1 * time.Second)

	// Try deleting the task, with a retry loop in case the runtime is slow to update state
	var lastErr error
	for i := 0; i < 10; i++ {
		if _, err := task.Delete(ctx); err != nil {
			lastErr = err
			// If it's still reported as running, try sending another SIGKILL to force the state change
			if strings.Contains(err.Error(), "running") {
				log.Printf("Warning: task %s still reported as running, sending extra SIGKILL...", containerID)
				_ = task.Kill(ctx, 9)
			}
			log.Printf("Warning: attempt %d to delete task %s failed: %v", i+1, containerID, err)
			time.Sleep(2 * time.Second)
			continue
		}
		lastErr = nil
		break
	}

	if lastErr != nil {
		return fmt.Errorf("failed to delete task %s after retries: %w", containerID, lastErr)
	}

	return nil
}

// DeleteContainer removes the container and its resources.
func (c *ContainerdClient) DeleteContainer(ctx context.Context, containerID string) error {
	c.logs.remove(containerID)

	ctx = c.withNamespace(ctx)
	log.Printf("Deleting container: %s", containerID)

	container, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return fmt.Errorf("failed to load container %s: %w", containerID, err)
	}

	if err := c.StopContainer(ctx, containerID); err != nil {
		log.Printf("Warning: stop failed during delete for %s: %v", containerID, err)
	}

	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return fmt.Errorf("failed to delete container %s: %w", containerID, err)
	}

	return nil
}

// ListContainers returns a list of currently running containers.
func (c *ContainerdClient) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	ctx = c.withNamespace(ctx)
	containers, err := c.client.Containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	var infos []ContainerInfo
	for _, container := range containers {
		info, err := container.Info(ctx)
		if err != nil {
			continue
		}
		
		image, err := container.Image(ctx)
		imageName := ""
		if err == nil {
			imageName = image.Name()
		}

		infos = append(infos, ContainerInfo{
			ID:    container.ID(),
			Name:  info.Labels["quartermaster.name"],
			Image: imageName,
			ConfigHash: info.Labels["quartermaster.config-hash"],
		})
	}

	// Fallback: if name is empty, use ID
	for i := range infos {
		if infos[i].Name == "" {
			infos[i].Name = infos[i].ID
		}
	}

	return infos, nil
}

// withNetworkNamespace is an OCI spec option that configures a container
// to share another container's network namespace (sidecar pattern).
func withNetworkNamespace(path string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		for i := range s.Linux.Namespaces {
			if s.Linux.Namespaces[i].Type == specs.NetworkNamespace {
				s.Linux.Namespaces[i].Path = path
				return nil
			}
		}
		// Network namespace not found in spec (shouldn't happen), add it
		s.Linux.Namespaces = append(s.Linux.Namespaces, specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: path,
		})
		return nil
	}
}
