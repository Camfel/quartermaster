package hardware

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectGPUs_NoNVIDIA(t *testing.T) {
	// Use a temp dir as the "system" to guarantee no nvidia-smi
	d := &Detector{
		nvidiaSmiPath: filepath.Join(t.TempDir(), "nonexistent-nvidia-smi"),
		devDir:        t.TempDir(),
	}

	gpus, err := d.DetectGPUs()
	if err != nil {
		t.Fatalf("DetectGPUs should not error: %v", err)
	}
	if len(gpus) != 0 {
		t.Errorf("Expected no GPUs, got %d", len(gpus))
	}
}

func TestHasGPU_NoGPU(t *testing.T) {
	d := &Detector{
		nvidiaSmiPath: filepath.Join(t.TempDir(), "nonexistent-nvidia-smi"),
		devDir:        t.TempDir(),
	}

	if d.HasGPU() {
		t.Error("HasGPU should return false when no GPU")
	}
}

func TestNVIDIAHookPath_NotFound(t *testing.T) {
	d := &Detector{
		nvidiaSmiPath: filepath.Join(t.TempDir(), "nvidia-smi"),
		devDir:        t.TempDir(),
	}

	// Should return empty when no hook is found
	path := d.NVIDIAHookPath()
	if path != "" {
		t.Errorf("Expected empty path, got %s", path)
	}
}

func TestNVIDIARequiredEnv_NoHook(t *testing.T) {
	d := &Detector{
		nvidiaSmiPath: filepath.Join(t.TempDir(), "nvidia-smi"),
		devDir:        t.TempDir(),
	}

	env := d.NVIDIARequiredEnv()
	if env != nil {
		t.Errorf("Expected nil env when no hook, got %v", env)
	}
}

func TestNVIDIARequiredDevices_Fallback(t *testing.T) {
	d := &Detector{
		devDir: t.TempDir(), // Empty dir, no nvidia devices
	}

	devices := d.NVIDIARequiredDevices()
	if len(devices) < 2 {
		t.Errorf("Expected at least fallback devices, got %v", devices)
	}
}

func TestNVIDIARequiredDevices_WithDevices(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock nvidia devices
	os.WriteFile(filepath.Join(tmpDir, "nvidia0"), nil, 0644)
	os.WriteFile(filepath.Join(tmpDir, "nvidiactl"), nil, 0644)
	os.WriteFile(filepath.Join(tmpDir, "nvidia-uvm"), nil, 0644)

	d := &Detector{
		devDir: tmpDir,
	}

	devices := d.NVIDIARequiredDevices()

	found := make(map[string]bool)
	for _, dev := range devices {
		found[dev] = true
	}

	expected := []string{tmpDir + "/nvidia0", tmpDir + "/nvidiactl", tmpDir + "/nvidia-uvm"}
	for _, exp := range expected {
		if !found[exp] {
			t.Errorf("Expected device %s not found in %v", exp, devices)
		}
	}
}

func TestGPUInfo_Structure(t *testing.T) {
	gpu := GPUInfo{
		Type: "nvidia",
		ID:   "GPU-abc123",
	}

	if gpu.Type != "nvidia" {
		t.Errorf("Expected type nvidia, got %s", gpu.Type)
	}
	if gpu.ID != "GPU-abc123" {
		t.Errorf("Expected ID GPU-abc123, got %s", gpu.ID)
	}
}
