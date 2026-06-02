package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"quartermaster/pkg/types"

	"gopkg.in/yaml.v3"
)

// ConfigManager handles loading and validating configurations.
type ConfigManager struct {
	// We can add more fields here later, like a cache of loaded configs.
}

// NewConfigManager creates a new instance of ConfigManager.
func NewConfigManager() *ConfigManager {
	return &ConfigManager{}
}

// LoadStack reads a YAML file from the given path and unmarshals it into a Stack.
func (cm *ConfigManager) LoadStack(path string) (*types.Stack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var stack types.Stack
	err = yaml.Unmarshal(data, &stack)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	if err := cm.validate(&stack); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &stack, nil
}

// SaveStack writes a Stack to a YAML file at the given path.
// Parent directories are created if they do not exist.
func (cm *ConfigManager) SaveStack(path string, stack *types.Stack) error {
	if err := cm.validate(stack); err != nil {
		return fmt.Errorf("refusing to save invalid stack: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	data, err := yaml.Marshal(stack)
	if err != nil {
		return fmt.Errorf("failed to marshal stack: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write stack file: %w", err)
	}

	return nil
}

// validRestartPolicies defines the allowed restart policy values.
var validRestartPolicies = map[string]bool{
	"always":        true,
	"unless-stopped": true,
	"on-failure":    true,
	"no":            true,
	"":              true, // empty defaults to the runtime default
}

// validVolumeTypes defines the allowed volume type values.
var validVolumeTypes = map[string]bool{
	"bind":   true,
	"volume": true,
	"tmpfs":  true,
	"":       true, // empty defaults to "bind"
}

// validHealthCheckTypes defines the allowed health check probe types.
var validHealthCheckTypes = map[string]bool{
	"http": true,
	"tcp":  true,
	"exec": true,
}

// imageRegex validates common container image reference formats.
// Matches: alpine, alpine:latest, library/alpine, docker.io/library/alpine:latest
var imageRegex = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._\-]*(/[a-zA-Z0-9_][a-zA-Z0-9._\-]*)*(:\w[\w.\-]*)?$`)

// validate performs structural and semantic validation on a Stack.
func (cm *ConfigManager) validate(stack *types.Stack) error {
	if stack.Version == "" {
		return fmt.Errorf("version is required")
	}
	if stack.Kind != "Stack" {
		return fmt.Errorf("kind must be 'Stack', got %q", stack.Kind)
	}
	if stack.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}

	// Service-level validation
	seenNames := make(map[string]bool)
	// Key: "hostPort:protocol" — protocols allow the same host port between
	// TCP and UDP (e.g. qBittorrent DHT, DNS).
	seenHostPorts := make(map[string]string)

	for i := range stack.Spec.Services {
		svc := &stack.Spec.Services[i]

		// Name is required and must be unique
		if svc.Name == "" {
			return fmt.Errorf("service at index %d: name is required", i)
		}
		if seenNames[svc.Name] {
			return fmt.Errorf("duplicate service name %q", svc.Name)
		}
		seenNames[svc.Name] = true

		// Image is required and must match valid format
		if svc.Image == "" {
			return fmt.Errorf("service %q: image is required", svc.Name)
		}
		if err := validateImage(svc.Image); err != nil {
			return fmt.Errorf("service %q: invalid image %q: %w", svc.Name, svc.Image, err)
		}

		// Restart policy validation
		if !validRestartPolicies[svc.RestartPolicy] {
			return fmt.Errorf("service %q: invalid restart_policy %q (must be one of: always, unless-stopped, on-failure, no)", svc.Name, svc.RestartPolicy)
		}

		// Port validation
		for _, port := range svc.Ports {
			if port.Host <= 0 || port.Host > 65535 {
				return fmt.Errorf("service %q: invalid host port %d (must be 1-65535)", svc.Name, port.Host)
			}
			if port.Container <= 0 || port.Container > 65535 {
				return fmt.Errorf("service %q: invalid container port %d (must be 1-65535)", svc.Name, port.Container)
			}
			proto := port.Protocol
			if proto == "" {
				proto = "tcp"
			}
			if proto != "tcp" && proto != "udp" && proto != "sctp" {
				return fmt.Errorf("service %q: invalid protocol %q for port %d (must be tcp, udp, or sctp)", svc.Name, proto, port.Host)
			}
			// Port collision detection (protocol-aware: tcp/80 and udp/80 can coexist)
			key := fmt.Sprintf("%d:%s", port.Host, proto)
			if existing, collision := seenHostPorts[key]; collision {
				return fmt.Errorf("port collision: service %q and service %q both use host port %d/%s", svc.Name, existing, port.Host, proto)
			}
			seenHostPorts[key] = svc.Name
		}

		// Volume validation
		for _, vol := range svc.Volumes {
			if vol.Source == "" {
				return fmt.Errorf("service %q: volume source is required", svc.Name)
			}
			if vol.Target == "" {
				return fmt.Errorf("service %q: volume target is required", svc.Name)
			}
			if !validVolumeTypes[vol.Type] {
				return fmt.Errorf("service %q: invalid volume type %q (must be one of: bind, volume, tmpfs)", svc.Name, vol.Type)
			}
		}

		// Environment variable validation
		for _, env := range svc.Env {
			if env.Name == "" {
				return fmt.Errorf("service %q: environment variable name is required", svc.Name)
			}
		}

		// Secret reference validation
		for _, secret := range svc.Secrets {
			if secret.Name == "" {
				return fmt.Errorf("service %q: secret name is required", svc.Name)
			}
			if secret.SecretRef == "" {
				return fmt.Errorf("service %q: secret_ref is required", svc.Name)
			}
		}

		// Health check validation
		if svc.HealthCheck != nil {
			hc := svc.HealthCheck
			if !validHealthCheckTypes[hc.Type] {
				return fmt.Errorf("service %q: invalid healthcheck type %q (must be one of: http, tcp, exec)", svc.Name, hc.Type)
			}
			if hc.Interval == "" {
				return fmt.Errorf("service %q: healthcheck interval is required", svc.Name)
			}
			if hc.Type == "http" && hc.Path == "" {
				return fmt.Errorf("service %q: healthcheck path is required for http type", svc.Name)
			}
			if (hc.Type == "http" || hc.Type == "tcp") && hc.Port <= 0 {
				return fmt.Errorf("service %q: healthcheck port is required for %s type", svc.Name, hc.Type)
			}
		}

		// User format validation (must be "uid:gid" or empty)
		if svc.User != "" {
			parts := strings.Split(svc.User, ":")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("service %q: invalid user format %q (must be 'uid:gid')", svc.Name, svc.User)
			}
		}

		// Network profile validation
		if svc.Network != "" {
			validProfiles := map[string]bool{"public": true, "internal": true, "vpn": true}
			if !validProfiles[strings.ToLower(svc.Network)] {
				return fmt.Errorf("service %q: invalid network %q (must be one of: public, internal, vpn)", svc.Name, svc.Network)
			}
		}
	}

	// Validate DependsOn references (must happen after all services are registered)
	for _, svc := range stack.Spec.Services {
		for _, dep := range svc.DependsOn {
			if !seenNames[dep] {
				return fmt.Errorf("service %q: depends_on %q references an unknown service", svc.Name, dep)
			}
			if dep == svc.Name {
				return fmt.Errorf("service %q: cannot depend on itself", svc.Name)
			}
		}
	}

	return nil
}

// validateImage checks that a container image reference is syntactically valid.
func validateImage(image string) error {
	if !imageRegex.MatchString(image) {
		return fmt.Errorf("does not match valid image reference format")
	}
	if strings.Contains(image, " ") {
		return fmt.Errorf("image reference contains spaces")
	}
	if strings.Count(image, ":") > 1 {
		return fmt.Errorf("image reference contains multiple colons")
	}
	return nil
}
