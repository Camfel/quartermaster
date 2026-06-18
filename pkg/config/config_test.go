package config

import (
	"testing"

	"quartermaster/pkg/types"
)

func TestLoadStack_Valid(t *testing.T) {
	cm := NewConfigManager()
	stack, err := cm.LoadStack("testdata/valid.yaml")

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if stack.Metadata.Name != "test-stack" {
		t.Errorf("Expected name 'test-stack', got %s", stack.Metadata.Name)
	}

	if len(stack.Spec.Services) != 1 {
		t.Errorf("Expected 1 service, got %d", len(stack.Spec.Services))
	}

	if stack.Spec.Services[0].Env[0].Value != "hello" {
		t.Errorf("Expected env value 'hello', got %s", stack.Spec.Services[0].Env[0].Value)
	}
}

func TestLoadStack_Invalid(t *testing.T) {
	cm := NewConfigManager()
	_, err := cm.LoadStack("testdata/invalid.yaml")

	if err == nil {
		t.Fatal("Expected an error for invalid configuration, but got none")
	}
}

func TestLoadStack_FileNotFound(t *testing.T) {
	cm := NewConfigManager()
	_, err := cm.LoadStack("testdata/nonexistent.yaml")

	if err == nil {
		t.Fatal("Expected an error for non-existent file, but got none")
	}
}

func TestSaveAndLoadStack(t *testing.T) {
	cm := NewConfigManager()

	tmpDir := t.TempDir()
	lkgPath := tmpDir + "/lkg/stack.yaml"

	// Load a valid stack
	stack, err := cm.LoadStack("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadStack failed: %v", err)
	}

	// Save it to a new path (creates dirs)
	if err := cm.SaveStack(lkgPath, stack); err != nil {
		t.Fatalf("SaveStack failed: %v", err)
	}

	// Load it back
	reloaded, err := cm.LoadStack(lkgPath)
	if err != nil {
		t.Fatalf("LoadStack from saved path failed: %v", err)
	}

	if reloaded.Metadata.Name != stack.Metadata.Name {
		t.Errorf("Name mismatch: expected %q, got %q", stack.Metadata.Name, reloaded.Metadata.Name)
	}
	if len(reloaded.Spec.Services) != len(stack.Spec.Services) {
		t.Errorf("Service count mismatch: expected %d, got %d", len(stack.Spec.Services), len(reloaded.Spec.Services))
	}
}

func TestSaveStack_Invalid(t *testing.T) {
	cm := NewConfigManager()

	tmpDir := t.TempDir()
	path := tmpDir + "/bad.yaml"

	// Create an invalid stack (missing kind)
	badStack := &types.Stack{
		Version:  "1",
		Kind:     "WrongKind",
		Metadata: types.Metadata{Name: "test"},
	}

	err := cm.SaveStack(path, badStack)
	if err == nil {
		t.Fatal("Expected error saving invalid stack, got nil")
	}
}

// --- Enhanced Validation Tests ---

func TestValidate_DuplicateServiceName(t *testing.T) {
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "web", Image: "nginx:latest"},
				{Name: "web", Image: "nginx:latest"},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for duplicate service name")
	}
}

func TestValidate_InvalidImage(t *testing.T) {
	cm := NewConfigManager()
	cases := []string{
		"",                  // empty
		"image with spaces", // spaces
		"repo:tag:extra",    // multiple colons
	}
	for _, img := range cases {
		stack := &types.Stack{
			Version:  "1",
			Kind:     "Stack",
			Metadata: types.Metadata{Name: "test"},
			Spec: types.StackSpec{
				Services: []types.Service{
					{Name: "svc", Image: img},
				},
			},
		}
		err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
		if err == nil {
			t.Errorf("Expected error for invalid image %q", img)
		}
	}
}

func TestValidate_ValidImages(t *testing.T) {
	cm := NewConfigManager()
	cases := []string{
		"alpine",
		"alpine:latest",
		"library/alpine",
		"docker.io/library/alpine:latest",
		"nginx:1.25",
		"ghcr.io/owner/repo:v1.2.3",
	}
	for _, img := range cases {
		stack := &types.Stack{
			Version:  "1",
			Kind:     "Stack",
			Metadata: types.Metadata{Name: "test"},
			Spec: types.StackSpec{
				Services: []types.Service{
					{Name: "svc", Image: img},
				},
			},
		}
		err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
		if err != nil {
			t.Errorf("Expected no error for valid image %q, got: %v", img, err)
		}
	}
}

