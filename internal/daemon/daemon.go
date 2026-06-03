package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/git"
	"quartermaster/pkg/health"
	"quartermaster/pkg/reconciler"
	"quartermaster/pkg/types"
)

// Daemon manages the lifecycle of the Quartermaster reconciliation loop.
type Daemon struct {
	reconciler          *reconciler.Reconciler
	containerClient     cri.ContainerClient
	configManager       *config.ConfigManager
	stackFiles          []string
	lkgPath             string
	socketPath          string
	settingsPath        string
	syncInterval        time.Duration
	healthCheckInterval time.Duration
	watchers            []*git.Watcher
	healthChecker       *health.Checker
	reconcileChan       chan struct{}
	reloadCh            chan struct{}

	status   *Status
	statusMu *sync.RWMutex

	consecutiveFailures map[string]int
	maxFailures         int

	eventHub     *EventHub
	latestStack   *types.Stack
	latestStackMu sync.RWMutex
}

// NewDaemon initializes a new Daemon instance.
func NewDaemon(
	r *reconciler.Reconciler,
	cc cri.ContainerClient,
	cm *config.ConfigManager,
	stackFiles []string,
	lkgPath string,
	socketPath string,
	settingsPath string,
	syncInterval, healthCheckInterval time.Duration,
	maxFailures int,
	watchers []*git.Watcher,
) *Daemon {
	d := &Daemon{
		reconciler:          r,
		containerClient:     cc,
		configManager:       cm,
		stackFiles:          stackFiles,
		lkgPath:             lkgPath,
		socketPath:          socketPath,
		settingsPath:        settingsPath,
		syncInterval:        syncInterval,
		healthCheckInterval: healthCheckInterval,
		watchers:            watchers,
		healthChecker:       health.NewChecker(),
		reconcileChan:       make(chan struct{}, 1),
		reloadCh:            make(chan struct{}, 1),
		consecutiveFailures: make(map[string]int),
		maxFailures:         maxFailures,
		eventHub:            NewEventHub(),
		status: &Status{
			Version:   apiVersion,
			StartedAt: time.Now(),
		},
		statusMu: &sync.RWMutex{},
	}

	for _, w := range watchers {
		w := w
		w.SetOnChanged(func(ctx context.Context, newHash string) {
			log.Printf("Watcher detected change at hash %s, triggering reconciliation", newHash)
			d.TriggerReconcile()
		})
	}

	return d
}

// Run starts the reconciliation loop and status API. Blocks until cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.syncInterval)
	defer ticker.Stop()

	log.Printf("Daemon loop started. Sync interval: %v, %d repo(s)", d.syncInterval, len(d.watchers))

	// ── Start status API ────────────────────────────────────────────
	// Build a log lookup closure that maps service name → container logs.
	logLookup := func(ctx context.Context, serviceName string, tail string) (string, error) {
		containers, err := d.containerClient.ListContainers(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to list containers: %w", err)
		}
		for _, c := range containers {
			if c.Name == serviceName {
				return d.containerClient.ContainerLogs(ctx, c.ID, tail)
			}
		}
		return "", fmt.Errorf("container for service %q not found", serviceName)
	}

	// Build a restart closure that stops + deletes a container by name.
	// The reconciler will detect the missing container and redeploy it.
	restartService := func(ctx context.Context, serviceName string) error {
		containers, err := d.containerClient.ListContainers(ctx)
		if err != nil {
			return fmt.Errorf("failed to list containers: %w", err)
		}
		for _, c := range containers {
			if c.Name == serviceName {
				log.Printf("Restart requested for %s (container %s)", serviceName, c.ID)
				if err := d.containerClient.StopContainer(ctx, c.ID); err != nil {
					log.Printf("Warning: stop failed for %s: %v", serviceName, err)
				}
				if err := d.containerClient.DeleteContainer(ctx, c.ID); err != nil {
					return fmt.Errorf("delete failed for %s: %w", serviceName, err)
				}
				d.TriggerReconcile()
				return nil
			}
		}
		return fmt.Errorf("container for service %q not found", serviceName)
	}

	if err := startAPI(d.socketPath, d.status, d.statusMu, d.reloadCh, d.reconcileChan, d.settingsPath, d.eventHub, d.latestStackSnapshot, logLookup, restartService); err != nil {
		log.Printf("Warning: status API failed to start: %v", err)
	}

	// ── Start watchers ──────────────────────────────────────────────
	for _, w := range d.watchers {
		w := w
		go func() {
			if err := w.Start(ctx); err != nil && err != context.Canceled {
				log.Printf("Watcher error: %v", err)
			}
		}()
	}

	// ── Initial sync ────────────────────────────────────────────────
	if err := d.reconcile(ctx); err != nil {
		log.Printf("Initial reconciliation failed: %v", err)
	}

	// ── Event loop ──────────────────────────────────────────────────
	healthTicker := time.NewTicker(d.healthCheckInterval)
	defer healthTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Daemon loop received shutdown signal.")
			return nil
		case <-ticker.C:
			if err := d.reconcile(ctx); err != nil {
				log.Printf("Reconciliation error (ticker): %v", err)
			}
		case <-d.reconcileChan:
			if err := d.reconcile(ctx); err != nil {
				log.Printf("Reconciliation error (watcher): %v", err)
			}
		case <-d.reloadCh:
			log.Println("Reload requested — re-reading settings...")
			if err := d.reload(ctx); err != nil {
				log.Printf("Reload failed: %v", err)
			} else {
				d.TriggerReconcile()
			}
		case <-healthTicker.C:
			d.runHealthChecks(ctx)
		}
	}
}

