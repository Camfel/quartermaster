package hardware

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasGPU_NoGPU(t *testing.T) {
	d := newDetector(t.TempDir())
	if d.HasGPU() {
		t.Error("HasGPU should return false when no GPU")
	}
}

func TestHasGPU_WithNVIDIA(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "nvidia0"), nil, 0644)
	if !newDetector(tmpDir).HasGPU() {
		t.Error("HasGPU should return true when /dev/nvidia0 exists")
	}
}

func TestHasGPU_WithDRM(t *testing.T) {
	tmpDir := t.TempDir()
	driDir := filepath.Join(tmpDir, "dri")
	os.MkdirAll(driDir, 0755)
	os.WriteFile(filepath.Join(driDir, "renderD128"), nil, 0644)
	if !newDetector(tmpDir).HasGPU() {
		t.Error("HasGPU should return true when a DRM render node exists")
	}
}

func TestNVIDIAHookPath_NotFound(t *testing.T) {
	// Use real detector — hook won't be installed in CI.
	d := NewDetector()
	// Override: we can't override hook paths, but on a non-GPU CI runner
	// the hook shouldn't be installed. Skip if it accidentally is.
	if d.NVIDIAHookPath() != "" {
		t.Skip("nvidia-container-runtime-hook unexpectedly found")
	}
}

func TestNVIDIARequiredEnv_NoHook(t *testing.T) {
	d := NewDetector()
	if d.NVIDIAHookPath() != "" {
		t.Skip("nvidia-container-runtime-hook found — skipping nil-env test")
	}
	if d.NVIDIARequiredEnv() != nil {
		t.Error("NVIDIARequiredEnv should return nil when no hook")
	}
}

func TestNVIDIARequiredDevices_Fallback(t *testing.T) {
	d := newDetector(t.TempDir())
	devices := d.NVIDIARequiredDevices()
	if len(devices) < 2 {
		t.Errorf("Expected fallback devices, got %v", devices)
	}
}

func TestNVIDIARequiredDevices_WithDevices(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "nvidia0"), nil, 0644)
	os.WriteFile(filepath.Join(tmpDir, "nvidiactl"), nil, 0644)

	d := newDetector(tmpDir)
	devices := d.NVIDIARequiredDevices()

	found := make(map[string]bool)
	for _, dev := range devices {
		found[dev] = true
	}
	for _, exp := range []string{tmpDir + "/nvidia0", tmpDir + "/nvidiactl"} {
		if !found[exp] {
			t.Errorf("Expected device %s not found in %v", exp, devices)
		}
	}
}
