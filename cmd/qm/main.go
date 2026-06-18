package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/secrets"

	"github.com/go-git/go-git/v5"
	"github.com/spf13/cobra"
)

const (
	version              = "0.5.0"
	defaultSecretsDir    = "/etc/quartermaster/secrets"
	defaultMasterKeyPath = "/etc/quartermaster/master.key"
)

func main() {
	var socketPath string

	root := &cobra.Command{
		Use:           "qm",
		Short:         "Quartermaster — lightweight container orchestrator for homelabs",
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	root.PersistentFlags().StringVar(&socketPath, "socket", "", "Daemon Unix socket directory (env: QM_SOCKET_PATH)")

	root.AddCommand(
		newValidateCmd(),
		newStatusCmd(&socketPath),
		newSecretCmd(),
		newServiceCmd(&socketPath),
		newComponentsCmd(),
		newEnableCmd(),
	)

	root.Version = version
	root.SetVersionTemplate("Quartermaster CLI v{{.Version}}\n")

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// ── validate ─────────────────────────────────────────────────────────────

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file>",
		Short: "Check a stack file for errors",
		Long:  "Validate a quartermaster stack YAML file and print a summary of services.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cm := config.NewConfigManager()
			stack, err := cm.LoadStack(args[0])
			if err != nil {
				return fmt.Errorf("validation failed: %w", err)
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
			return nil
		},
	}
}

// ── status ───────────────────────────────────────────────────────────────

func newStatusCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon and container status",
		Long:  "Query the running qm-daemon and display a table of managed containers.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := httpGet(*socketPath, "/v1/status")
			if err != nil {
				return fmt.Errorf("cannot reach daemon (is qm-daemon running?): %w", err)
			}

			var s struct {
				Version            string `json:"version"`
				Uptime             string `json:"uptime"`
				LastReconcileError string `json:"last_reconcile_error"`
				ReconcileCount     int64  `json:"reconcile_count"`
				Containers         []struct {
					Name    string   `json:"name"`
					Image   string   `json:"image"`
					Running bool     `json:"running"`
					PID     uint32   `json:"pid"`
					Healthy *bool    `json:"healthy"`
					Ports   []string `json:"ports"`
					Network string   `json:"network"`
				} `json:"containers"`
			}
			if err := json.Unmarshal(resp, &s); err != nil {
				return fmt.Errorf("malformed daemon response: %w", err)
			}

			fmt.Printf("Quartermaster Daemon %s  │  uptime %s  │  %d reconcile(s)\n\n",
				s.Version, s.Uptime, s.ReconcileCount)

			if s.LastReconcileError != "" {
				fmt.Printf("⚠ Last reconcile error: %s\n\n", s.LastReconcileError)
			}

			if len(s.Containers) == 0 {
				fmt.Println("No containers managed.")
				return nil
			}

			fmt.Printf("%-20s %-8s %-8s %-16s %-8s %s\n", "NAME", "STATUS", "NET", "PORTS", "HEALTH", "IMAGE")
			fmt.Println(strings.Repeat("-", 90))

			for _, c := range s.Containers {
				status := "down"
				if c.Running {
					status = "up"
				}
				health := "-"
				if c.Healthy != nil {
					if *c.Healthy {
						health = "\u2713"
					} else {
						health = "\u2717"
					}
				}
				ports := "-"
				if len(c.Ports) > 0 {
					ports = strings.Join(c.Ports, ",")
				}
				net := c.Network
				if net == "" {
					net = "-"
				}
				fmt.Printf("%-20s %-8s %-8s %-16s %-8s %s\n", c.Name, status, net, ports, health, c.Image)
			}
			return nil
		},
	}
}

// ── secret ───────────────────────────────────────────────────────────────

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage encrypted secrets",
		Long:  "Create and list secrets encrypted at rest with NaCl secretbox.",
	}
	cmd.AddCommand(
		newSecretCreateCmd(),
		newSecretListCmd(),
	)
	return cmd
}

func newSecretCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create or update an encrypted secret (reads from stdin)",
		Long: `Read a value from stdin, encrypt it with the master key, and store it
in /etc/quartermaster/secrets/.  The value is never echoed.

Example:
  echo "my-api-token" | qm secret create my-api-token`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			key, err := secrets.LoadOrCreateKey(defaultMasterKeyPath)
			if err != nil {
				return fmt.Errorf("master key: %w", err)
			}

			input, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
			if err != nil {
				return fmt.Errorf("reading stdin: %w", err)
			}
			input = []byte(strings.TrimRight(string(input), "\n\r"))
			if len(input) == 0 {
				return fmt.Errorf("no input provided on stdin")
			}

			if err := os.MkdirAll(defaultSecretsDir, 0700); err != nil {
				return err
			}

			mgr := secrets.NewManager(defaultSecretsDir).WithEncryption(key)
			if err := mgr.CreateEncrypted(name, input); err != nil {
				return fmt.Errorf("storing secret: %w", err)
			}

			fmt.Printf("✓ Secret %q created (encrypted)\n", name)
			return nil
		},
	}
}

func newSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored secret names (values not shown)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(defaultMasterKeyPath); os.IsNotExist(err) {
				fmt.Println("No master key found. Create a secret first with: qm secret create <name>")
				return nil
			}
			mgr := secrets.NewManager(defaultSecretsDir)
			names, err := mgr.ListNames()
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Println("No secrets stored.")
				return nil
			}
			fmt.Println("Secrets:")
			for _, name := range names {
				fmt.Printf("  - %s\n", name)
			}
			return nil
		},
	}
}

// ── service ──────────────────────────────────────────────────────────────

func newServiceCmd(socketPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Inspect and manage running services",
	}
	cmd.AddCommand(
		newServiceLogsCmd(socketPath),
		newServiceRestartCmd(socketPath),
	)
	return cmd
}

func newServiceLogsCmd(socketPath *string) *cobra.Command {
	var tail string
	c := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show recent container logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tail == "" {
				tail = "4096"
			}
			resp, err := httpGet(*socketPath, "/v1/services/"+args[0]+"/logs?tail="+tail)
			if err != nil {
				return fmt.Errorf("fetching logs: %w", err)
			}
			fmt.Print(string(resp))
			return nil
		},
	}
	c.Flags().StringVarP(&tail, "tail", "n", "4096", "Bytes of log tail to fetch (or 'all')")
	return c
}

func newServiceRestartCmd(socketPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "restart <name>",
		Short: "Stop and redeploy a service",
		Long:  "Delete the running container so the daemon recreates it on the next reconciliation pass.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := httpPost(*socketPath, "/v1/services/"+args[0]+"/restart")
			if err != nil {
				return fmt.Errorf("restart request: %w", err)
			}
			var result struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
				Error   string `json:"error,omitempty"`
			}
			if err := json.Unmarshal(resp, &result); err != nil {
				return fmt.Errorf("malformed daemon response: %w", err)
			}
			if result.Error != "" {
				return fmt.Errorf("%s", result.Error)
			}
			fmt.Println(result.Message)
			return nil
		},
	}
}

// ── HTTP helpers ─────────────────────────────────────────────────────────

func daemonSocketPath(flagPath string) string {
	if flagPath != "" {
		return flagPath + "/daemon.sock"
	}
	if p := os.Getenv("QM_SOCKET_PATH"); p != "" {
		return p + "/daemon.sock"
	}
	return "/run/quartermaster/daemon.sock"
}

func httpGet(socketOverride, urlPath string) ([]byte, error) {
	socketPath := daemonSocketPath(socketOverride)
	client := &http.Client{
		Timeout: 10 * time.Second,
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

func httpPost(socketOverride, urlPath string) ([]byte, error) {
	socketPath := daemonSocketPath(socketOverride)
	client := &http.Client{
		Timeout: 10 * time.Second,
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

// ── components ──────────────────────────────────────────────────────────

func newComponentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "components",
		Short: "Manage curated components (reverse proxy, VPN, monitoring)",
	}
	cmd.AddCommand(newComponentsListCmd())
	return cmd
}

func newComponentsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available components and their enabled status",
		RunE: func(cmd *cobra.Command, args []string) error {
			settingsPath := config.DefaultSettingsPath()
			settings, err := config.LoadSettings(settingsPath)
			if err != nil {
				return fmt.Errorf("loading settings: %w", err)
			}

			if settings.ComponentsRepo == "" {
				fmt.Printf("components_repo is not configured in %s.\n", settingsPath)
				fmt.Println("Enable a component to auto-configure it:")
				fmt.Println("  qm enable vpn")
				return nil
			}

			localBase := filepath.Join(filepath.Dir(settings.LKGPath), "components")
			repoDir := filepath.Join(localBase, "repo")

			if err := cloneOrPull(settings.ComponentsRepo, repoDir); err != nil {
				return fmt.Errorf("fetching components: %w", err)
			}

			entries, err := os.ReadDir(repoDir)
			if err != nil {
				return fmt.Errorf("reading components repo: %w", err)
			}

			fmt.Println("Available components:")
			for _, entry := range entries {
				if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
					continue
				}
				name := entry.Name()
				compDir := filepath.Join(repoDir, name)

				verEntries, err := os.ReadDir(compDir)
				if err != nil {
					continue
				}
				var versions []string
				for _, ve := range verEntries {
					if ve.IsDir() {
						sf := filepath.Join(compDir, ve.Name(), "stack.yaml")
						if _, err := os.Stat(sf); err == nil {
							versions = append(versions, ve.Name())
						}
					}
				}
				if len(versions) == 0 {
					continue
				}

				comp, enabled := settings.Components[name]
				status := "  disabled"
				ver := ""
				if enabled && comp.Enabled {
					status = "✓ enabled"
					ver = comp.Version
				}
				if ver == "" {
					ver = versions[0]
				}

				fmt.Printf("  %-22s %s  (version: %s, available: %s)\n", name, status, ver, strings.Join(versions, ", "))
			}
			fmt.Println()
			fmt.Println("Enable a component with: qm enable <name>")
			return nil
		},
	}
}

