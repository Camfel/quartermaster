// Package hardware detects host hardware capabilities for container integration.
// Currently supports NVIDIA GPU detection for OCI spec injection.
package hardware

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GPUInfo describes a detected GPU on the host.
type GPUInfo struct {
	Type string // "nvidia", "amd", "intel", or "none"
	ID   string // PCI bus ID or device identifier
}

// Detector scans the host for available hardware resources.
type Detector struct {
	// Configurable paths for detection
	nvidiaSmiPath string
	devDir        string
}

// NewDetector creates a new hardware detector with default paths.
func NewDetector() *Detector {
	return &Detector{
		nvidiaSmiPath: "/usr/bin/nvidia-smi",
		devDir:        "/dev",
	}
}

// DetectGPUs checks for GPU hardware on the host.
// Returns a list of detected GPUs, or an empty list if none found.
func (d *Detector) DetectGPUs() ([]GPUInfo, error) {
	var gpus []GPUInfo

	// 1. Check for NVIDIA GPUs
	nvidiaGPUs, err := d.detectNVIDIA()
	if err != nil {
		// Non-fatal: log but continue checking other vendors
		return nil, fmt.Errorf("nvidia detection failed: %w", err)
	}
	gpus = append(gpus, nvidiaGPUs...)

	return gpus, nil
}

// detectNVIDIA checks for NVIDIA GPU hardware.
func (d *Detector) detectNVIDIA() ([]GPUInfo, error) {
	// Check if nvidia-smi is available
	if _, err := os.Stat(d.nvidiaSmiPath); os.IsNotExist(err) {
		return nil, nil // No NVIDIA tools installed
	}

	// Try to run nvidia-smi to query GPU info
	cmd := exec.Command(d.nvidiaSmiPath,
		"--query-gpu=index,uuid,pci.bus_id",
		"--format=csv,noheader")
	out, err := cmd.Output()
	if err != nil {
		// nvidia-smi might fail if no GPU or driver issue
		return nil, nil
	}

	var gpus []GPUInfo
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ", ")
		id := ""
		if len(parts) >= 2 {
			id = parts[1] // Use UUID as ID
		}
		gpus = append(gpus, GPUInfo{
			Type: "nvidia",
			ID:   id,
		})
	}

	return gpus, nil
}

// HasGPU returns true if any GPU is detected on the host.
func (d *Detector) HasGPU() bool {
	gpus, err := d.DetectGPUs()
	if err != nil || len(gpus) == 0 {
		return false
	}
	return true
}

// NVIDIAHookPath returns the path to the nvidia-container-runtime-hook,
// used for OCI spec injection.
func (d *Detector) NVIDIAHookPath() string {
	// Common installation paths
	candidates := []string{
		"/usr/bin/nvidia-container-runtime-hook",
		"/usr/local/bin/nvidia-container-runtime-hook",
		"/usr/libexec/oci/hooks.d/nvidia-container-runtime-hook",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// NVIDIARequiredEnv returns environment variables required for NVIDIA container support.
func (d *Detector) NVIDIARequiredEnv() []string {
	if d.NVIDIAHookPath() == "" {
		return nil
	}
	return []string{
		"NVIDIA_VISIBLE_DEVICES=all",
		"NVIDIA_DRIVER_CAPABILITIES=compute,utility",
	}
}

// NVIDIARequiredDevices returns device paths to mount for NVIDIA GPU access.
func (d *Detector) NVIDIARequiredDevices() []string {
	entries, err := os.ReadDir(d.devDir)
	if err != nil {
		return []string{"/dev/nvidiactl", "/dev/nvidia-uvm"}
	}

	var devices []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "nvidia") {
			devices = append(devices, d.devDir+"/"+name)
		}
	}

	if len(devices) == 0 {
		// Fallback for when the dev directory isn't readable or no devices found
		return []string{d.devDir + "/nvidiactl", d.devDir + "/nvidia-uvm"}
	}

	return devices
}
