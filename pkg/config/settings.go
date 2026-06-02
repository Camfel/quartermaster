package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Settings file types ──────────────────────────────────────────────────

// Settings is the top-level daemon configuration loaded from settings.json.
type Settings struct {
	// Path to the containerd gRPC socket.
	ContainerdSocket string `json:"containerd_socket"`

	// containerd namespace used to isolate Quartermaster containers.
	Namespace string `json:"namespace"`

	// Interval between periodic reconciliation passes (e.g. "1m", "30s").
	SyncInterval string `json:"sync_interval"`

	// Interval between health-check probe runs.
	HealthCheckInterval string `json:"health_check_interval"`

	// Number of consecutive health-check failures that trigger LKG rollback.
	MaxHealthFailures int `json:"max_health_failures"`

	// Path where the Last Known Good manifest snapshot is persisted.
	LKGPath string `json:"lkg_path"`

	// Directory containing encrypted secrets.
	SecretsDir string `json:"secrets_dir"`

	// Path to the NaCl secretbox master key (auto-created if missing).
	MasterKeyPath string `json:"master_key_path"`

	// Path where the daemon's Unix socket for the status API is created.
	SocketPath string `json:"socket_path"`

	// Git repositories to watch. Each repo must contain at least one
	// stack file (default "stack.yaml" at the repo root).
	Repos []RepoConfig `json:"repos"`

	// Optional curated components managed by a central repository.
	// When a component is enabled, QM pulls its stack from the components
	// repo and merges it with user stacks (user services win on name conflict).
	ComponentsRepo string           `json:"components_repo"`
	Components     ComponentsConfig `json:"components"`
}

// ComponentsConfig maps component names (e.g. "vpn", "ingress") to their
// configuration.  Only enabled components are fetched.
type ComponentsConfig map[string]ComponentConfig

// ComponentConfig describes a single optional component.
type ComponentConfig struct {
	Enabled bool                   `json:"enabled"`
	Version string                 `json:"version"`
	Config  map[string]interface{} `json:"config"`
}

// RepoConfig describes a single Git repository Quartermaster watches.
type RepoConfig struct {
	// HTTPS URL of the remote repository (required).
	URL string `json:"url"`

	// Branch to track (default "main").
	Branch string `json:"branch"`

	// Personal Access Token for HTTPS repositories (optional).
	Token string `json:"token"`

	// Path to an SSH private key for git@ SSH-style URLs (recommended
	// over tokens).  Takes precedence over Token when both are set.
	SSHKeyPath string `json:"ssh_key_path"`

	// Path to an OpenSSH known_hosts file for host-key verification.
	// Defaults to ~/.ssh/known_hosts when ssh_key_path is configured.
	SSHKnownHostsPath string `json:"ssh_known_hosts_path"`

	// Local directory where the repo will be cloned.
	LocalPath string `json:"local_path"`

	// Path to the stack file relative to the repo root (default "stack.yaml").
	StackFile string `json:"stack_file"`

	// How often to poll the remote for new commits (e.g. "30s").
	PollInterval string `json:"poll_interval"`

	// Minimum time between two watcher-triggered reconciliations (anti-storm).
	Cooldown string `json:"cooldown"`
}

// ── Loading ──────────────────────────────────────────────────────────────

// LoadSettings reads and validates settings from a JSON file.  Missing or
// empty fields are back-filled with sensible defaults.  If the file does
// not exist or is empty, a default settings struct is returned (useful
// when the caller relies entirely on env-var overrides).
func LoadSettings(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s := &Settings{}
			s.applyDefaults()
			return s, nil
		}
		return nil, fmt.Errorf("reading settings file %s: %w", path, err)
	}

	// Treat an empty file the same as a missing file.
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "{}" {
		s := &Settings{}
		s.applyDefaults()
		return s, nil
	}

	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing settings file %s: %w", path, err)
	}

	s.applyDefaults()
	return &s, nil
}

// DefaultSettingsPath returns the default location of the settings file.
// When running as root, uses the system-wide path; otherwise per-user.
func DefaultSettingsPath() string {
	if p := os.Getenv("QM_CONFIG_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "/root" {
		return "/etc/quartermaster/settings.json"
	}
	return filepath.Join(home, ".qm", "settings.json")
}

// ── Defaults ─────────────────────────────────────────────────────────────

