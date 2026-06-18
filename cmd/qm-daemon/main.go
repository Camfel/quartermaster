package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"quartermaster/internal/daemon"
	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/git"
	"quartermaster/pkg/hardware"
	"quartermaster/pkg/metrics"
	"quartermaster/pkg/network"
	"quartermaster/pkg/reconciler"
	"quartermaster/pkg/secrets"
)

func main() {
	// ── CLI flags ────────────────────────────────────────────────────
	stackFileFlag := flag.String("stack", "/etc/quartermaster/stack.yaml",
		"Path to stack.yaml")
	configPathFlag := flag.String("config", config.DefaultSettingsPath(),
		"Path to settings.json")
	syncIntervalFlag := flag.Duration("sync-interval", 30*time.Second,
		"Reconciliation interval")
	flag.Parse()

	stackFile := *stackFileFlag
	if env := os.Getenv("QM_STACK_FILE"); env != "" {
		stackFile = env
	}

	settingsPath := *configPathFlag
	if env := os.Getenv("QM_CONFIG_FILE"); env != "" {
		settingsPath = env
	}

	log.Printf("Quartermaster Daemon starting (stack: %s)", stackFile)

	// ── 1. Load settings ─────────────────────────────────────────────
	settings, err := config.LoadSettings(settingsPath)
	if err != nil {
		log.Fatalf("Failed to load settings: %v", err)
	}

	syncInterval := *syncIntervalFlag
	if settings.SyncInterval != "" {
		if d, err := time.ParseDuration(settings.SyncInterval); err == nil {
			syncInterval = d
		}
	}

	// ── 2. Initialise components ─────────────────────────────────────
	cm := config.NewConfigManager()

	cc, err := cri.NewContainerdClient(settings.ContainerdSocket, settings.Namespace)
	if err != nil {
		log.Fatalf("Failed to initialize CRI client: %v", err)
	}

	secretMgr := secrets.NewManager(settings.SecretsDir)
	if key, err := secrets.LoadOrCreateKey(settings.MasterKeyPath); err != nil {
		log.Printf("Warning: failed to load master key: %v (secrets will be read as plaintext)", err)
	} else {
		secretMgr.WithEncryption(key)
		log.Println("Secret encryption enabled")
	}

	netMgr, err := network.NewBridgeManager()
	if err != nil {
		log.Fatalf("Failed to initialize bridge manager: %v", err)
	}
	cc.WithSecrets(secretMgr).WithNetManager(netMgr).WithHardwareDetector(hardware.NewDetector())

	// Set up the qm0 bridge for non-host-networked containers.
	if err := netMgr.Setup(); err != nil {
		log.Printf("Warning: bridge setup failed: %v (non-host containers will lack outbound internet)", err)
	}
	// Recover bridge IPs from previous daemon run (survives restart).
	if err := netMgr.Recover(); err != nil {
		log.Printf("Warning: bridge IP recovery failed: %v", err)
	}
	// Start the in-process DNS forwarder on the bridge gateway.
	if err := netMgr.StartDNS(); err != nil {
		log.Printf("Warning: DNS forwarder failed to start: %v", err)
	}

	// Recover VPN routing after a daemon restart.  Scan the stack for
	// the VPN gateway (first vpn service with no running vpn dependency).
	if stack, err := cm.LoadStack(stackFile); err == nil {
		for _, svc := range stack.Spec.Services {
			if network.NormaliseProfile(svc.Network) != network.ProfileVPN {
				continue
			}
			isGateway := true
			for _, dep := range svc.DependsOn {
				if netMgr.LookupIP(dep) != nil {
					isGateway = false
					break
				}
			}
			if isGateway {
				if err := netMgr.RecoverVPNRouting(svc.Name); err != nil {
					log.Printf("Warning: VPN routing recovery for %s: %v", svc.Name, err)
				}
				break
			}
		}
	}

	r := reconciler.NewReconciler(cc, cm)
	r.SetNetManager(netMgr)
	r.SetIngressConfig(settings.Ingress.Domain, settings.Ingress.TLS)

	// Channel for git watchers to signal changes.
	gitChangeCh := make(chan struct{}, 1)

	// ── 2a. Git watchers ─────────────────────────────────────────
	var watchers []*git.Watcher
	for _, repo := range settings.Repos {
		repo := repo
		w := git.NewWatcher(
			repo.URL, repo.Branch, repo.Token,
			repo.SSHKeyPath, repo.SSHKnownHostsPath, repo.LocalPath,
			repo.PollDuration(), repo.CooldownDuration(),
			func(ctx context.Context, hash string) {
				log.Printf("Git change detected (repo %s, hash %s)", repo.URL, hash)
				select {
				case gitChangeCh <- struct{}{}:
				default:
				}
			},
		)
		watchers = append(watchers, w)
		log.Printf("Watching repo: %s (branch: %s)", repo.URL, repo.Branch)
	}

	// ── 2b. Metrics ──────────────────────────────────────────────
	var m *metrics.Metrics
	metricsAddr := ""
	if settings.Metrics.Enabled && settings.Metrics.ListenAddr != "" {
		m = metrics.New()
		metricsAddr = settings.Metrics.ListenAddr
		log.Printf("Metrics enabled on %s", metricsAddr)
	}

	// ── 3. Context + signal handling ─────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal: %v. Shutting down...", sig)
		cancel()
	}()

	// ── 4. Run the daemon ────────────────────────────────────────────
	d := daemon.NewDaemon(
		r, cc, cm, netMgr,
		stackFile,
		settings.SocketPath,
		settingsPath,
		settings.LKGPath,
		syncInterval,
		settings.MaxHealthFailures,
		watchers,
		m,
		metricsAddr,
		gitChangeCh,
	)

	if err := d.Run(ctx); err != nil {
		log.Fatalf("Daemon exited with error: %v", err)
	}

	log.Println("Quartermaster Daemon stopped gracefully.")
}
