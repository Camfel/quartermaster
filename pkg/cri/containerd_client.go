package cri

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
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
	mu  sync.Mutex
	buf bytes.Buffer
	cap int // max bytes to retain
}

func newRingBuffer(cap int) *ringBuffer {
	return &ringBuffer{cap: cap}
}

func (rb *ringBuffer) Write(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.buf.Len()+len(p) > rb.cap {
		excess := rb.buf.Len() + len(p) - rb.cap
		if excess < rb.buf.Len() {
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
	mu   sync.Mutex
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

// ── Persistent log file with rotation ──────────────────────────────────

// rotatingFile writes to a log file, rotating when it exceeds maxBytes.
// Safe for concurrent use by a single writer (container stdout/stderr).
type rotatingFile struct {
	dir      string
	maxBytes int64
	maxFiles int
	mu       sync.Mutex
	file     *os.File
	written  int64
}

func newRotatingFile(dir string, maxBytes int64, maxFiles int) (*rotatingFile, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	rf := &rotatingFile{dir: dir, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := rf.open(); err != nil {
		return nil, err
	}
	return rf, nil
}

func (rf *rotatingFile) open() error {
	path := filepath.Join(rf.dir, "current.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	fi, _ := f.Stat()
	rf.file = f
	rf.written = fi.Size()
	return nil
}

func (rf *rotatingFile) Write(p []byte) (int, error) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.written+int64(len(p)) > rf.maxBytes && rf.maxBytes > 0 {
		rf.rotate()
	}
	n, err := rf.file.Write(p)
	rf.written += int64(n)
	return n, err
}

func (rf *rotatingFile) rotate() {
	rf.file.Close()
	for i := rf.maxFiles - 1; i >= 1; i-- {
		old := filepath.Join(rf.dir, fmt.Sprintf("current.%d.log", i))
		new := filepath.Join(rf.dir, fmt.Sprintf("current.%d.log", i+1))
		os.Rename(old, new)
	}
	current := filepath.Join(rf.dir, "current.log")
	first := filepath.Join(rf.dir, "current.1.log")
	os.Rename(current, first)
	rf.open()
}

func (rf *rotatingFile) Close() error {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.file != nil {
		return rf.file.Close()
	}
	return nil
}

// ContainerdClient is the real implementation of ContainerClient using containerd.
type ContainerdClient struct {
	client    *containerd.Client
	namespace string
	secrets   *secrets.Manager     // optional: for secret injection
	hwDetect  *hardware.Detector   // optional: for GPU detection
	netMgr    network.NetManager   // network: bridge, IPAM, port forwarding, VPN routing
	configMgr *configManagerLookup // optional: for ConfigMap resolution
	logs      *logStore
	logCfgs   map[string]*types.LogConfig // containerID → persistent log config
	logNames  map[string]string           // containerID → service name for log dirs
}

// configManagerLookup is a subset of ConfigManager used for runtime resolution.
type configManagerLookup struct {
	resolve  func(name, key string) (string, error)
	listKeys func(name string) (map[string]string, error)
}

// WithConfigManager sets a ConfigMap resolver for env-var and volume injection.
func (c *ContainerdClient) WithConfigManager(
	resolve func(name, key string) (string, error),
	listKeys func(name string) (map[string]string, error),
) *ContainerdClient {
	c.configMgr = &configManagerLookup{resolve: resolve, listKeys: listKeys}
	return c
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
		logCfgs:   make(map[string]*types.LogConfig),
		logNames:  make(map[string]string),
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

// WithNetManager sets the network manager for bridge, IPAM, port forwarding,
// and VPN policy routing.
func (c *ContainerdClient) WithNetManager(nm network.NetManager) *ContainerdClient {
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

	fullRef := qualifyImageRef(ref)

	log.Printf("Pulling image: %s", fullRef)

	image, err := c.client.Pull(ctx, fullRef, containerd.WithPullUnpack)
	if err != nil {
		return "", fmt.Errorf("failed to pull image %s: %w", fullRef, err)
	}

	return image.Name(), nil
}

// qualifyImageRef adds docker.io/ prefix and :latest tag if missing.
func qualifyImageRef(ref string) string {
	parts := strings.SplitN(ref, "/", 3)
	if len(parts) >= 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		return ref
	}
	if len(parts) == 1 {
		if !strings.Contains(ref, ":") {
			return "docker.io/library/" + ref + ":latest"
		}
		return "docker.io/library/" + ref
	}
	if !strings.Contains(ref, ":") {
		return "docker.io/" + ref + ":latest"
	}
	return "docker.io/" + ref
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

	// If the service specifies a command, override the image's entrypoint.
	if len(svc.Command) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(svc.Command...))
	}

	// ── 0. Network profile routing ─────────────────────────────────
	//    public   (or empty) → host networking
	//    internal            → own netns, bridge, no host ports exposed
	//    vpn                 → own netns, bridge, CAP_NET_ADMIN, optional VPN policy routing
	netProfile := strings.ToLower(svc.Network)
	useHostNet := true
	switch netProfile {
	case "internal", "vpn":
		useHostNet = false
	}

	if useHostNet {
		specOpts = append(specOpts, oci.WithHostNamespace(specs.NetworkNamespace))
	}
	if netProfile == "vpn" {
		specOpts = append(specOpts, oci.WithAddedCapabilities([]string{"CAP_NET_ADMIN"}))
	}

	// ── 1. Add Environment Variables ───────────────────────────────
	var envVars []string
	for _, env := range svc.Env {
		resolved := false

		if env.ValueFrom != nil {
			if env.ValueFrom.SecretRef != "" && c.secrets != nil {
				sd, err := c.secrets.Resolve(env.Name, env.ValueFrom.SecretRef)
				if err == nil {
					envVars = append(envVars, fmt.Sprintf("%s=%s", env.Name, string(sd.Content)))
					resolved = true
					log.Printf("Injected secret %s → env %s for container %s", env.ValueFrom.SecretRef, env.Name, svc.Name)
				} else {
					log.Printf("Warning: failed to resolve secret %s for %s: %v", env.ValueFrom.SecretRef, svc.Name, err)
				}
			}

			if !resolved && env.ValueFrom.ConfigMapRef != "" && c.configMgr != nil {
				key := env.ValueFrom.Key
				if key == "" {
					key = env.Name
				}
				val, err := c.configMgr.resolve(env.ValueFrom.ConfigMapRef, key)
				if err == nil {
					envVars = append(envVars, fmt.Sprintf("%s=%s", env.Name, val))
					resolved = true
					log.Printf("ConfigMap %s/%s → env %s=%s for container %s", env.ValueFrom.ConfigMapRef, key, env.Name, val, svc.Name)
				} else {
					log.Printf("ConfigMap %s/%s not resolved for %s: %v", env.ValueFrom.ConfigMapRef, key, svc.Name, err)
				}
			}
		}

		if !resolved && env.Value != "" {
			envVars = append(envVars, fmt.Sprintf("%s=%s", env.Name, env.Value))
		}
	}
	if len(envVars) > 0 {
		specOpts = append(specOpts, oci.WithEnv(envVars))
	}

	// ── 2. Add Volume Mounts ───────────────────────────────────────
	for _, vol := range svc.Volumes {
		if vol.Type == "configmap" && vol.ConfigMap != nil && c.configMgr != nil {
			mountDir, cleanup, err := c.prepareConfigMapMount(vol)
			if err != nil {
				return "", fmt.Errorf("failed to prepare configmap volume for %s: %w", svc.Name, err)
			}
			_ = cleanup

			specOpts = append(specOpts, oci.WithMounts([]specs.Mount{
				{
					Type:        "bind",
					Source:      mountDir,
					Destination: vol.Target,
					Options:     []string{"bind", "ro"},
				},
			}))
			continue
		}

		specOpts = append(specOpts, oci.WithMounts([]specs.Mount{
			{
				Type:        vol.Type,
				Source:      vol.Source,
				Destination: vol.Target,
				Options:     []string{"bind", "rw"},
			},
		}))
	}

	// ── 3. Set User ────────────────────────────────────────────────
	if svc.User != "" {
		specOpts = append(specOpts, oci.WithUser(svc.User))
	}

	// ── 4. Inject Secrets ──────────────────────────────────────────
	if c.secrets != nil && len(svc.Secrets) > 0 {
		secretRefs := make([]secrets.SecretRef, len(svc.Secrets))
		for i, s := range svc.Secrets {
			secretRefs[i] = secrets.SecretRef{Name: s.Name, SecretRef: s.SecretRef}
		}
		mountDir, cleanup, err := c.secrets.PrepareMountDir(secretRefs)
		if err != nil {
			return "", fmt.Errorf("failed to prepare secrets for %s: %w", svc.Name, err)
		}
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

	// ── 5. Inject GPU resources ────────────────────────────────────
	if c.hwDetect != nil && svc.Resources != nil && svc.Resources.GPU != nil {
		gpu := svc.Resources.GPU
		if gpu.Type == "nvidia" || gpu.Type == "" {
			nvidiaEnv := c.hwDetect.NVIDIARequiredEnv()
			if len(nvidiaEnv) > 0 {
				specOpts = append(specOpts, oci.WithEnv(nvidiaEnv))
			}

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

	// ── 6. Network: set up non-host networking via NetManager ──────
	//    The NetManager handles bridge, IPAM, DNS, port forwarding, and
	//    VPN policy routing.  The CRI client only wires in the namespace
	//    and bind-mounts the DNS resolver.
	var preparedNs string
	var gatewayIP string // stored as label for staleness detection
	if !useHostNet {
		if c.netMgr == nil {
			return "", fmt.Errorf("network profile %q requires NetManager but none is configured", netProfile)
		}
		// Resolve VPN gateway (if any) for policy routing.
		vpnGateway := resolveVPNGateway(c.netMgr, netProfile, svc.DependsOn)
		netInfo, err := c.netMgr.Attach(svc.Name, netProfile, vpnGateway)
		if err != nil {
			return "", fmt.Errorf("network attach for %s: %w", svc.Name, err)
		}
		preparedNs = netInfo.NSPath

		// Record the gateway's IP so the reconciler can detect staleness
		// if the gateway restarts and gets a new IP.
		if vpnGateway != "" {
			if gwIP := c.netMgr.LookupIP(vpnGateway); gwIP != nil {
				gatewayIP = gwIP.String()
			}
		}

		// Bind-mount the single shared resolv.conf so the container has
		// working DNS via the bridge gateway's in-process forwarder.
		if preparedNs != "" {
			resolvPath := "/var/lib/quartermaster/resolv.conf"
			specOpts = append(specOpts, oci.WithMounts([]specs.Mount{
				{
					Type:        "bind",
					Source:      resolvPath,
					Destination: "/etc/resolv.conf",
					Options:     []string{"bind", "ro"},
				},
			}))

			// Mount generated hosts file for inter-container name resolution.
			hostsPath := "/var/lib/quartermaster/caddy/hosts"
			if _, err := os.Stat(hostsPath); err == nil {
				specOpts = append(specOpts, oci.WithMounts([]specs.Mount{
					{
						Type:        "bind",
						Source:      hostsPath,
						Destination: "/etc/hosts",
						Options:     []string{"bind", "ro"},
					},
				}))
			}
		}

		// Port forwarding via DNAT.
		if len(svc.Ports) > 0 && netInfo.IP != nil {
			c.netMgr.ExposePorts(svc.Name, netInfo.IP, svc.Ports)
		}
	}

	if preparedNs != "" {
		specOpts = append(specOpts, withNetworkNamespace(preparedNs))
	}

	labels := map[string]string{
		"quartermaster.name":        svc.Name,
		"quartermaster.config-hash": svc.ConfigHash,
	}
	if gatewayIP != "" {
		labels["quartermaster.gateway-ip"] = gatewayIP
	}

	container, err := c.client.NewContainer(
		ctx,
		svc.Name,
		containerd.WithNewSnapshot(svc.Name+"-snapshot", image),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(labels),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create container %s: %w", svc.Name, err)
	}

	// Store log config and service name for use by StartContainer.
	if svc.Log != nil && svc.Log.Enabled {
		c.logCfgs[container.ID()] = svc.Log
		c.logNames[container.ID()] = svc.Name
	}

	return container.ID(), nil
}

// resolveVPNGateway determines the VPN gateway name for a service that
// should route egress through a VPN.  Uses the NetManager's IP tracking
// to check if the dependency is already running.
func resolveVPNGateway(nm network.NetManager, netProfile string, dependsOn []string) string {
	if netProfile != "vpn" {
		return ""
	}

	for _, dep := range dependsOn {
		if ip := nm.LookupIP(dep); ip != nil {
			log.Printf("VPN gateway %s found at %s", dep, ip)
			return dep
		}
	}
	log.Printf("VPN gateway not found among deps %v (LookupIP returned nil)", dependsOn)
	return ""
}

// StartContainer starts a task for the container.
func (c *ContainerdClient) StartContainer(ctx context.Context, containerID string) error {
	ctx = c.withNamespace(ctx)
	log.Printf("Starting container: %s", containerID)

	container, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return fmt.Errorf("failed to load container %s: %w", containerID, err)
	}

	rb := c.logs.get(containerID)
	var logWriter io.Writer = rb

	// If persistent logging is enabled for this container, tee output to
	// a rotating log file on disk at /var/lib/quartermaster/logs/<name>/.
	if lc, ok := c.logCfgs[containerID]; ok && lc.Enabled {
		maxBytes := int64(50 * 1024 * 1024) // default 50MB
		if lc.MaxSize != "" {
			if parsed, err := parseSize(lc.MaxSize); err == nil {
				maxBytes = parsed
			} else {
				log.Printf("Warning: invalid log max_size %q for %s, using 50MB", lc.MaxSize, containerID)
			}
		}
		maxFiles := lc.MaxFiles
		if maxFiles <= 0 {
			maxFiles = 5
		}
		name := c.logNames[containerID]
		if name == "" {
			name = containerID
		}
		dir := filepath.Join("/var/lib/quartermaster/logs", name)
		if rf, err := newRotatingFile(dir, maxBytes, maxFiles); err == nil {
			logWriter = io.MultiWriter(rb, rf)
			log.Printf("Persistent logging enabled for %s → %s", containerID, dir)
		} else {
			log.Printf("Warning: failed to create rotating log for %s: %v", containerID, err)
			logWriter = rb
		}
	}

	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(
		nil,
		logWriter,
		logWriter,
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
		return 0, nil
	}

	return task.Pid(), nil
}

// ContainerLogs returns the trailing logs for a running container.
func (c *ContainerdClient) ContainerLogs(ctx context.Context, containerID string, tail string) (string, error) {
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

	var n int
	if _, scanErr := fmt.Sscanf(tail, "%d", &n); scanErr != nil || n <= 0 {
		n = 4096
	}

	rb := c.logs.get(containerID)
	if buf := rb.String(); len(buf) > 0 {
		if tail == "all" || tail == "" {
			return buf, nil
		}
		return rb.TailBytes(n), nil
	}

	return "(container was started before log capture was enabled — logs will appear after next restart)", nil
}

// StopContainer stops the running task for the container.
func (c *ContainerdClient) StopContainer(ctx context.Context, containerID string) error {
	ctx = c.withNamespace(ctx)
	log.Printf("Stopping container: %s", containerID)

	container, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return fmt.Errorf("failed to load container %s: %w", containerID, err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return nil
	}

	if err := task.Kill(ctx, 15); err != nil {
		log.Printf("Warning: SIGTERM failed for %s: %v", containerID, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	exitStatusC, err := task.Wait(waitCtx)
	if err != nil {
		if waitCtx.Err() == context.DeadlineExceeded {
			log.Printf("Container %s did not stop after SIGTERM, sending SIGKILL...", containerID)
			if err := task.Kill(ctx, 9); err != nil {
				return fmt.Errorf("failed to kill task %s with SIGKILL: %w", containerID, err)
			}

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
		<-exitStatusC
	}

	time.Sleep(1 * time.Second)

	var lastErr error
	for i := 0; i < 10; i++ {
		if _, err := task.Delete(ctx); err != nil {
			lastErr = err
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

		// Check if the task is actually running.
		running := false
		var pid uint32
		if task, err := container.Task(ctx, nil); err == nil {
			pid = task.Pid()
			running = pid > 0
		}

		infos = append(infos, ContainerInfo{
			ID:         container.ID(),
			Name:       info.Labels["quartermaster.name"],
			Image:      imageName,
			ConfigHash: info.Labels["quartermaster.config-hash"],
			Running:    running,
			PID:        pid,
			GatewayIP:  info.Labels["quartermaster.gateway-ip"],
		})
	}

	for i := range infos {
		if infos[i].Name == "" {
			infos[i].Name = infos[i].ID
		}
	}

	return infos, nil
}

// prepareConfigMapMount creates a temporary directory containing all keys
// from a ConfigMap as individual files.
func (c *ContainerdClient) prepareConfigMapMount(vol types.Volume) (string, func(), error) {
	if c.configMgr == nil {
		return "", nil, fmt.Errorf("config manager not configured")
	}

	data, err := c.configMgr.listKeys(vol.ConfigMap.Name)
	if err != nil {
		return "", nil, fmt.Errorf("configmap %q: %w", vol.ConfigMap.Name, err)
	}

	dir, err := os.MkdirTemp("", "quartermaster-configmap-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create configmap tmp dir: %w", err)
	}

	cleanup := func() { os.RemoveAll(dir) }

	for key, val := range data {
		path := filepath.Join(dir, key)
		if err := os.WriteFile(path, []byte(val), 0444); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("failed to write configmap key %q: %w", key, err)
		}
	}

	return dir, cleanup, nil
}

// withNetworkNamespace is an OCI spec option that points the container's
// network namespace at a pre-created named netns (via /var/run/netns/...).
func withNetworkNamespace(path string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		for i := range s.Linux.Namespaces {
			if s.Linux.Namespaces[i].Type == specs.NetworkNamespace {
				s.Linux.Namespaces[i].Path = path
				return nil
			}
		}
		s.Linux.Namespaces = append(s.Linux.Namespaces, specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: path,
		})
		return nil
	}
}

// parseSize converts a human-readable size string like "10MB" to bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size: %q", s)
	}
	return n * multiplier, nil
}
