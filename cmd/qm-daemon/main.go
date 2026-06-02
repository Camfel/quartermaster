package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"quartermaster/internal/daemon"
	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/git"
	"quartermaster/pkg/hardware"
	"quartermaster/pkg/network"
	"quartermaster/pkg/reconciler"
	"quartermaster/pkg/secrets"
)

func main() {
	// ── CLI flags ────────────────────────────────────────────────────
	configPathFlag := flag.String("config", config.DefaultSettingsPath(),
		"Path to settings.json")
	flag.Parse()

	settingsPath := *configPathFlag
	// Env var overrides the flag for scripting convenience.
	if env := os.Getenv("QM_CONFIG_FILE"); env != "" {
		settingsPath = env
	}

	log.Printf("Quartermaster Daemon starting (config: %s)", settingsPath)

	// ── 1. Load settings ─────────────────────────────────────────────
	settings, err := config.LoadSettings(settingsPath)
	if err != nil {
		log.Fatalf("Failed to load settings: %v", err)
	}

	// Allow env vars to override key settings (convenience + testing).
	applyEnvOverrides(settings)

	// Validate settings (skip for static-file mode, which has no repos).
	if len(settings.Repos) > 0 {
		if err := settings.Validate(); err != nil {
			log.Fatalf("Invalid settings: %v", err)
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

	hwDetector := hardware.NewDetector()
	netMgr := network.NewManager()
	cc.WithSecrets(secretMgr).WithHardwareDetector(hwDetector).WithNetworkManager(netMgr)

	log.Printf("GPU available: %v", hwDetector.HasGPU())

	reconciler := reconciler.NewReconciler(cc, cm)
	reconciler.SetNetworkManager(netMgr)

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

	// ── 4. Create watchers for each repo and enabled component ──────
	var watchers []*git.Watcher
	var stackFiles []string

	// Components first — user repos override on name conflicts.
	for _, repo := range settings.ExpandComponents() {
		w := git.NewWatcher(
			repo.URL, repo.Branch, repo.Token,
			repo.SSHKeyPath, repo.SSHKnownHostsPath,
			repo.LocalPath,
			repo.PollDuration(), repo.CooldownDuration(),
			nil,
		)
		watchers = append(watchers, w)
		stackFiles = append(stackFiles, repo.StackPath())
		log.Printf("Component watcher: %s/%s → %s", repo.URL, repo.StackFile, repo.StackPath())
	}

	for _, repo := range settings.Repos {
		w := git.NewWatcher(
			repo.URL,
			repo.Branch,
			repo.Token,
			repo.SSHKeyPath,
			repo.SSHKnownHostsPath,
			repo.LocalPath,
			repo.PollDuration(),
			repo.CooldownDuration(),
			nil, // wired by daemon
		)
		watchers = append(watchers, w)
		stackFiles = append(stackFiles, repo.StackPath())

		log.Printf("Repo watcher: %s (%s) → %s", repo.URL, repo.Branch, repo.StackPath())
	}

	// Fallback: if no repos, use static file mode
	if len(stackFiles) == 0 {
		staticPath := "/etc/quartermaster/stack.yaml"
		if env := os.Getenv("QM_CONFIG_PATH"); env != "" {
			staticPath = env
		}
		stackFiles = append(stackFiles, staticPath)
		log.Printf("No repos configured — static-file mode (%s)", staticPath)
	}

	// ── 5. Run the daemon ────────────────────────────────────────────
	d := daemon.NewDaemon(
		reconciler, cc, cm,
		stackFiles,
		settings.LKGPath,
		settings.SocketPath,
		settingsPath,
		settings.SyncDuration(),
		settings.HealthCheckDuration(),
		settings.MaxHealthFailures,
		watchers,
	)

	if err := d.Run(ctx); err != nil {
		log.Fatalf("Daemon exited with error: %v", err)
	}

	log.Println("Quartermaster Daemon stopped gracefully.")
}

// ── Env var overrides (backward compat + integration tests) ──────────────

func applyEnvOverrides(s *config.Settings) {
	if v := os.Getenv("QM_CONTAINERD_SOCKET"); v != "" {
		s.ContainerdSocket = v
	}
	if v := os.Getenv("QM_NAMESPACE"); v != "" {
		s.Namespace = v
	}
	if v := os.Getenv("QM_SYNC_INTERVAL"); v != "" {
		s.SyncInterval = v
	}
	if v := os.Getenv("QM_HEALTH_CHECK_INTERVAL"); v != "" {
		s.HealthCheckInterval = v
	}
	if v, err := strconv.Atoi(os.Getenv("QM_MAX_HEALTH_FAILURES")); err == nil && v > 0 {
		s.MaxHealthFailures = v
	}
	if v := os.Getenv("QM_LKG_PATH"); v != "" {
		s.LKGPath = v
	}
	if v := os.Getenv("QM_SECRETS_DIR"); v != "" {
		s.SecretsDir = v
	}
	if v := os.Getenv("QM_MASTER_KEY_PATH"); v != "" {
		s.MasterKeyPath = v
	}
	if v := os.Getenv("QM_SOCKET_PATH"); v != "" {
		s.SocketPath = v
	}

	// Git repo env overrides (for single-repo backward compat / testing).
	gitURL := os.Getenv("QM_GIT_REPO_URL")
	gitBranch := os.Getenv("QM_GIT_BRANCH")
	gitLocal := os.Getenv("QM_GIT_LOCAL_PATH")
	gitPoll := os.Getenv("QM_GIT_POLL_INTERVAL")
	gitCooldown := os.Getenv("QM_GIT_COOLDOWN")
	gitToken := os.Getenv("QM_GIT_TOKEN")
	gitSSHKey := os.Getenv("QM_GIT_SSH_KEY")
	gitSSHKnownHosts := os.Getenv("QM_GIT_SSH_KNOWN_HOSTS")

	if gitURL != "" {
		// Auto-prefix bare filesystem paths for convenience.
		if strings.HasPrefix(gitURL, "/") && !strings.HasPrefix(gitURL, "file://") {
			gitURL = "file://" + gitURL
		}
		if gitBranch == "" {
			gitBranch = "main"
		}
		if gitLocal == "" {
			gitLocal = "/var/lib/quartermaster/git-repo"
		}
		if gitPoll == "" {
			gitPoll = "30s"
		}
		if gitCooldown == "" {
			gitCooldown = "30s"
		}
		// Prepend so env-var repo takes priority over file-configured repos.
		s.Repos = append([]config.RepoConfig{{
			URL:               gitURL,
			Branch:            gitBranch,
			Token:             gitToken,
			SSHKeyPath:        gitSSHKey,
			SSHKnownHostsPath: gitSSHKnownHosts,
			LocalPath:         gitLocal,
			StackFile:         "stack.yaml",
			PollInterval:      gitPoll,
			Cooldown:          gitCooldown,
		}}, s.Repos...)
	}

	// Static config path override (backward compat).
	if v := os.Getenv("QM_CONFIG_PATH"); v != "" && gitURL == "" && len(s.Repos) == 0 {
		// Only used in static-file mode (no repos configured).
	}
}

// envDuration kept for backwards compat in applyEnvOverrides if needed later.
func envDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