func (s *Settings) applyDefaults() {
	if s.ContainerdSocket == "" {
		s.ContainerdSocket = "/run/containerd/containerd.sock"
	}
	if s.Namespace == "" {
		s.Namespace = "quartermaster"
	}
	if s.SyncInterval == "" {
		s.SyncInterval = "1m"
	}
	if s.HealthCheckInterval == "" {
		s.HealthCheckInterval = "30s"
	}
	if s.MaxHealthFailures <= 0 {
		s.MaxHealthFailures = 3
	}
	if s.LKGPath == "" {
		s.LKGPath = "/var/lib/quartermaster/lkg-stack.yaml"
	}
	if s.SecretsDir == "" {
		s.SecretsDir = "/etc/quartermaster/secrets"
	}
	if s.MasterKeyPath == "" {
		s.MasterKeyPath = "/etc/quartermaster/master.key"
	}
	if s.SocketPath == "" {
		s.SocketPath = "/run/quartermaster"
	}
	if s.ComponentsRepo == "" {
		s.ComponentsRepo = "https://github.com/Camfel/quartermaster-components"
	}

	for i := range s.Repos {
		r := &s.Repos[i]
		if r.Branch == "" {
			r.Branch = "main"
		}
		if r.StackFile == "" {
			r.StackFile = "stack.yaml"
		}
		if r.PollInterval == "" {
			r.PollInterval = "30s"
		}
		if r.Cooldown == "" {
			r.Cooldown = "30s"
		}
		// Default SSH known_hosts to ~/.ssh/known_hosts when key is configured.
		if r.SSHKeyPath != "" && r.SSHKnownHostsPath == "" {
			home, _ := os.UserHomeDir()
			r.SSHKnownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
		}
	}
}

// ── Duration helpers ─────────────────────────────────────────────────────

// SyncDuration parses SyncInterval and returns a time.Duration.
// Returns 1 minute on parse failure.
func (s *Settings) SyncDuration() time.Duration {
	d, err := time.ParseDuration(s.SyncInterval)
	if err != nil {
		return 1 * time.Minute
	}
	return d
}

