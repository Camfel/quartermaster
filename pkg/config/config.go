// Package config loads, validates, and persists Quartermaster stack manifests
// and daemon settings.  It is the single entry point for all configuration I/O
// — YAML stacks, JSON settings, and component definitions.
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"quartermaster/pkg/types"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

// ConfigManager handles loading and validating configurations.
type ConfigManager struct{}

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

// MergeStacks combines two stacks into one.  Services from the second stack
// are appended to the first, and the first stack's metadata is preserved.
// On a duplicate service name the later (additional) stack wins, so user
// repos override component defaults (see Settings.StackFiles ordering).
func (cm *ConfigManager) MergeStacks(base, additional *types.Stack) *types.Stack {
	// Later stacks win on service name conflicts.  StackFiles() orders
	// component stacks first and user repos last, so repo overrides replace
	// component defaults rather than being silently dropped.
	for i := range base.Spec.Services {
		for _, svc := range additional.Spec.Services {
			if svc.Name == base.Spec.Services[i].Name {
				base.Spec.Services[i] = svc
				break
			}
		}
	}

	seen := make(map[string]bool, len(base.Spec.Services))
	for _, svc := range base.Spec.Services {
		seen[svc.Name] = true
	}
	for _, svc := range additional.Spec.Services {
		if !seen[svc.Name] {
			base.Spec.Services = append(base.Spec.Services, svc)
			seen[svc.Name] = true
		}
	}
	return base
}

// validRestartPolicies defines the allowed restart policy values.
var validRestartPolicies = map[string]bool{
	"always":         true,
	"unless-stopped": true,
	"on-failure":     true,
	"no":             true,
	"":               true, // empty defaults to the runtime default
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
	// "exec" is intentionally unsupported: pkg/health does not implement exec
	// probes, so accepting it made every probe report the service unhealthy
	// and drove repeated restarts (and now LKG rollbacks).
}

// serviceNameRegex restricts service names to a conservative charset.  A name
// becomes a container name, a netns/veth prefix, a log file name, and is
// rendered in the dashboard, so spaces and punctuation are both unsafe and a
// stored-XSS vector.
var serviceNameRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_-]{0,61}[a-zA-Z0-9])?$`)

// sensitiveHostDirs are directory trees that must never be bind-mounted into a
// container: they grant root-equivalent control of the host or expose
// credentials.
var sensitiveHostDirs = []string{
	"/proc",
	"/sys",
	"/boot",
	"/root",
	"/etc/quartermaster",
	"/etc/ssh",
	"/run/containerd",
	"/var/run/containerd",
}

// sensitiveHostFiles are individual host files that must never be bind-mounted.
// Note that /dev itself is on this list, but device nodes such as /dev/dri or
// /dev/nvidia* are intentionally allowed for GPU workloads.
var sensitiveHostFiles = map[string]bool{
	"/dev":         true,
	"/dev/mem":     true,
	"/dev/kmem":    true,
	"/dev/kmsg":    true,
	"/dev/port":    true,
	"/etc/shadow":  true,
	"/etc/sudoers": true,
}

// containerRuntimeSockets are host sockets that let a container talk to the
// container runtime directly, which is equivalent to host root.
var containerRuntimeSockets = map[string]bool{
	"/run/docker.sock":                    true,
	"/var/run/docker.sock":                true,
	"/run/containerd/containerd.sock":     true,
	"/var/run/containerd/containerd.sock": true,
	"/run/podman/podman.sock":             true,
}

// validateHostMountPath rejects bind-mount sources that would break host
// isolation.  The path is cleaned first so "/var/../etc/shadow" cannot
// bypass the checks.
func validateHostMountPath(p string) error {
	clean := filepath.Clean(p)
	if clean == "/" {
		return fmt.Errorf("refusing to bind-mount the host root filesystem")
	}
	if containerRuntimeSockets[clean] {
		return fmt.Errorf("refusing to bind-mount container runtime socket %q", clean)
	}
	if sensitiveHostFiles[clean] {
		return fmt.Errorf("refusing to bind-mount sensitive host path %q", clean)
	}
	for _, sensitive := range sensitiveHostDirs {
		if clean == sensitive || strings.HasPrefix(clean, sensitive+"/") {
			return fmt.Errorf("refusing to bind-mount sensitive host path %q", clean)
		}
	}
	return nil
}

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

		// Name is required and must be unique. Restrict it to a conservative
		// charset: the name becomes a container name, netns/veth prefix, log
		// file, and is rendered in the dashboard.
		if svc.Name == "" {
			return fmt.Errorf("service at index %d: name is required", i)
		}
		if !serviceNameRegex.MatchString(svc.Name) {
			return fmt.Errorf("service at index %d: name %q must match %s", i, svc.Name, serviceNameRegex.String())
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
			// Bind mounts are attached read-write (see pkg/cri). Require an
			// absolute path and refuse host paths that grant the container
			// root-equivalent control or expose credentials.
			if vol.Type == "bind" || vol.Type == "" {
				if !filepath.IsAbs(vol.Source) {
					return fmt.Errorf("service %q: volume source %q must be an absolute path", svc.Name, vol.Source)
				}
				if err := validateHostMountPath(vol.Source); err != nil {
					return fmt.Errorf("service %q: %w", svc.Name, err)
				}
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

		// GPU validation
		if svc.Resources != nil && svc.Resources.GPU != nil {
			switch svc.Resources.GPU.Type {
			case "", "nvidia":
			default:
				return fmt.Errorf("service %q: unsupported GPU type %q (must be 'nvidia')", svc.Name, svc.Resources.GPU.Type)
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
			validProfiles := map[string]bool{"public": true, "internal": true, "vpn": true, "host": true}
			if !validProfiles[strings.ToLower(svc.Network)] {
				return fmt.Errorf("service %q: invalid network %q (must be one of: public, internal, vpn, host)", svc.Name, svc.Network)
			}
		}

		// Ingress validation
		if svc.Ingress != nil {
			if svc.Ingress.Host == "" {
				return fmt.Errorf("service %q: ingress host is required", svc.Name)
			}
			if svc.Ingress.Port <= 0 {
				return fmt.Errorf("service %q: ingress port is required", svc.Name)
			}
		}
	}

	// Validate DependsOn references (must happen after all services are registered).
	// Missing references are logged as warnings rather than errors — services may
	// come from other stacks (e.g. gluetun from the VPN component referenced by
	// media-stack services).
	for _, svc := range stack.Spec.Services {
		for _, dep := range svc.DependsOn {
			if !seenNames[dep] {
				log.Printf("Warning: service %q depends_on %q which is in another stack or not yet defined", svc.Name, dep)
			}
			if dep == svc.Name {
				return fmt.Errorf("service %q: cannot depend on itself", svc.Name)
			}
		}
	}

	return nil
}

// validateImage checks that a container image reference is syntactically valid.
// It uses the same parser as the container runtime, so registry hosts with a
// port (e.g. localhost:5000/app:tag) and digest references are accepted.
func validateImage(image string) error {
	if _, err := reference.ParseNormalizedNamed(image); err != nil {
		return fmt.Errorf("invalid image reference: %w", err)
	}
	return nil
}
