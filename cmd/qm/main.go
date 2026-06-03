package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/secrets"
)

const (
	version              = "0.3.0"
	defaultSecretsDir    = "/etc/quartermaster/secrets"
	defaultMasterKeyPath = "/etc/quartermaster/master.key"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "up":
		cmdUp(os.Args[2:])

	case "validate":
		cmdValidate(os.Args[2:])

	case "repo":
		cmdRepo(os.Args[2:])

	case "status":
		cmdStatus()

	case "create-secret":
		cmdCreateSecret(os.Args[2:])

	case "list-secrets":
		cmdListSecrets()

	case "enable":
		cmdEnable(os.Args[2:])

	case "disable":
		cmdDisable(os.Args[2:])

	case "components":
		cmdComponents(os.Args[2:])

	case "configmap":
		cmdConfigMap(os.Args[2:])

	case "version":
		fmt.Printf("Quartermaster CLI v%s\n", version)

	default:
		fmt.Printf("Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Quartermaster - GitOps-native container orchestrator for homelabs")
	fmt.Println()
	fmt.Println("Usage: qm <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  up <file>             Validate and display a stack configuration")
	fmt.Println("  validate <file>       Validate a stack file and report any errors")
	fmt.Println("  repo add              Add a Git repository to the daemon config")
	fmt.Println("  repo list             List configured repositories")
	fmt.Println("  status                Show daemon and container status")
	fmt.Println("  create-secret <name>  Create or update an encrypted secret (reads from stdin)")
	fmt.Println("  list-secrets          List all stored secret names (values not shown)")
	fmt.Println("  enable <name>         Enable a component from the catalog")
	fmt.Println("  disable <name>        Disable a component")
	fmt.Println("  components list       List components and their enabled state")
	fmt.Println("  configmap set <name> <key=value>...  Create or update a ConfigMap")
	fmt.Println("  version               Show the Quartermaster version")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  qm validate ./stack.yaml")
	fmt.Println("  qm up ./samples/media-stack.yaml")
	fmt.Println("  qm repo add")
	fmt.Println("  qm enable dashboard")
	fmt.Println("  qm disable dashboard")
	fmt.Println("  qm components list")
	fmt.Println("  echo 'mypassword' | qm create-secret db-password")
	fmt.Println("  qm list-secrets")
	fmt.Println("  qm configmap set vpn-config provider=protonvpn type=wireguard")
}

// ── repo command ─────────────────────────────────────────────────────────

func cmdRepo(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: qm repo <add|list>")
		os.Exit(1)
	}

	switch args[0] {
	case "add":
		cmdRepoAdd(args[1:])
	case "list":
		cmdRepoList()
	default:
		fmt.Printf("Unknown repo command: %s\n", args[0])
		fmt.Println("Usage: qm repo <add|list>")
		os.Exit(1)
	}
}

