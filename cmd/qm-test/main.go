// qm-test is a standalone test harness for Quartermaster.  It sets up a
// fresh environment, installs the daemon, deploys a test stack, and verifies
// everything works — no external credentials required.
//
// Usage:
//
//	curl -L -o qm-test https://github.com/Camfel/quartermaster/releases/latest/download/qm-test
//	chmod +x qm-test
//	sudo ./qm-test                 # uses latest release (matching self version)
//	sudo ./qm-test --version v0.6.0
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var version = "dev" // set via -ldflags at build time

func main() {
	var (
		releaseVersion = flag.String("version", "v"+version, "Release version to fetch (e.g. v0.6.0)")
		keepRunning    = flag.Bool("keep", false, "Keep daemon running after test")
		timeout        = flag.Duration("timeout", 2*time.Minute, "Maximum wait time for containers")
	)
	flag.Parse()

	if os.Getuid() != 0 {
		fail("must run as root (sudo ./qm-test)")
	}

	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║     Quartermaster Automated Test Harness             ║")
	fmt.Printf("║     version: %-40s ║\n", version)
	fmt.Println("╚══════════════════════════════════════════════════════╝")
	fmt.Println()

	release := *releaseVersion
	if release == "vdev" {
		release = "latest"
	}
	fmt.Printf("Target release: %s\n", release)

	// ── Phase 1: prerequisites ────────────────────────────────────────
	fmt.Println("\n── Phase 1: Checking prerequisites")
	checkDebian()
	installPrerequisites()

	// ── Phase 2: cleanup ──────────────────────────────────────────────
	fmt.Println("\n── Phase 2: Cleaning up existing Quartermaster")
	cleanupQuartermaster()

	// ── Phase 3: download and install ─────────────────────────────────
	fmt.Println("\n── Phase 3: Downloading and installing Quartermaster")
	installDir := "/usr/local/bin"
	fetchBinary(release, "qm", installDir)
	fetchBinary(release, "qm-daemon", installDir)

	// ── Phase 4: deploy test stack ────────────────────────────────────
	fmt.Println("\n── Phase 4: Deploying test stack")
	configDir := "/etc/quartermaster"
	os.MkdirAll(configDir, 0750)

	stackPath := filepath.Join(configDir, "stack.yaml")
	writeTestStack(stackPath)

	writeSettings(configDir)

	// ── Phase 5: start daemon ─────────────────────────────────────────
	fmt.Println("\n── Phase 5: Starting qm-daemon")
	startDaemon()

	// ── Phase 6: verify ───────────────────────────────────────────────
	fmt.Println("\n── Phase 6: Verifying services")
	verifyServices(*timeout)

	// ── Phase 7: report ───────────────────────────────────────────────
	fmt.Println("\n╔══════════════════════════════════════════════════════╗")
	fmt.Println("║  ✓ All tests passed!                                ║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")

	if !*keepRunning {
		fmt.Println("\nCleaning up...")
		run("systemctl", "stop", "qm-daemon")
		cleanupContainers()
		fmt.Println("Quartermaster uninstalled.  Run with --keep to leave it running.")
	} else {
		fmt.Println("\nDaemon is running.  Check status: qm status")
		fmt.Printf("Stack: %s\n", stackPath)
	}
}

// ── Phase 1: prerequisites ────────────────────────────────────────────

func checkDebian() {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		fail("cannot read /etc/os-release: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "ID=debian") && !strings.Contains(s, "ID=ubuntu") {
		fmt.Println("Warning: not Debian/Ubuntu.  Proceeding but YMMV.")
	} else {
		fmt.Println("  ✓ Debian/Ubuntu detected")
	}
}

func installPrerequisites() {
	pkgs := []struct {
		bin  string
		pkg  string
		name string
	}{
		{"containerd", "containerd", "containerd runtime"},
		{"systemctl", "systemd", "systemd init"},
		{"curl", "curl", "curl downloader"},
	}

	for _, p := range pkgs {
		if _, err := exec.LookPath(p.bin); err == nil {
			fmt.Printf("  ✓ %s found\n", p.name)
			continue
		}
		fmt.Printf("  Installing %s (%s)...\n", p.pkg, p.name)
		run("apt-get", "update", "-qq")
		run("apt-get", "install", "-y", "-qq", p.pkg)
		fmt.Printf("  ✓ %s installed\n", p.name)
	}

	// Ensure containerd is running.
	run("systemctl", "start", "containerd")
	run("systemctl", "enable", "containerd")
	fmt.Println("  ✓ containerd is running")
}