// ── enable ──────────────────────────────────────────────────────────────

func newEnableCmd() *cobra.Command {
	var compVersion string

	cmd := &cobra.Command{
		Use:   "enable <name>",
		Short: "Enable a curated component",
		Long: `Enable a component from the quartermaster-components repository.

The component's stack.yaml is merged with your stacks.  User services
take precedence on name conflicts.

Reload the daemon to apply:
  sudo systemctl reload qm-daemon`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			settingsPath := config.DefaultSettingsPath()
			settings, err := config.LoadSettings(settingsPath)
			if err != nil {
				return fmt.Errorf("loading settings: %w", err)
			}

			if settings.ComponentsRepo == "" {
				settings.ComponentsRepo = "https://github.com/Camfel/quartermaster-components"
			}

			localBase := filepath.Join(filepath.Dir(settings.LKGPath), "components")
			repoDir := filepath.Join(localBase, "repo")
			if err := cloneOrPull(settings.ComponentsRepo, repoDir); err != nil {
				return fmt.Errorf("fetching components: %w", err)
			}

			compDir := filepath.Join(repoDir, name)
			if info, err := os.Stat(compDir); err != nil || !info.IsDir() {
				return fmt.Errorf("component %q not found in %s.\nRun 'qm components list' to see available components.", name, settings.ComponentsRepo)
			}

			version := compVersion
			if version == "" {
				verEntries, err := os.ReadDir(compDir)
				if err != nil {
					return fmt.Errorf("reading component %q: %w", name, err)
				}
				for _, ve := range verEntries {
					if ve.IsDir() {
						sf := filepath.Join(compDir, ve.Name(), "stack.yaml")
						if _, err := os.Stat(sf); err == nil {
							version = ve.Name()
							break
						}
					}
				}
			}
			if version == "" {
				return fmt.Errorf("no version found for component %q", name)
			}

			if settings.Components == nil {
				settings.Components = make(config.ComponentsConfig)
			}
			existing := settings.Components[name]
			existing.Enabled = true
			if existing.Version == "" {
				existing.Version = version
			}
			settings.Components[name] = existing

			if err := config.SaveSettings(settingsPath, settings); err != nil {
				return fmt.Errorf("saving settings: %w", err)
			}

			fmt.Printf("✓ Component %q enabled (version: %s)\n", name, existing.Version)
			fmt.Println()
			fmt.Println("Reload the daemon to apply:")
			fmt.Println("  sudo systemctl reload qm-daemon")
			return nil
		},
	}

	cmd.Flags().StringVarP(&compVersion, "version", "v", "", "Component version (default: latest available)")
	return cmd
}

// ── Git helper ──────────────────────────────────────────────────────────

// cloneOrPull ensures the repo exists at dir, cloning if absent or pulling if present.
func cloneOrPull(repoURL, dir string) error {
	if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
		repo, err := git.PlainOpen(dir)
		if err != nil {
			return fmt.Errorf("opening repo at %s: %w", dir, err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			return fmt.Errorf("worktree: %w", err)
		}
		if err := wt.Pull(&git.PullOptions{
			RemoteName: "origin",
		}); err != nil && err != git.NoErrAlreadyUpToDate {
			os.RemoveAll(dir)
		} else {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return err
	}
	os.RemoveAll(dir)

	_, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL:   repoURL,
		Depth: 1,
	})
	if err != nil {
		return fmt.Errorf("cloning %s: %w", repoURL, err)
	}
	return nil
}
