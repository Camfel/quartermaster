// Package daemon manages the Quartermaster reconciliation loop, health checks,
// and status API.
package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/git"
	"quartermaster/pkg/health"
	"quartermaster/pkg/metrics"
	"quartermaster/pkg/network"
	"quartermaster/pkg/reconciler"
	"quartermaster/pkg/types"
)

// Daemon manages the lifecycle of the Quartermaster reconciliation loop.
type Daemon struct {
	reconciler      *reconciler.Reconciler
	containerClient cri.ContainerClient
	configManager   *config.ConfigManager
	netMgr          network.NetManager
	stackFile       string
	socketPath      string
	settingsPath    string
	syncInterval    time.Duration

	healthChecker *health.Checker
	watchers      []*git.Watcher
	metrics       *metrics.Metrics
	metricsAddr   string
	gitChangeCh   <-chan struct{}

	lkgPath             string
	consecutiveFailures int
	maxFailures         int

	status *Status

	reconcileChan chan struct{}
	reloadCh      chan struct{}
}

// NewDaemon initializes a new Daemon instance.
func NewDaemon(
	r *reconciler.Reconciler,
	cc cri.ContainerClient,
	cm *config.ConfigManager,
	nm network.NetManager,
	stackFile string,
	socketPath string,
	settingsPath string,
	lkgPath string,
	syncInterval time.Duration,
	maxFailures int,
	watchers []*git.Watcher,
	m *metrics.Metrics,
	metricsAddr string,
	gitChangeCh <-chan struct{},
) *Daemon {
	return &Daemon{
		reconciler:      r,
		containerClient: cc,
		configManager:   cm,
		netMgr:          nm,
		stackFile:       stackFile,
		socketPath:      socketPath,
		settingsPath:    settingsPath,
		lkgPath:         lkgPath,
		syncInterval:    syncInterval,
		maxFailures:     maxFailures,
		healthChecker:   health.NewChecker(),
		watchers:        watchers,
		metrics:         m,
		metricsAddr:     metricsAddr,
		gitChangeCh:     gitChangeCh,
		reconcileChan:   make(chan struct{}, 1),
		reloadCh:        make(chan struct{}, 1),
		status: &Status{
			Version:    apiVersion,
			StartedAt:  time.Now(),
			LKGHealthy: true, // assume healthy until proven otherwise
		},
	}
}

// Run starts the reconciliation loop and status API. Blocks until cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.syncInterval)
	defer ticker.Stop()

	healthTicker := time.NewTicker(30 * time.Second)
	defer healthTicker.Stop()

	log.Printf("Daemon loop started. Sync interval: %v, Health interval: 30s", d.syncInterval)

	// ── Start git watchers ─────────────────────────────────────────
	for _, w := range d.watchers {
		go func(watcher *git.Watcher) {
			if err := watcher.Start(ctx); err != nil {
				log.Printf("Git watcher for %s exited: %v", watcher.RepoURL(), err)
			}
		}(w)
	}

	// ── Start metrics listener ────────────────────────────────────
	if d.metrics != nil && d.metricsAddr != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/v1/metrics", d.metrics.Handler())
		metricsSrv := &http.Server{
			Addr:         d.metricsAddr,
			Handler:      metricsMux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  30 * time.Second,
		}
		go func() {
			ln, err := net.Listen("tcp", d.metricsAddr)
			if err != nil {
				log.Printf("Warning: metrics listener failed: %v", err)
				return
			}
			log.Printf("Metrics endpoint listening on %s/v1/metrics", d.metricsAddr)
			if err := metricsSrv.Serve(ln); err != http.ErrServerClosed {
				log.Printf("Metrics listener error: %v", err)
			}
		}()
	}

	// ── Start status API ────────────────────────────────────────────
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

	var mh http.Handler
	if d.metrics != nil {
		mh = d.metrics.Handler()
	}
	if err := startAPI(d.socketPath, d.status, d.reloadCh, d.reconcileChan, logLookup, restartService, mh); err != nil {
		log.Printf("Warning: status API failed to start: %v", err)
	}

	// ── Initial sync ────────────────────────────────────────────────
	if err := d.reconcile(ctx); err != nil {
		log.Printf("Initial reconciliation failed: %v", err)
	}

	// ── Event loop ──────────────────────────────────────────────────
	for {
		select {
		case <-ctx.Done():
			log.Println("Daemon loop received shutdown signal.")
			return nil
		case <-ticker.C:
			if err := d.reconcile(ctx); err != nil {
				log.Printf("Reconciliation error (ticker): %v", err)
			}
		case <-healthTicker.C:
			d.runHealthChecks(ctx)
		case <-d.reconcileChan:
			if err := d.reconcile(ctx); err != nil {
				log.Printf("Reconciliation error (triggered): %v", err)
			}
		case <-d.reloadCh:
			log.Println("Reload requested — re-reading settings...")
			if err := d.reload(ctx); err != nil {
				log.Printf("Reload failed: %v", err)
			} else {
				d.TriggerReconcile()
			}
		case <-d.gitChangeCh:
			log.Println("Git change detected — triggering reconcile")
			d.TriggerReconcile()
		}
	}
}