// ── Phase 2: cleanup ──────────────────────────────────────────────────

func cleanupQuartermaster() {
	// Stop the daemon if running.
	runIgnore("systemctl", "stop", "qm-daemon")
	runIgnore("systemctl", "disable", "qm-daemon")

	// Remove systemd unit.
	os.Remove("/etc/systemd/system/qm-daemon.service")
	runIgnore("systemctl", "daemon-reload")

	cleanupContainers()

	// Remove old binaries.
	for _, bin := range []string{"qm", "qm-daemon"} {
		os.Remove(filepath.Join("/usr/local/bin", bin))
	}

	// Remove old config (preserve secrets dir if it has secrets).
	os.RemoveAll("/etc/quartermaster/stack.yaml")
	os.RemoveAll("/etc/quartermaster/settings.json")
	os.RemoveAll("/var/lib/quartermaster/repos")
	os.RemoveAll("/run/quartermaster")
	os.RemoveAll("/var/lib/quartermaster/caddy")
	os.RemoveAll("/var/lib/quartermaster/logs")

	fmt.Println("  ✓ Cleanup complete")
}

func cleanupContainers() {
	// Get all containers in the quartermaster namespace.
	out, err := runCapture("ctr", "-n", "quartermaster", "containers", "list", "-q")
	if err != nil {
		return // no containerd or no containers
	}

	ids := strings.Fields(out)
	if len(ids) == 0 {
		fmt.Println("  No containers to clean up")
		return
	}

	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		fmt.Printf("  Removing container: %s\n", id)
		// Kill task if running.
		runIgnore("ctr", "-n", "quartermaster", "task", "kill", "-s", "SIGKILL", id)
		runIgnore("ctr", "-n", "quartermaster", "task", "delete", id)
		// Delete container.
		runIgnore("ctr", "-n", "quartermaster", "container", "delete", id)
	}

	// Also delete the namespace itself to clear any orphaned state.
	runIgnore("ctr", "namespace", "delete", "quartermaster")
}

// ── Phase 3: download and install ─────────────────────────────────────

func fetchBinary(release, name, dir string) {
	url := fmt.Sprintf("https://github.com/Camfel/quartermaster/releases/download/%s/%s", release, name)
	dest := filepath.Join(dir, name)

	fmt.Printf("  Fetching %s from %s...\n", name, url)

	// Use curl for reliable downloads with redirects.
	run("curl", "-fsSL", "-o", dest, url)
	os.Chmod(dest, 0755)
	fmt.Printf("  ✓ %s installed (%s)\n", name, dest)
}

// ── Phase 4: deploy test stack ────────────────────────────────────────