func TestValidate_InvalidRestartPolicy(t *testing.T) {
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", RestartPolicy: "whenever"},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for invalid restart policy")
	}
}

func TestValidate_PortCollision(t *testing.T) {
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc-a", Image: "alpine", Ports: []types.Port{{Host: 8080, Container: 80}}},
				{Name: "svc-b", Image: "nginx", Ports: []types.Port{{Host: 8080, Container: 80}}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for port collision")
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	cm := NewConfigManager()
	cases := []struct {
		host, container int
	}{
		{0, 80},     // host too low
		{65536, 80}, // host too high
		{80, 0},     // container too low
		{80, 65536}, // container too high
	}
	for _, c := range cases {
		stack := &types.Stack{
			Version:  "1",
			Kind:     "Stack",
			Metadata: types.Metadata{Name: "test"},
			Spec: types.StackSpec{
				Services: []types.Service{
					{Name: "svc", Image: "alpine", Ports: []types.Port{{Host: c.host, Container: c.container}}},
				},
			},
		}
		err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
		if err == nil {
			t.Errorf("Expected error for invalid port (host=%d, container=%d)", c.host, c.container)
		}
	}
}

func TestValidate_VolumeSourceTargetRequired(t *testing.T) {
	cm := NewConfigManager()

	// Missing source
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", Volumes: []types.Volume{{Target: "/data"}}},
			},
		},
	}
	if err := cm.SaveStack(t.TempDir()+"/test.yaml", stack); err == nil {
		t.Fatal("Expected error for missing volume source")
	}

	// Missing target
	stack.Spec.Services[0].Volumes = []types.Volume{{Source: "/host/data"}}
	if err := cm.SaveStack(t.TempDir()+"/test2.yaml", stack); err == nil {
		t.Fatal("Expected error for missing volume target")
	}
}

func TestValidate_InvalidVolumeType(t *testing.T) {
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", Volumes: []types.Volume{{Source: "/s", Target: "/t", Type: "nfs"}}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for invalid volume type")
	}
}

func TestValidate_EnvNameRequired(t *testing.T) {
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", Env: []types.EnvVar{{Value: "bar"}}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for missing env name")
	}
}

func TestValidate_SecretRefRequired(t *testing.T) {
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", Secrets: []types.SecretRef{{Name: "mysecret"}}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for missing secret_ref")
	}
}

func TestValidate_HealthCheck(t *testing.T) {
	cm := NewConfigManager()

	// Invalid type
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", HealthCheck: &types.HealthCheck{Type: "grpc", Interval: "10s"}},
			},
		},
	}
	if err := cm.SaveStack(t.TempDir()+"/test.yaml", stack); err == nil {
		t.Fatal("Expected error for invalid healthcheck type")
	}

	// Missing interval
	stack.Spec.Services[0].HealthCheck = &types.HealthCheck{Type: "http", Path: "/health"}
	if err := cm.SaveStack(t.TempDir()+"/test2.yaml", stack); err == nil {
		t.Fatal("Expected error for missing healthcheck interval")
	}

	// HTTP type missing path
	stack.Spec.Services[0].HealthCheck = &types.HealthCheck{Type: "http", Port: 8080, Interval: "10s"}
	if err := cm.SaveStack(t.TempDir()+"/test3.yaml", stack); err == nil {
		t.Fatal("Expected error for healthcheck http type missing path")
	}

	// TCP type missing port
	stack.Spec.Services[0].HealthCheck = &types.HealthCheck{Type: "tcp", Interval: "10s"}
	if err := cm.SaveStack(t.TempDir()+"/test4.yaml", stack); err == nil {
		t.Fatal("Expected error for healthcheck tcp type missing port")
	}

	// Valid healthcheck
	stack.Spec.Services[0].HealthCheck = &types.HealthCheck{Type: "http", Path: "/health", Port: 8080, Interval: "10s"}
	if err := cm.SaveStack(t.TempDir()+"/test5.yaml", stack); err != nil {
		t.Errorf("Expected no error for valid healthcheck, got: %v", err)
	}
}

