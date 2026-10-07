// Package daemon manages the Quartermaster reconciliation loop, health checks,
// and status API.
package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
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
	healthInterval      time.Duration

	// healthFailures counts consecutive failed health probes per service,
	// used to escalate to an LKG rollback.  Only touched from the daemon
	// event loop.
	healthFailures map[string]int

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
	healthInterval time.Duration,
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
		healthInterval:  healthInterval,
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
		healthFailures: make(map[string]int),
	}
}

// Run starts the reconciliation loop and status API. Blocks until cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.syncInterval)
	defer ticker.Stop()

	healthInterval := d.healthInterval
	if healthInterval <= 0 {
		healthInterval = 30 * time.Second
	}
	healthTicker := time.NewTicker(healthInterval)
	defer healthTicker.Stop()

	log.Printf("Daemon loop started. Sync interval: %v, Health interval: %v", d.syncInterval, healthInterval)

	// Status starts out assuming a healthy LKG; keep the gauge in sync.
	d.setLKGHealthyMetric(true)

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
					log.Printf("Warning: delete failed for %s: %v (will still trigger reconcile)", serviceName, err)
				}
				d.TriggerReconcile()
				return nil
			}
		}
		return fmt.Errorf("container for service %q not found", serviceName)
	}

	serviceLookup := func(name string) *types.Service {
		stack := d.status.stack()
		if stack == nil {
			return nil
		}
		for i := range stack.Spec.Services {
			if stack.Spec.Services[i].Name == name {
				return &stack.Spec.Services[i]
			}
		}
		return nil
	}

	var mh http.Handler
	if d.metrics != nil {
		mh = d.metrics.Handler()
	}
	if err := startAPI(d.socketPath, d.status, d.reloadCh, d.reconcileChan, logLookup, restartService, serviceLookup, mh); err != nil {
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

		// Roll back to LKG after N consecutive failures.  Pass the daemon
		// context (not reconCtx) so the rollback gets a fresh timeout but
		// still aborts on shutdown.
		if d.maxFailures > 0 && d.consecutiveFailures >= d.maxFailures {
			if d.rollbackToLKG(ctx) {
				d.consecutiveFailures = 0
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
	d.status.setLKG(true, "")
	d.setLKGHealthyMetric(true)

	recordReconcile(d.status, nil)

	containers, listErr := d.containerClient.ListContainers(ctx)
	if listErr == nil {
		recordContainers(d.status, containers, stack)
		d.updateUnhealthyMetric()
		if d.metrics != nil {
			running := 0
			for _, c := range containers {
				if c.Running {
					running++
				}
			}
			d.metrics.SetContainers(len(stack.Spec.Services), running)
			d.recordContainerStats(ctx, containers, stack)
			if d.netMgr != nil {
				d.metrics.SetBridgeIPs(d.netMgr.IPCount(), d.netMgr.IPFree())
			}
		}
	}

	log.Println("Reconciliation complete.")
	return nil
}

// recordContainerStats publishes per-container CPU/memory metrics.  Services
// that are no longer running have their series removed so stale samples do not
// persist.
func (d *Daemon) recordContainerStats(ctx context.Context, containers []cri.ContainerInfo, stack *types.Stack) {
	if d.metrics == nil {
		return
	}
	// Bound the stats RPCs so a wedged containerd cannot stall the event loop.
	statsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	infoByName := make(map[string]cri.ContainerInfo, len(containers))
	for _, c := range containers {
		infoByName[c.Name] = c
	}
	keep := make(map[string]bool, len(stack.Spec.Services))
	for _, svc := range stack.Spec.Services {
		keep[svc.Name] = true
		c, ok := infoByName[svc.Name]
		if !ok || !c.Running {
			d.metrics.ResetContainerStats(svc.Name)
			continue
		}
		stats, err := d.containerClient.ContainerStats(statsCtx, c.ID)
		if err != nil {
			log.Printf("Warning: container stats for %s: %v", svc.Name, err)
			continue
		}
		d.metrics.RecordContainerStats(svc.Name, stats)
	}
	// Drop series for services deleted or renamed in the manifest.
	d.metrics.ResetContainerStatsNotIn(keep)
}

// setLKGHealthyMetric mirrors the LKG status into the metrics registry.
func (d *Daemon) setLKGHealthyMetric(healthy bool) {
	if d.metrics != nil {
		d.metrics.SetLKGHealthy(healthy)
	}
}

// updateUnhealthyMetric refreshes the unhealthy-container gauge from the
// latest health snapshots.
func (d *Daemon) updateUnhealthyMetric() {
	if d.metrics == nil {
		return
	}
	unhealthy := 0
	for _, c := range d.status.snapshot().Containers {
		if c.Healthy != nil && !*c.Healthy {
			unhealthy++
		}
	}
	d.metrics.SetUnhealthy(unhealthy)
}

// rollbackToLKG re-applies the Last Known Good manifest.  It runs on its own
// timeout, independent of the caller's deadline, because the reconcile pass
// that triggered it may already have exhausted its context.  Shared by the
// reconcile failure path and the health-check escalation path.
func (d *Daemon) rollbackToLKG(ctx context.Context) bool {
	log.Printf("Rolling back to Last Known Good manifest: %s", d.lkgPath)

	timeout := 2 * d.syncInterval
	if timeout < time.Minute {
		timeout = time.Minute
	}
	// Derive from the daemon context so a shutdown still cancels the
	// rollback, but give it a fresh deadline so it is not bounded by the
	// reconcile pass that triggered it.
	rollbackCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lkg, err := d.configManager.LoadStack(d.lkgPath)
	if err != nil {
		log.Printf("Cannot roll back — LKG manifest %s is invalid: %v", d.lkgPath, err)
		d.status.setLKG(false, err.Error())
		d.setLKGHealthyMetric(false)
		return false
	}
	if err := d.reconciler.ReconcileStack(rollbackCtx, lkg); err != nil {
		log.Printf("LKG rollback also failed: %v", err)
		d.status.setLKG(false, err.Error())
		d.setLKGHealthyMetric(false)
		return false
	}
	log.Println("LKG rollback successful.")
	d.status.setLKG(true, "")
	d.setLKGHealthyMetric(true)
	return true
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
	defer d.updateUnhealthyMetric()

	// Prune counters for services no longer present in the merged stack so a
	// removed/re-added service cannot inherit an old failure streak.
	configured := make(map[string]bool, len(stack.Spec.Services))
	for _, svc := range stack.Spec.Services {
		configured[svc.Name] = true
	}
	for name := range d.healthFailures {
		if !configured[name] {
			delete(d.healthFailures, name)
		}
	}

	for _, svc := range stack.Spec.Services {
		if svc.HealthCheck == nil {
			continue
		}

		containerID, exists := idByName[svc.Name]
		if !exists {
			// No running container to probe; clear any previous streak so a
			// later incarnation does not trip the threshold immediately.
			delete(d.healthFailures, svc.Name)
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
		d.status.setContainerHealth(svc.Name, result.Healthy)

		if d.metrics != nil {
			outcome := "pass"
			if !result.Healthy {
				outcome = "fail"
			}
			d.metrics.RecordHealthCheck(svc.Name, result.Type, outcome, result.Duration)
		}

		if result.Healthy {
			delete(d.healthFailures, svc.Name)
			continue
		}

		d.healthFailures[svc.Name]++
		failures := d.healthFailures[svc.Name]

		// After max_health_failures consecutive failures, escalate from a
		// restart to a Last Known Good rollback.  The rollback reconciles the
		// LKG stack itself, so do not also queue a reconcile for the current
		// (broken) desired state — that would immediately undo it.
		if d.maxFailures > 0 && failures >= d.maxFailures {
			log.Printf("Health check for %s failed %d consecutive times — rolling back to LKG", svc.Name, failures)
			if d.rollbackToLKG(ctx) {
				delete(d.healthFailures, svc.Name)
			}
			break
		}

		log.Printf("Health check failed for %s (%s) %d/%d: %v — restarting",
			svc.Name, result.Type, failures, d.maxFailures, result.Error)

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
	primary, err := d.configManager.LoadStack(d.stackFile)
	if err != nil {
		return nil, err
	}

	settings, sErr := config.LoadSettings(d.settingsPath)
	if sErr != nil {
		return primary, nil // settings not available — use primary stack only
	}

	// StackFiles() returns component stacks first and user repos last, so a
	// later merge overrides an earlier one.  Do NOT sort the whole list: that
	// grouping is the precedence order.  Merge the primary (main user) stack
	// last so customisations in it always win over component/repo defaults.
	files := settings.StackFiles()
	var merged *types.Stack
	for _, f := range files {
		if f == d.stackFile {
			continue // applied last as the primary stack
		}
		additional, aErr := d.configManager.LoadStack(f)
		if aErr != nil {
			log.Printf("Warning: skipping stack %s: %v", f, aErr)
			continue
		}
		if merged == nil {
			merged = additional
		} else {
			merged = d.configManager.MergeStacks(merged, additional)
		}
	}
	if merged == nil {
		return primary, nil
	}
	return d.configManager.MergeStacks(merged, primary), nil
}

// reload re-reads the settings file and updates the daemon's stack file path.
func (d *Daemon) reload(ctx context.Context) error {
	settings, err := config.LoadSettings(d.settingsPath)
	if err != nil {
		return fmt.Errorf("failed to reload settings: %w", err)
	}

	// Do not change d.stackFile here: it is the primary user stack (from
	// --stack/QM_STACK_FILE), not the first entry of StackFiles(), which may be
	// a component stack.
	d.reconciler.SetIngressConfig(settings.Ingress.Domain, settings.Ingress.TLS, settings.Ingress.ExcludeServices)
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