func writeTestStack(path string) {
	content := `# Quartermaster test stack — automated verification.
# No external credentials required.

version: "1"
kind: stack
metadata:
  name: qm-test

spec:
  services:
    - name: test-web
      image: nginx:alpine
      network: internal
      restart_policy: always
      healthcheck:
        type: http
        path: /
        port: 80
        interval: 5s

    - name: test-sleeper
      image: alpine:latest
      network: internal
      restart_policy: always
      command:
        - sleep
        - infinity
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		fail("write stack.yaml: %v", err)
	}
	fmt.Printf("  ✓ Test stack written: %s\n", path)
}

func writeSettings(configDir string) {
	settings := map[string]interface{}{
		"containerd_socket": "/run/containerd/containerd.sock",
		"namespace":         "quartermaster",
		"sync_interval":     "5s",
		"ingress": map[string]interface{}{
			"domain":           "",
			"tls":              "internal",
			"exclude_services": []string{},
		},
	}
	data, _ := json.MarshalIndent(settings, "", "  ")
	path := filepath.Join(configDir, "settings.json")
	if err := os.WriteFile(path, data, 0640); err != nil {
		fail("write settings.json: %v", err)
	}

	// Set ownership.
	run("chown", "-R", "root:root", configDir)
	run("chmod", "750", configDir)
	fmt.Printf("  ✓ Settings written: %s\n", path)
}

// ── Phase 5: start daemon ─────────────────────────────────────────────

func startDaemon() {
	systemdUnit := `[Unit]
Description=Quartermaster Container Orchestrator (test)
After=network-online.target containerd.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/qm-daemon
Restart=no
RestartSec=5
Environment=QM_SOCKET_PATH=/run/quartermaster
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`

	path := "/etc/systemd/system/qm-daemon.service"
	if err := os.WriteFile(path, []byte(systemdUnit), 0644); err != nil {
		fail("write systemd unit: %v", err)
	}

	run("systemctl", "daemon-reload")
	run("systemctl", "start", "qm-daemon")

	// Wait briefly for the daemon socket to appear.
	time.Sleep(2 * time.Second)
	fmt.Println("  ✓ qm-daemon started")
}

// ── Phase 6: verify ───────────────────────────────────────────────────

func verifyServices(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	socketPath := "/run/quartermaster/daemon.sock"

	var statusJSON struct {
		Containers []struct {
			Name    string `json:"name"`
			Running bool   `json:"running"`
			Healthy *bool  `json:"healthy"`
			Image   string `json:"image"`
		} `json:"containers"`
	}

	required := map[string]bool{
		"test-web":     false,
		"test-sleeper": false,
	}

	fmt.Printf("  Waiting for containers (timeout: %v)...\n", timeout)

	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)

		data, err := daemonGet(socketPath, "/v1/status")
		if err != nil {
			fmt.Printf("  Waiting for daemon... (%v)\n", err)
			continue
		}

		if err := json.Unmarshal(data, &statusJSON); err != nil {
			continue
		}

		for _, c := range statusJSON.Containers {
			if _, ok := required[c.Name]; ok {
				status := "✗"
				if c.Running {
					status = "✓"
				}
				health := "-"
				if c.Healthy != nil {
					if *c.Healthy {
						health = "✓"
					} else {
						health = "✗"
					}
				}
				fmt.Printf("    %s  %-20s running=%v healthy=%s image=%s\n",
					status, c.Name, c.Running, health, c.Image)

				if c.Running {
					required[c.Name] = true
				}
			}
		}

		allReady := true
		for _, ready := range required {
			if !ready {
				allReady = false
				break
			}
		}
		if allReady {
			fmt.Println("  ✓ All services running")

			// Verify dashboard endpoint.
			fmt.Println("\n  Checking dashboard...")
			data, err := daemonGet(socketPath, "/v1/dashboard")
			if err != nil {
				fmt.Printf("  ✗ Dashboard endpoint failed: %v\n", err)
			} else {
				html := string(data)
				if strings.Contains(html, "<!DOCTYPE html>") && strings.Contains(html, "Quartermaster Dashboard") {
					fmt.Println("  ✓ Dashboard endpoint responding")
					fmt.Println("    View it:  curl --unix-socket /run/quartermaster/daemon.sock http://localhost/v1/dashboard")
				} else {
					fmt.Println("  ✗ Dashboard returned unexpected content")
				}
			}
			return
		}
	}

	// Report failures.
	var failed []string
	for name, ready := range required {
		if !ready {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		fmt.Println("\n  Containers that didn't come up:")
		for _, name := range failed {
			fmt.Printf("    - %s\n", name)
			// Try to get logs.
			data, err := daemonGet(socketPath, "/v1/services/"+name+"/logs?tail=all")
			if err == nil {
				fmt.Printf("      logs: %s\n", string(data))
			}
		}
		fail("services failed to start: %v", failed)
	}
}

// ── Helpers ────────────────────────────────────────────────────────────

func daemonGet(socketPath, urlPath string) ([]byte, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
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

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fail("%s %s: %v", name, strings.Join(args, " "), err)
	}
}

func runIgnore(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Run()
}

func runCapture(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	return string(out), err
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "\n  ✗ FAIL: "+format+"\n\n", args...)
	fmt.Fprintln(os.Stderr, "  Test harness failed.  Check logs above for details.")
	os.Exit(1)
}