func TestValidate_UserFormat(t *testing.T) {
	cm := NewConfigManager()
	cases := []string{
		"root",          // missing colon
		"1000:",         // missing gid
		":1000",         // missing uid
		"uid:gid:extra", // too many parts
	}
	for _, u := range cases {
		stack := &types.Stack{
			Version:  "1",
			Kind:     "Stack",
			Metadata: types.Metadata{Name: "test"},
			Spec: types.StackSpec{
				Services: []types.Service{
					{Name: "svc", Image: "alpine", User: u},
				},
			},
		}
		err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
		if err == nil {
			t.Errorf("Expected error for invalid user format %q", u)
		}
	}

	// Valid user format
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", User: "1000:1000"},
			},
		},
	}
	if err := cm.SaveStack(t.TempDir()+"/test2.yaml", stack); err != nil {
		t.Errorf("Expected no error for valid user format, got: %v", err)
	}
}

func TestValidate_DependsOn(t *testing.T) {
	cm := NewConfigManager()

	// Self-dependency
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", DependsOn: []string{"svc"}},
			},
		},
	}
	if err := cm.SaveStack(t.TempDir()+"/test.yaml", stack); err == nil {
		t.Fatal("Expected error for self-dependency")
	}

	// Unknown dependency — now allowed (services may be in other stacks).
	stack.Spec.Services[0].DependsOn = []string{"nonexistent"}
	if err := cm.SaveStack(t.TempDir()+"/test2.yaml", stack); err != nil {
		t.Fatal("Unexpected error for cross-stack dependency:", err)
	}

	// Valid dependency
	stack.Spec.Services = []types.Service{
		{Name: "db", Image: "postgres"},
		{Name: "web", Image: "nginx", DependsOn: []string{"db"}},
	}
	if err := cm.SaveStack(t.TempDir()+"/test3.yaml", stack); err != nil {
		t.Errorf("Expected no error for valid dependency, got: %v", err)
	}
}

func TestValidate_PortProtocol_DifferentProtoSamePort(t *testing.T) {
	// TCP and UDP on the same host port should be allowed (e.g. DNS, DHT)
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", Ports: []types.Port{
					{Host: 6881, Container: 6881, Protocol: "tcp"},
					{Host: 6881, Container: 6881, Protocol: "udp"},
				}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err != nil {
		t.Errorf("Expected no error for tcp+udp on same port, got: %v", err)
	}
}

func TestValidate_PortProtocol_SameProtoCollision(t *testing.T) {
	// Same protocol on the same host port should still collide
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc-a", Image: "alpine", Ports: []types.Port{{Host: 8080, Container: 80, Protocol: "tcp"}}},
				{Name: "svc-b", Image: "nginx", Ports: []types.Port{{Host: 8080, Container: 80, Protocol: "tcp"}}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for same protocol port collision")
	}
}

func TestValidate_PortProtocol_DefaultIsTCP(t *testing.T) {
	// Omitting protocol defaults to "tcp", so two unspecified ports collide
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc-a", Image: "alpine", Ports: []types.Port{{Host: 8080, Container: 80}}},
				{Name: "svc-b", Image: "nginx", Ports: []types.Port{{Host: 8080, Container: 80}}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for default-protocol port collision")
	}
}

func TestValidate_PortProtocol_InvalidProtocol(t *testing.T) {
	cm := NewConfigManager()
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", Ports: []types.Port{{Host: 80, Container: 80, Protocol: "https"}}},
			},
		},
	}
	err := cm.SaveStack(t.TempDir()+"/test.yaml", stack)
	if err == nil {
		t.Fatal("Expected error for invalid protocol")
	}
}

func TestValidate_NetworkProfile(t *testing.T) {
	cm := NewConfigManager()

	// Invalid profile
	stack := &types.Stack{
		Version:  "1",
		Kind:     "Stack",
		Metadata: types.Metadata{Name: "test"},
		Spec: types.StackSpec{
			Services: []types.Service{
				{Name: "svc", Image: "alpine", Network: "bridge"},
			},
		},
	}
	if err := cm.SaveStack(t.TempDir()+"/test.yaml", stack); err == nil {
		t.Fatal("Expected error for invalid network profile")
	}

	// Valid profiles
	for _, profile := range []string{"public", "internal", "vpn", ""} {
		stack.Spec.Services[0].Network = profile
		err := cm.SaveStack(t.TempDir()+"/test-valid.yaml", stack)
		if err != nil {
			t.Errorf("Expected no error for profile %q, got: %v", profile, err)
		}
	}
}