// reconcile loads all stack files, merges them, and reconciles.
func (d *Daemon) reconcile(ctx context.Context) error {
	reconCtx, cancel := context.WithTimeout(ctx, d.syncInterval-1*time.Second)
	defer cancel()

	// Load ConfigMaps from the same directories as stacks.
	d.loadConfigMaps()

	stack, err := d.loadMergedStack()
	if err != nil {
		recordReconcile(d.status, d.statusMu, err)
		d.eventHub.PublishEvent("reconcile", ReconcileData{
			Success: false,
			Error:   err.Error(),
			Count:   d.status.ReconcileCount + 1,
		})
		return fmt.Errorf("failed to load desired state: %w", err)
	}

	// Store the latest stack for service detail lookups.
	d.latestStackMu.Lock()
	d.latestStack = stack
	d.latestStackMu.Unlock()

	err = d.reconciler.ReconcileStack(reconCtx, stack)
	recordReconcile(d.status, d.statusMu, err)

	d.eventHub.PublishEvent("reconcile", ReconcileData{
		Success: err == nil,
		Error:   func() string { if err != nil { return err.Error() }; return "" }(),
		Count:   d.status.ReconcileCount,
	})

	if err != nil {
		log.Printf("Reconciliation failed: %v", err)

		lkgStack, lkgErr := d.configManager.LoadStack(d.lkgPath)
		if lkgErr != nil {
			log.Printf("No LKG manifest available for rollback: %v", lkgErr)
			return err
		}

		log.Println("Rolling back to Last Known Good manifest...")
		rollbackErr := d.reconciler.ReconcileStack(reconCtx, lkgStack)
		if rollbackErr != nil {
			log.Printf("LKG rollback also failed: %v", rollbackErr)
			return fmt.Errorf("reconciliation failed (%v) and LKG rollback also failed (%v)", err, rollbackErr)
		}

		log.Println("Successfully rolled back to LKG manifest")
		return err
	}

	if saveErr := d.configManager.SaveStack(d.lkgPath, stack); saveErr != nil {
		log.Printf("Warning: failed to save LKG manifest: %v", saveErr)
	}

	// Update container status after successful reconcile.
	containers, listErr := d.containerClient.ListContainers(ctx)
	if listErr == nil {
		recordContainers(d.status, d.statusMu, containers)
		// Fill in PID + running state.
		for _, c := range containers {
			pid, pidErr := d.containerClient.GetContainerPID(ctx, c.ID)
			updateContainerHealth(d.status, d.statusMu, c.Name, pidErr == nil && pid > 0, pid)
		}
	}

	// Publish full status snapshot to WebSocket subscribers.
	d.publishStatusEvent()

	return nil
}