// HealthCheckDuration parses HealthCheckInterval and returns a time.Duration.
// Returns 30 seconds on parse failure.
func (s *Settings) HealthCheckDuration() time.Duration {
	d, err := time.ParseDuration(s.HealthCheckInterval)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// ── Repo helpers ─────────────────────────────────────────────────────────

// PollDuration parses r.PollInterval and returns a time.Duration.
func (r *RepoConfig) PollDuration() time.Duration {
	d, err := time.ParseDuration(r.PollInterval)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// CooldownDuration parses r.Cooldown and returns a time.Duration.
func (r *RepoConfig) CooldownDuration() time.Duration {
	d, err := time.ParseDuration(r.Cooldown)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// StackPath returns the full local path to the repo's stack file.
func (r *RepoConfig) StackPath() string {
	return filepath.Join(r.LocalPath, r.StackFile)
}

// ── Stack merging ────────────────────────────────────────────────────────

// StackFiles returns a deduplicated, ordered slice of stack-file paths from
// all configured repos AND enabled components.  Component stacks come first
// so user repos can override them on service name conflicts.
func (s *Settings) StackFiles() []string {
	seen := make(map[string]bool)
	var paths []string

	// Components first (user repos override on name conflict).
	for _, r := range s.ExpandComponents() {
		p := r.StackPath()
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	// Then user repos.
	for _, r := range s.Repos {
		p := r.StackPath()
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths
}

// ── Components ───────────────────────────────────────────────────────────

// ExpandComponents returns a list of RepoConfig entries for each enabled
// component.  All components share a single clone of the components repo;
// each component references a different stack_file path within that clone.
func (s *Settings) ExpandComponents() []RepoConfig {
	if s.ComponentsRepo == "" || len(s.Components) == 0 {
		return nil
	}

	var repos []RepoConfig
	localBase := filepath.Join(filepath.Dir(s.LKGPath), "components")

	for name, c := range s.Components {
		if !c.Enabled {
			continue
		}
		version := c.Version
		if version == "" {
			version = "latest"
		}

		repos = append(repos, RepoConfig{
			URL:          s.ComponentsRepo,
			Branch:       "main",
			LocalPath:    localBase,                                              // single clone
			StackFile:    filepath.Join(name, version, "stack.yaml"),            // path inside repo
			PollInterval: "5m",
			Cooldown:     "1m",
		})
	}

	return repos
}

// ── Validation ────────────────────────────────────────────────────────────

// Validate checks all settings and returns the first error encountered.
func (s *Settings) Validate() error {
	if s.ContainerdSocket == "" {
		return fmt.Errorf("containerd_socket is required")
	}
	if s.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if _, err := time.ParseDuration(s.SyncInterval); err != nil {
		return fmt.Errorf("sync_interval: %w", err)
	}
	if _, err := time.ParseDuration(s.HealthCheckInterval); err != nil {
		return fmt.Errorf("health_check_interval: %w", err)
	}
	if s.MaxHealthFailures < 1 {
		return fmt.Errorf("max_health_failures must be >= 1")
	}

	if len(s.Repos) == 0 && len(s.ExpandComponents()) == 0 {
		return fmt.Errorf("at least one repo or enabled component is required (or set QM_CONFIG_PATH for static-file mode)")
	}

	seenLocal := make(map[string]bool)
	for i := range s.Repos {
		if err := s.Repos[i].Validate(); err != nil {
			return fmt.Errorf("repos[%d]: %w", i, err)
		}
		// Check for overlapping local_paths.
		lp := filepath.Clean(s.Repos[i].LocalPath)
		if seenLocal[lp] {
			return fmt.Errorf("repos[%d]: local_path %q conflicts with another repo", i, lp)
		}
		seenLocal[lp] = true
	}

	return nil
}

// Validate checks a single repo config and returns an error if required
// fields are missing or inconsistent.
func (r *RepoConfig) Validate() error {
	if r.URL == "" {
		return fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(r.URL, "https://") &&
		!strings.HasPrefix(r.URL, "git@") &&
		!strings.HasPrefix(r.URL, "ssh://") &&
		!strings.HasPrefix(r.URL, "file://") {
		return fmt.Errorf("url must start with https://, git@, ssh://, or file://: %q", r.URL)
	}
	if r.LocalPath == "" {
		return fmt.Errorf("local_path is required")
	}
	if _, err := time.ParseDuration(r.PollInterval); err != nil {
		return fmt.Errorf("poll_interval: %w", err)
	}
	if _, err := time.ParseDuration(r.Cooldown); err != nil {
		return fmt.Errorf("cooldown: %w", err)
	}
	return nil
}

// ── Serialisation helpers ────────────────────────────────────────────────

// WriteDefault writes a well-commented default settings file to path.
// Existing files are not overwritten.
func WriteDefault(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultSettingsJSON), 0644)
}

const defaultSettingsJSON = `{
  "containerd_socket": "/run/containerd/containerd.sock",
  "namespace": "quartermaster",
  "sync_interval": "1m",
  "health_check_interval": "30s",
  "max_health_failures": 3,
  "lkg_path": "/var/lib/quartermaster/lkg-stack.yaml",
  "secrets_dir": "/etc/quartermaster/secrets",
  "master_key_path": "/etc/quartermaster/master.key",
  "repos": [
    {
      "url": "git@github.com:example/quartermaster-stacks.git",
      "branch": "main",
      "token": "",
      "ssh_key_path": "/etc/quartermaster/keys/repo-deploy-key",
      "ssh_known_hosts_path": "/etc/quartermaster/keys/known_hosts",
      "local_path": "/var/lib/quartermaster/repos/main",
      "stack_file": "stack.yaml",
      "poll_interval": "30s",
      "cooldown": "30s"
    }
  ],
  "components_repo": "https://github.com/quartermaster-hq/qm-components.git",
  "components": {
    "vpn": {
      "enabled": false,
      "version": "v1.0",
      "config": {
        "provider": "wireguard",
        "secret_ref": "vpn-config"
      }
    },
    "ingress": {
      "enabled": false,
      "version": "v0.1",
      "config": {
        "http_port": 80,
        "https_port": 443
      }
    }
  }
}
` + ""

// sanitiseForLog redacts tokens from a repo URL for safe logging.
func sanitiseForLog(s string) string {
	// tokens appear as "token@host" or in query params; just strip the userinfo
	if idx := strings.Index(s, "://"); idx != -1 {
		schemeEnd := idx + 3
		if atIdx := strings.Index(s[schemeEnd:], "@"); atIdx != -1 {
			return s[:schemeEnd] + "***" + s[schemeEnd+atIdx:]
		}
	}
	return s
}