// reconcile loads the stack file and runs reconciliation.
func (d *Daemon) reconcile(ctx context.Context) error {
	start := time.Now()

	reconCtx, cancel := context.WithTimeout(ctx, d.syncInterval)
	if d.syncInterval > 2*time.Second {
		var cancel2 context.CancelFunc
		reconCtx, cancel2 = context.WithTimeout(ctx, d.syncInterval-1*time.Second)
		defer cancel2()
	}
	defer cancel()

	// Merge all stack files (components + repos) into a single combined stack.
	stack, err := d.loadMergedStack()
	if err != nil {
		recordReconcile(d.status, err)
		log.Printf("Failed to load stack: %v", err)
		return fmt.Errorf("failed to load desired state: %w", err)
	}

	err = d.reconciler.ReconcileStack(reconCtx, stack)

	if d.metrics != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		d.metrics.RecordReconcile(outcome, time.Since(start))
	}

	if err != nil {
		d.consecutiveFailures++
		log.Printf("Reconciliation failed (%d consecutive): %v", d.consecutiveFailures, err)

		// Roll back to LKG after N consecutive failures.
		if d.maxFailures > 0 && d.consecutiveFailures >= d.maxFailures {
			log.Printf("Rolling back to Last Known Good manifest: %s", d.lkgPath)
			if lkg, lkgErr := d.configManager.LoadStack(d.lkgPath); lkgErr == nil {
				if rollbackErr := d.reconciler.ReconcileStack(reconCtx, lkg); rollbackErr != nil {
					log.Printf("LKG rollback also failed: %v", rollbackErr)
					d.status.LKGHealthy = false
					d.status.LKGError = rollbackErr.Error()
				} else {
					log.Println("LKG rollback successful.")
					d.consecutiveFailures = 0
					d.status.LKGHealthy = true
					d.status.LKGError = ""
				}
			} else {
				log.Printf("Cannot roll back — LKG manifest %s is invalid: %v", d.lkgPath, lkgErr)
				d.status.LKGHealthy = false
				d.status.LKGError = lkgErr.Error()
			}
		}
		recordReconcile(d.status, err)
		return err
	}

	// Success — save LKG and reset failure count.
	d.consecutiveFailures = 0
	if err := d.configManager.SaveStack(d.lkgPath, stack); err != nil {
		log.Printf("Warning: failed to save LKG: %v", err)
	}
	d.status.LKGHealthy = true
	d.status.LKGError = ""

	recordReconcile(d.status, nil)

	containers, listErr := d.containerClient.ListContainers(ctx)
	if listErr == nil {
		recordContainers(d.status, containers, stack)
		if d.metrics != nil {
			running := 0
			for _, c := range containers {
				if c.Running {
					running++
				}
			}
			d.metrics.SetContainers(len(stack.Spec.Services), running)
		}
	}

	log.Println("Reconciliation complete.")
	return nil
}