// loadConfigMaps scans directories for ConfigMap YAML files and registers
// them by name.  Priority (last wins):
//
//  1. /etc/quartermaster/configmaps/   — baseline (GUI/CLI for non-technical users)
//  2. Component catalog                — curated defaults
//  3. User Git repos                   — highest priority (technical users)
//
// This means a user who defines "vpn-config" in their Git repo overrides
// the same ConfigMap set via the GUI, while a non-technical user who only
// uses the GUI gets their values from /etc/quartermaster/configmaps/.
func (d *Daemon) loadConfigMaps() {
	// Order matters: scan baseline first so higher-priority sources overwrite.
	dirs := []string{"/etc/quartermaster/configmaps"}
	dirs = append(dirs, d.stackDirs()...) // Git repos override local config

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if _, err := d.configManager.LoadConfigMap(path); err != nil {
				// Not a ConfigMap file — that's fine.
				continue
			}
		}
	}
}

// stackDirs returns unique directories containing stack files.
func (d *Daemon) stackDirs() []string {
	seen := make(map[string]bool)
	var dirs []string
	for _, p := range d.stackFiles {
		dir := filepath.Dir(p)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func (d *Daemon) loadMergedStack() (*types.Stack, error) {
	if len(d.stackFiles) == 0 {
		return nil, fmt.Errorf("no stack files configured")
	}

	var merged *types.Stack
	serviceMap := make(map[string]types.Service)
	order := make([]string, 0)

	for _, path := range d.stackFiles {
		s, err := d.configManager.LoadStack(path)
		if err != nil {
			log.Printf("Warning: skipping stack %s: %v", path, err)
			continue
		}
		if merged == nil {
			merged = s
		}
		for _, svc := range s.Spec.Services {
			if _, exists := serviceMap[svc.Name]; !exists {
				order = append(order, svc.Name)
			}
			serviceMap[svc.Name] = svc
		}
	}

	if merged == nil {
		return nil, fmt.Errorf("no valid stacks loaded from %v", d.stackFiles)
	}

	merged.Spec.Services = make([]types.Service, 0, len(serviceMap))
	for _, name := range order {
		merged.Spec.Services = append(merged.Spec.Services, serviceMap[name])
	}

	log.Printf("Merged %d stack(s) → %d unique service(s)", len(d.stackFiles), len(merged.Spec.Services))
	return merged, nil
}

func (d *Daemon) runHealthChecks(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	stack, err := d.loadMergedStack()
	if err != nil {
		log.Printf("Health check: failed to load merged stack: %v", err)
		return
	}

	containers, err := d.containerClient.ListContainers(ctx)
	if err != nil {
		log.Printf("Health check: failed to list containers: %v", err)
		return
	}

	containerMap := make(map[string]cri.ContainerInfo)
	for _, c := range containers {
		containerMap[c.Name] = c
	}

	for _, svc := range stack.Spec.Services {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if svc.HealthCheck == nil {
			continue
		}

		container, exists := containerMap[svc.Name]
		if !exists {
			continue
		}

		result := d.healthChecker.RunCheck(svc)

		d.eventHub.PublishEvent("health", HealthData{
			Service: svc.Name,
			Healthy: result.Healthy,
			Type:    result.Type,
			Error:   func() string { if result.Error != nil { return result.Error.Error() }; return "" }(),
		})

		if result.Healthy {
			if d.consecutiveFailures[svc.Name] > 0 {
				log.Printf("Health check for %s passed after %d failure(s)",
					svc.Name, d.consecutiveFailures[svc.Name])
			}
			d.consecutiveFailures[svc.Name] = 0
			setContainerHealthy(d.status, d.statusMu, svc.Name, true)
		} else {
			d.consecutiveFailures[svc.Name]++
			failures := d.consecutiveFailures[svc.Name]
			setContainerHealthy(d.status, d.statusMu, svc.Name, false)

			log.Printf("Health check FAILED for %s (%s): %v (failure %d/%d)",
				svc.Name, result.Type, result.Error, failures, d.maxFailures)

			if failures >= d.maxFailures {
				log.Printf("Service %s has failed %d health checks. Triggering LKG rollback.",
					svc.Name, failures)
				d.consecutiveFailures[svc.Name] = 0
				d.TriggerReconcile()
			} else {
				log.Printf("Attempting to restart %s (container %s)...", svc.Name, container.ID)
				if err := d.restartContainer(ctx, container.ID); err != nil {
					log.Printf("Failed to restart %s: %v", svc.Name, err)
				} else {
					log.Printf("Successfully restarted %s", svc.Name)
				}
			}
		}
	}
}

func (d *Daemon) restartContainer(ctx context.Context, containerID string) error {
	if err := d.containerClient.StopContainer(ctx, containerID); err != nil {
		return fmt.Errorf("stop failed: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	if err := d.containerClient.StartContainer(ctx, containerID); err != nil {
		return fmt.Errorf("start failed: %w", err)
	}
	return nil
}

// reload re-reads the settings file and updates the daemon's stack file list.
// New watchers are started for any repos/components not previously tracked.
func (d *Daemon) reload(ctx context.Context) error {
	settings, err := config.LoadSettings(d.settingsPath)
	if err != nil {
		return fmt.Errorf("failed to reload settings: %w", err)
	}

	d.stackFiles = settings.StackFiles()
	log.Printf("Reloaded settings: %d stack file(s)", len(d.stackFiles))

	// Track which repo URLs we already watch (all components share one URL).
	seenURLs := make(map[string]bool)
	for _, w := range d.watchers {
		seenURLs[w.RepoURL()] = true
	}

	// Start watchers for newly enabled components.
	for _, repo := range settings.ExpandComponents() {
		if seenURLs[repo.URL] {
			continue
		}
		w := git.NewWatcher(
			repo.URL, repo.Branch, repo.Token,
			repo.SSHKeyPath, repo.SSHKnownHostsPath,
			repo.LocalPath,
			repo.PollDuration(), repo.CooldownDuration(),
			nil,
		)
		w.SetOnChanged(func(ctx context.Context, newHash string) {
			log.Printf("Watcher detected change at hash %s, triggering reconciliation", newHash)
			d.TriggerReconcile()
		})
		go func() {
			if err := w.Start(ctx); err != nil && err != context.Canceled {
				log.Printf("Watcher error: %v", err)
			}
		}()
		d.watchers = append(d.watchers, w)
		log.Printf("Started component watcher for: %s", repo.URL)
	}

	return nil
}

func (d *Daemon) TriggerReconcile() {
	select {
	case d.reconcileChan <- struct{}{}:
	default:
	}
}

// publishStatusEvent sends the full daemon status to WebSocket subscribers.
func (d *Daemon) publishStatusEvent() {
	d.statusMu.RLock()
	s := *d.status
	s.Uptime = time.Since(s.StartedAt).Truncate(time.Second).String()
	if s.Containers == nil {
		s.Containers = []ContainerStatus{}
	}
	if s.Watchers == nil {
		s.Watchers = []WatcherStatus{}
	}
	d.statusMu.RUnlock()

	d.eventHub.PublishEvent("status", StatusData{
		Version:            s.Version,
		StartedAt:          s.StartedAt,
		Uptime:             s.Uptime,
		LastReconcile:      s.LastReconcile,
		LastReconcileError: s.LastReconcileError,
		ReconcileCount:     s.ReconcileCount,
		Containers:         s.Containers,
		Watchers:           s.Watchers,
		LKGHealthy:         s.LKGHealthy,
		LKGError:           s.LKGError,
	})
}

// latestStackSnapshot returns a copy of the current merged stack (thread-safe).
func (d *Daemon) latestStackSnapshot() *types.Stack {
	d.latestStackMu.RLock()
	defer d.latestStackMu.RUnlock()
	return d.latestStack
}