func cmdRepoAdd(args []string) {
	settingsPath := settingsFilePath()
	settings, err := loadOrCreateSettings(settingsPath)
	if err != nil {
		fmt.Printf("Error loading settings: %v\n", err)
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	// ── 1. Repo URL ─────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║           Add a Git repository to Quartermaster       ║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("URL examples:")
	fmt.Println("  HTTPS:  https://github.com/you/quartermaster-stacks.git")
	fmt.Println("  SSH:    git@github.com:you/quartermaster-stacks.git")
	fmt.Println()

	repoURL := prompt(reader, "Repository URL", "")
	if repoURL == "" {
		fmt.Println("Error: URL is required")
		os.Exit(1)
	}

	isSSH := strings.HasPrefix(repoURL, "git@") || strings.HasPrefix(repoURL, "ssh://")
	isHTTPS := strings.HasPrefix(repoURL, "https://")

	if !isSSH && !isHTTPS {
		fmt.Printf("Error: URL must start with https:// or git@ (got: %s)\n", repoURL)
		os.Exit(1)
	}

	// ── 2. Branch ───────────────────────────────────────────────────
	branch := prompt(reader, "Branch", "main")

	// ── 3. Authentication ───────────────────────────────────────────
	var token, sshKeyPath, sshKnownHosts string

	if isSSH {
		fmt.Println()
		fmt.Println("SSH authentication:")
		defaultKey := filepath.Join(os.Getenv("HOME"), ".ssh", "id_ed25519")
		sshKeyPath = prompt(reader, "  SSH private key path", defaultKey)

		if _, err := os.Stat(sshKeyPath); os.IsNotExist(err) {
			fmt.Printf("  ⚠ Key not found at %s\n", sshKeyPath)
			generate := prompt(reader, "  Generate a new Ed25519 key? (y/N)", "n")
			if strings.ToLower(generate) == "y" {
				sshKeyPath = filepath.Join("/etc/quartermaster/keys",
					"deploy-"+strings.ReplaceAll(repoURLToSlug(repoURL), "/", "-"))
				if err := generateSSHKey(sshKeyPath, repoURL); err != nil {
					fmt.Printf("Error generating key: %v\n", err)
				} else {
					fmt.Printf("  ✓ Key generated: %s\n", sshKeyPath)
				}
			}
		}

		defaultKH := filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
		sshKnownHosts = prompt(reader, "  known_hosts file", defaultKH)

		// Seed known_hosts with github.com if the file is new or empty.
		seedKnownHosts(sshKnownHosts)
	} else {
		fmt.Println()
		fmt.Println("HTTPS authentication (Personal Access Token):")
		fmt.Println("  GitHub: Settings → Developer settings → Personal access tokens → Tokens (classic)")
		fmt.Println("  Scopes needed: repo (private repos) or public_repo (public only)")
		fmt.Println()
		token = promptMasked(reader, "  Token")
		if token == "" {
			fmt.Println("  ⚠ No token provided — repo must be public")
		}
	}

	// ── 4. Local path ───────────────────────────────────────────────
	defaultLocal := "/var/lib/quartermaster/repos/" + repoURLToSlug(repoURL)
	localPath := prompt(reader, "Local clone path", defaultLocal)

	// ── 5. Stack file ───────────────────────────────────────────────
	stackFile := prompt(reader, "Stack file (relative to repo root)", "stack.yaml")

	// ── 6. Polling ──────────────────────────────────────────────────
	pollInterval := prompt(reader, "Poll interval", "30s")
	cooldown := prompt(reader, "Cooldown", "30s")

	// ── 7. Test connection ──────────────────────────────────────────
	fmt.Println()
	fmt.Print("Testing connection... ")
	if err := testGitConnection(repoURL, branch, token, sshKeyPath, sshKnownHosts); err != nil {
		fmt.Printf("✗\n  %v\n", err)
		fmt.Println("  Continuing anyway — you can fix this later in settings.json")
	} else {
		fmt.Println("✓")
	}

	// ── 8. Add to settings ──────────────────────────────────────────
	repo := config.RepoConfig{
		URL:               repoURL,
		Branch:            branch,
		Token:             token,
		SSHKeyPath:        sshKeyPath,
		SSHKnownHostsPath: sshKnownHosts,
		LocalPath:         localPath,
		StackFile:         stackFile,
		PollInterval:      pollInterval,
		Cooldown:          cooldown,
	}

	settings.Repos = append(settings.Repos, repo)

	if err := writeSettings(settingsPath, settings); err != nil {
		fmt.Printf("Error saving settings: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Printf("✓ Repository added to %s\n", settingsPath)
	fmt.Printf("  URL:      %s\n", repoURL)
	fmt.Printf("  Branch:   %s\n", branch)
	fmt.Printf("  Local:    %s\n", localPath)
	fmt.Println()
	fmt.Println("Run the daemon to start reconciling:")
	fmt.Println("  sudo qm-daemon")
}

func cmdRepoList() {
	settingsPath := settingsFilePath()
	settings, err := loadOrCreateSettings(settingsPath)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	if len(settings.Repos) == 0 {
		fmt.Println("No repositories configured.")
		fmt.Println("Add one with: qm repo add")
		return
	}

	fmt.Printf("Configured repositories (%s):\n\n", settingsPath)
	for i, r := range settings.Repos {
		auth := "none"
		if r.SSHKeyPath != "" {
			auth = fmt.Sprintf("SSH (%s)", r.SSHKeyPath)
		} else if r.Token != "" {
			auth = "HTTPS (token)"
		}
		fmt.Printf("  [%d] %s\n", i+1, r.URL)
		fmt.Printf("      Branch: %-10s  Auth: %s\n", r.Branch, auth)
		fmt.Printf("      Local:  %s\n", r.LocalPath)
		fmt.Printf("      Stack:  %-15s  Poll: %-6s  Cooldown: %s\n",
			r.StackFile, r.PollInterval, r.Cooldown)
		fmt.Println()
	}

	if err := settings.Validate(); err != nil {
		fmt.Printf("⚠ Configuration warning: %v\n", err)
	}
}

func cmdStatus() {
	socketPath := "/run/quartermaster/daemon.sock"
	if p := os.Getenv("QM_SOCKET_PATH"); p != "" {
		socketPath = p + "/daemon.sock"
	}

	resp, err := httpGet(socketPath, "/v1/status")
	if err != nil {
		fmt.Printf("Error connecting to daemon: %v\n", err)
		fmt.Println("Is qm-daemon running?")
		os.Exit(1)
	}

	var s struct {
		Version             string `json:"version"`
		Uptime              string `json:"uptime"`
		LastReconcile       string `json:"last_reconcile"`
		LastReconcileError  string `json:"last_reconcile_error"`
		ReconcileCount      int64  `json:"reconcile_count"`
		Containers          []struct {
			Name    string `json:"name"`
			Image   string `json:"image"`
			Running bool   `json:"running"`
			PID     uint32 `json:"pid"`
			Healthy *bool  `json:"healthy"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(resp, &s); err != nil {
		fmt.Printf("Error parsing status: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Quartermaster Daemon %s  │  uptime %s  │  %d reconcile(s)\n\n",
		s.Version, s.Uptime, s.ReconcileCount)

	if s.LastReconcileError != "" {
		fmt.Printf("⚠ Last reconcile error: %s\n\n", s.LastReconcileError)
	}

	if len(s.Containers) == 0 {
		fmt.Println("No containers managed.")
		return
	}

	fmt.Printf("%-24s %-12s %-10s %s\n", "NAME", "STATUS", "HEALTH", "IMAGE")
	fmt.Println(strings.Repeat("-", 75))

	for _, c := range s.Containers {
		status := "stopped"
		if c.Running {
			status = fmt.Sprintf("running (pid %d)", c.PID)
		}

		health := "-"
		if c.Healthy != nil {
			if *c.Healthy {
				health = "✓ healthy"
			} else {
				health = "✗ unhealthy"
			}
		}

		fmt.Printf("%-24s %-12s %-10s %s\n", c.Name, status, health, c.Image)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────

// httpGet performs an HTTP GET over a Unix socket and returns the body.
func httpGet(socketPath, urlPath string) ([]byte, error) {
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
	resp, err := client.Get("http://unix" + urlPath)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// httpPost performs an HTTP POST over a Unix socket and returns the body.
func httpPostJSON(socketPath, urlPath string, body []byte) ([]byte, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 10 * time.Second,
	}

	resp, err := client.Post("http://unix"+urlPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return data, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}

func httpPost(socketPath, urlPath string) ([]byte, error) {
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
	resp, err := client.Post("http://unix"+urlPath, "application/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func daemonSocketPath() string {
	socketPath := "/run/quartermaster/daemon.sock"
	if p := os.Getenv("QM_SOCKET_PATH"); p != "" {
		socketPath = p + "/daemon.sock"
	}
	return socketPath
}

func settingsFilePath() string {
	if p := os.Getenv("QM_CONFIG_FILE"); p != "" {
		return p
	}
	return config.DefaultSettingsPath()
}

func loadOrCreateSettings(path string) (*config.Settings, error) {
	s, err := config.LoadSettings(path)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func writeSettings(path string, s *config.Settings) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func prompt(r *bufio.Reader, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", label, defaultVal)
	} else {
		fmt.Printf("%s: ", label)
	}
	input, _ := r.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

func promptMasked(r *bufio.Reader, label string) string {
	// Use stty to disable echo if available.
	fmt.Printf("%s: ", label)
	disableEcho()
	input, _ := r.ReadString('\n')
	enableEcho()
	fmt.Println() // newline after masked input
	return strings.TrimSpace(input)
}

func disableEcho() {
	exec.Command("stty", "-F", "/dev/tty", "-echo").Run()
}

func enableEcho() {
	exec.Command("stty", "-F", "/dev/tty", "echo").Run()
}

func repoURLToSlug(url string) string {
	s := url
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "git@")
	s = strings.ReplaceAll(s, ":", "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.ReplaceAll(s, "/", "-")
	return s
}

func generateSSHKey(path, comment string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	cmd := exec.Command("ssh-keygen",
		"-t", "ed25519",
		"-C", "quartermaster-"+comment,
		"-N", "",
		"-f", path,
		"-q",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, string(out))
	}
	os.Chmod(path, 0600)
	os.Chmod(path+".pub", 0644)
	return nil
}

func seedKnownHosts(path string) {
	// Only seed if the file doesn't exist or is empty.
	if info, err := os.Stat(path); err == nil && info.Size() > 10 {
		return
	}
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0755)

	// Scan all key types — go-git may negotiate any of them.
	out, err := exec.Command("ssh-keyscan", "github.com").CombinedOutput()
	if err != nil {
		return // non-fatal
	}
	// Strip comment lines.
	var clean []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "#") && line != "" {
			clean = append(clean, line)
		}
	}
	if len(clean) > 0 {
		os.WriteFile(path, []byte(strings.Join(clean, "\n")+"\n"), 0644)
	}
}

func testGitConnection(url, branch, token, sshKeyPath, sshKnownHosts string) error {
	// Use git ls-remote for a lightweight connectivity check.
	args := []string{"ls-remote", "--heads", url, branch}
	cmd := exec.Command("git", args...)
	cmd.Env = os.Environ()

	if sshKeyPath != "" {
		kh := sshKnownHosts
		if kh == "" {
			kh = filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
		}
		cmd.Env = append(cmd.Env,
			"GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile="+kh+" -i "+sshKeyPath,
		)
	} else if token != "" {
		cmd.Env = append(cmd.Env,
			"GIT_ASKPASS=echo",
		)
		cmd.Stdin = strings.NewReader(token + "\n")
	}

	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ── Existing commands ────────────────────────────────────────────────────

func cmdUp(args []string) {
	if len(args) < 1 {
		fmt.Println("Error: 'up' requires a config file path")
		fmt.Println("Usage: qm up <file>")
		os.Exit(1)
	}

	configPath := args[0]
	cm := config.NewConfigManager()
	stack, err := cm.LoadStack(configPath)
	if err != nil {
		fmt.Printf("Error loading stack: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Stack: %s (version %s)\n", stack.Metadata.Name, stack.Version)
	fmt.Printf("Services: %d\n", len(stack.Spec.Services))
	fmt.Println()

	for _, svc := range stack.Spec.Services {
		fmt.Printf("  ✓ %s\n", svc.Name)
		fmt.Printf("    Image: %s\n", svc.Image)
		if svc.RestartPolicy != "" {
			fmt.Printf("    Restart: %s\n", svc.RestartPolicy)
		}
		if len(svc.Ports) > 0 {
			fmt.Printf("    Ports:\n")
			for _, p := range svc.Ports {
				fmt.Printf("      %d:%d\n", p.Host, p.Container)
			}
		}
		if len(svc.Volumes) > 0 {
			fmt.Printf("    Volumes:\n")
			for _, v := range svc.Volumes {
				fmt.Printf("      %s -> %s (%s)\n", v.Source, v.Target, v.Type)
			}
		}
		if len(svc.Env) > 0 {
			fmt.Printf("    Environment:\n")
			for _, e := range svc.Env {
				val := e.Value
				if len(val) > 40 {
					val = val[:40] + "..."
				}
				fmt.Printf("      %s=%s\n", e.Name, val)
			}
		}
		if svc.Network != "" {
			fmt.Printf("    Network: %s\n", svc.Network)
		}
		if len(svc.DependsOn) > 0 {
			fmt.Printf("    Depends on: %v\n", svc.DependsOn)
		}
		if len(svc.Secrets) > 0 {
			fmt.Printf("    Secrets: %d\n", len(svc.Secrets))
		}
		if svc.Resources != nil && svc.Resources.GPU != nil {
			fmt.Printf("    GPU: %s (id=%s)\n", svc.Resources.GPU.Type, svc.Resources.GPU.ID)
		}
		fmt.Println()
	}
}

func cmdValidate(args []string) {
	if len(args) < 1 {
		fmt.Println("Error: 'validate' requires a config file path")
		fmt.Println("Usage: qm validate <file>")
		os.Exit(1)
	}

	configPath := args[0]
	cm := config.NewConfigManager()
	stack, err := cm.LoadStack(configPath)
	if err != nil {
		fmt.Printf("✗ Validation failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Configuration is valid\n")
	fmt.Printf("  Stack: %s\n", stack.Metadata.Name)
	fmt.Printf("  Services: %d\n", len(stack.Spec.Services))
	for _, svc := range stack.Spec.Services {
		deps := ""
		if len(svc.DependsOn) > 0 {
			deps = fmt.Sprintf(" (depends: %v)", svc.DependsOn)
		}
		fmt.Printf("    - %s: %s%s\n", svc.Name, svc.Image, deps)
	}
}

func cmdCreateSecret(args []string) {
	if len(args) < 1 {
		fmt.Println("Error: 'create-secret' requires a secret name")
		fmt.Println("Usage: echo '<value>' | qm create-secret <name>")
		os.Exit(1)
	}

	name := args[0]

	key, err := secrets.LoadOrCreateKey(defaultMasterKeyPath)
	if err != nil {
		fmt.Printf("Error loading master key: %v\n", err)
		os.Exit(1)
	}

	var input []byte
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			input = append(input, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	if len(input) > 0 && input[len(input)-1] == '\n' {
		input = input[:len(input)-1]
	}

	if len(input) == 0 {
		fmt.Println("Error: no input provided on stdin")
		os.Exit(1)
	}

	if err := os.MkdirAll(defaultSecretsDir, 0700); err != nil {
		fmt.Printf("Error creating secrets directory: %v\n", err)
		os.Exit(1)
	}

	mgr := secrets.NewManager(defaultSecretsDir).WithEncryption(key)
	if err := mgr.CreateEncrypted(name, input); err != nil {
		fmt.Printf("Error storing secret: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Secret %q created (encrypted)\n", name)
}

func cmdListSecrets() {
	if _, err := os.Stat(defaultMasterKeyPath); os.IsNotExist(err) {
		fmt.Println("No master key found. Create a secret first with: qm create-secret <name>")
		return
	}

	mgr := secrets.NewManager(defaultSecretsDir)
	names, err := mgr.ListNames()
	if err != nil {
		fmt.Printf("Error listing secrets: %v\n", err)
		os.Exit(1)
	}

	if len(names) == 0 {
		fmt.Println("No secrets stored.")
		return
	}

	fmt.Println("Secrets:")
	for _, name := range names {
		fmt.Printf("  - %s\n", name)
	}
}

// ── enable / disable / components ──────────────────────────────────────

func cmdEnable(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: qm enable <component-name>")
		os.Exit(1)
	}
	name := args[0]
	setComponentEnabled(name, true)
}

func cmdDisable(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: qm disable <component-name>")
		os.Exit(1)
	}
	name := args[0]
	setComponentEnabled(name, false)
}

func setComponentEnabled(name string, enabled bool) {
	path := settingsFilePath()
	s, err := loadOrCreateSettings(path)
	if err != nil {
		fmt.Printf("Error loading settings: %v\n", err)
		os.Exit(1)
	}

	if s.Components == nil {
		s.Components = make(config.ComponentsConfig)
	}

	c, exists := s.Components[name]
	if !exists {
		c = config.ComponentConfig{Version: "v1.0"}
	}
	c.Enabled = enabled
	s.Components[name] = c

	// Ensure components_repo is set if not already.
	if s.ComponentsRepo == "" {
		s.ComponentsRepo = "https://github.com/Camfel/quartermaster-components"
	}

	if err := writeSettings(path, s); err != nil {
		fmt.Printf("Error saving settings: %v\n", err)
		os.Exit(1)
	}

	action := "enabled"
	if !enabled {
		action = "disabled"
	}
	fmt.Printf("✓ Component %q %s\n", name, action)

	// Notify the daemon to reload.
	body, err := httpPost(daemonSocketPath(), "/v1/reload")
	if err != nil {
		fmt.Printf("Warning: could not notify daemon: %v\n", err)
		fmt.Println("Restart the daemon to apply changes.")
		return
	}
	fmt.Printf("Daemon notified: %s\n", string(body))
}

// ── configmap command ───────────────────────────────────────────────────

func cmdConfigMap(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: qm configmap <set> <name> [key=value...]")
		os.Exit(1)
	}

	sub := args[0]
	switch sub {
	case "set":
		if len(args) < 3 {
			fmt.Println("Usage: qm configmap set <name> <key=value>...")
			os.Exit(1)
		}
		name := args[1]
		data := make(map[string]string)
		for _, kv := range args[2:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				fmt.Printf("Invalid key=value pair: %s\n", kv)
				os.Exit(1)
			}
			data[parts[0]] = parts[1]
		}

		body, _ := json.Marshal(map[string]interface{}{"data": data})
		resp, err := httpPostJSON(daemonSocketPath(), "/v1/configmaps/"+name, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(resp))

	case "list":
		// List configmaps from the daemon
		resp, err := httpGet(daemonSocketPath(), "/v1/configmaps/")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(resp))

	default:
		fmt.Printf("Unknown subcommand: %s\n", sub)
		fmt.Println("Usage: qm configmap <set|list> ...")
		os.Exit(1)
	}
}

func cmdComponents(args []string) {
	if len(args) < 1 || args[0] != "list" {
		fmt.Println("Usage: qm components list")
		os.Exit(1)
	}

	// Try to get component state from the daemon first.
	body, err := httpGet(daemonSocketPath(), "/v1/components")
	if err == nil {
		var resp struct {
			ComponentsRepo string                       `json:"components_repo"`
			Components     config.ComponentsConfig `json:"components"`
		}
		if json.Unmarshal(body, &resp) == nil {
			if resp.ComponentsRepo != "" {
				fmt.Printf("Catalog: %s\n\n", resp.ComponentsRepo)
			}
			if len(resp.Components) == 0 {
				fmt.Println("No components configured.")
				fmt.Println("Use 'qm enable <name>' to enable a component.")
				return
			}
			for name, c := range resp.Components {
				marker := " "
				if c.Enabled {
					marker = "✓"
				}
				fmt.Printf("  %s  %-20s  %s\n", marker, name, c.Version)
			}
			return
		}
	}

	// Fallback: read from settings file directly.
	path := settingsFilePath()
	s, err := loadOrCreateSettings(path)
	if err != nil {
		fmt.Printf("Error loading settings: %v\n", err)
		os.Exit(1)
	}

	if len(s.Components) == 0 {
		fmt.Println("No components configured.")
		fmt.Println("Use 'qm enable <name>' to enable a component.")
		return
	}

	if s.ComponentsRepo != "" {
		fmt.Printf("Catalog: %s\n\n", s.ComponentsRepo)
	}
	for name, c := range s.Components {
		marker := " "
		if c.Enabled {
			marker = "✓"
		}
		fmt.Printf("  %s  %-20s  %s\n", marker, name, c.Version)
	}
}