// runHealthChecks probes every service with a configured health check.
// Unhealthy containers are stopped and deleted so the reconciler redeploys
// them on the next pass.
func (d *Daemon) runHealthChecks(ctx context.Context) {
	stack, err := d.loadMergedStack()
	if err != nil {
		log.Printf("Health check: failed to load stack: %v", err)
		return
	}

	containers, err := d.containerClient.ListContainers(ctx)
	if err != nil {
		log.Printf("Health check: failed to list containers: %v", err)
		return
	}

	idByName := make(map[string]string)
	for _, c := range containers {
		idByName[c.Name] = c.ID
	}

	for _, svc := range stack.Spec.Services {
		if svc.HealthCheck == nil {
			continue
		}

		containerID, exists := idByName[svc.Name]
		if !exists {
			continue
		}

		// Resolve bridge IP for non-public containers.
		var bridgeIP string
		if d.netMgr != nil {
			if ip := d.netMgr.LookupIP(svc.Name); ip != nil {
				bridgeIP = ip.String()
			}
		}

		result := d.healthChecker.RunCheck(svc, bridgeIP)

		// Write result back to status so the GUI shows health state.
		healthy := result.Healthy
		for i := range d.status.Containers {
			if d.status.Containers[i].Name == svc.Name {
				d.status.Containers[i].Healthy = &healthy
				break
			}
		}

		if d.metrics != nil {
			outcome := "pass"
			if !result.Healthy {
				outcome = "fail"
			}
			d.metrics.RecordHealthCheck(svc.Name, result.Type, outcome, result.Duration)
		}

		if result.Healthy {
			continue
		}

		log.Printf("Health check failed for %s (%s): %v — restarting",
			svc.Name, result.Type, result.Error)

		if err := d.containerClient.StopContainer(ctx, containerID); err != nil {
			log.Printf("Warning: stop failed for %s: %v", svc.Name, err)
		}
		if err := d.containerClient.DeleteContainer(ctx, containerID); err != nil {
			log.Printf("Warning: delete failed for %s: %v", svc.Name, err)
		}
		// Trigger reconciliation so the service is recreated.
		d.TriggerReconcile()
		// Only restart one unhealthy service per tick to avoid cascades.
		break
	}
}

// loadMergedStack loads the primary stack and merges all additional stacks
// from components and repos (via StackFiles).  The merged result includes
// services from every enabled component and user repo.
func (d *Daemon) loadMergedStack() (*types.Stack, error) {
	stack, err := d.configManager.LoadStack(d.stackFile)
	if err != nil {
		return nil, err
	}

	settings, sErr := config.LoadSettings(d.settingsPath)
	if sErr != nil {
		return stack, nil // settings not available — use primary stack only
	}

	files := settings.StackFiles()
	sort.Strings(files) // deterministic order regardless of map iteration

	for _, f := range files {
		if f == d.stackFile {
			continue // already loaded as primary
		}
		additional, aErr := d.configManager.LoadStack(f)
		if aErr != nil {
			log.Printf("Warning: skipping stack %s: %v", f, aErr)
			continue
		}
		stack = d.configManager.MergeStacks(stack, additional)
	}
	return stack, nil
}

// reload re-reads the settings file and updates the daemon's stack file path.
func (d *Daemon) reload(ctx context.Context) error {
	settings, err := config.LoadSettings(d.settingsPath)
	if err != nil {
		return fmt.Errorf("failed to reload settings: %w", err)
	}

	files := settings.StackFiles()
	if len(files) > 0 {
		d.stackFile = files[0]
	}
	d.reconciler.SetIngressConfig(settings.Ingress.Domain, settings.Ingress.TLS)
	log.Printf("Reloaded settings: stack = %s, ingress = %s/%s", d.stackFile, settings.Ingress.Domain, settings.Ingress.TLS)
	return nil
}

// TriggerReconcile signals the event loop to run reconciliation.
func (d *Daemon) TriggerReconcile() {
	select {
	case d.reconcileChan <- struct{}{}:
	default:
	}
}
