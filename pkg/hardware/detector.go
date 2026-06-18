// Package hardware detects host hardware capabilities for container integration.
// Uses pure Go filesystem checks — no os/exec, no external binaries.
package hardware

import (
	"os"
	"strings"
)

// Detector scans the host for available hardware resources.
type Detector struct {
	devDir string
}

// NewDetector creates a new hardware detector with default paths.
func NewDetector() *Detector {
	return newDetector("/dev")
}

// newDetector is the internal constructor — the test package uses it
// directly to supply a temp directory.
func newDetector(devDir string) *Detector {
	return &Detector{devDir: devDir}
}

// HasGPU returns true if a GPU is detected on the host.
// Checks for NVIDIA (/dev/nvidia0) and generic DRM render nodes.
func (d *Detector) HasGPU() bool {
	return d.hasNVIDIA() || d.hasDRM()
}

// hasNVIDIA checks for the NVIDIA kernel module via /dev/nvidia0.
func (d *Detector) hasNVIDIA() bool {
	_, err := os.Stat(d.devDir + "/nvidia0")
	return err == nil
}

// hasDRM checks for Direct Rendering Manager GPU devices.
func (d *Detector) hasDRM() bool {
	entries, err := os.ReadDir(d.devDir + "/dri")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "renderD") {
			return true
		}
	}
	return false
}

// NVIDIAHookPath returns the path to the nvidia-container-runtime-hook
// if it is installed, or empty string otherwise.
func (d *Detector) NVIDIAHookPath() string {
	for _, p := range []string{
		"/usr/bin/nvidia-container-runtime-hook",
		"/usr/local/bin/nvidia-container-runtime-hook",
		"/usr/libexec/oci/hooks.d/nvidia-container-runtime-hook",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// NVIDIARequiredEnv returns environment variables required for NVIDIA
// container support.  Returns nil if no hook is found.
func (d *Detector) NVIDIARequiredEnv() []string {
	if d.NVIDIAHookPath() == "" {
		return nil
	}
	return []string{
		"NVIDIA_VISIBLE_DEVICES=all",
		"NVIDIA_DRIVER_CAPABILITIES=compute,utility",
	}
}

// NVIDIARequiredDevices returns device paths to mount for GPU access.
func (d *Detector) NVIDIARequiredDevices() []string {
	entries, err := os.ReadDir(d.devDir)
	if err != nil {
		return []string{"/dev/nvidiactl", "/dev/nvidia-uvm"}
	}
	var devices []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "nvidia") {
			devices = append(devices, d.devDir+"/"+entry.Name())
		}
	}
	if len(devices) == 0 {
		return []string{"/dev/nvidiactl", "/dev/nvidia-uvm"}
	}
	return devices
}
