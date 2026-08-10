// Package types defines the core data model for Quartermaster: stacks,
// services, networking, volumes, health checks, GPU resources, and ingress.
// These types are shared across all packages and serialized to/from YAML.
package types

// Stack represents the top-level structure of a quartermaster configuration.
type Stack struct {
	Version  string    `yaml:"version"`
	Kind     string    `yaml:"kind"`
	Metadata Metadata  `yaml:"metadata"`
	Spec     StackSpec `yaml:"spec"`
}

// Metadata contains descriptive information about the stack.
type Metadata struct {
	Name string `yaml:"name"`
}

// StackSpec defines the desired state of the services in the stack.
type StackSpec struct {
	Services []Service `yaml:"services"`
}

// Service defines an individual workload/container to be managed.
type Service struct {
	Name          string         `yaml:"name"                    json:"name"`
	Image         string         `yaml:"image"                   json:"image"`
	RestartPolicy string         `yaml:"restart_policy"          json:"restart_policy"`
	Ports         []Port         `yaml:"ports,omitempty"         json:"ports,omitempty"`
	Volumes       []Volume       `yaml:"volumes,omitempty"       json:"volumes,omitempty"`
	Env           []EnvVar       `yaml:"env,omitempty"           json:"env,omitempty"`
	Secrets       []SecretRef    `yaml:"secrets,omitempty"       json:"secrets,omitempty"`
	Network       string         `yaml:"network,omitempty"       json:"network,omitempty"`
	User          string         `yaml:"user,omitempty"          json:"user,omitempty"`
	DependsOn     []string       `yaml:"depends_on,omitempty"    json:"depends_on,omitempty"`
	HealthCheck   *HealthCheck   `yaml:"healthcheck,omitempty"   json:"healthcheck,omitempty"`
	Command       []string       `yaml:"command,omitempty"       json:"command,omitempty"`
	Resources     *Resources     `yaml:"resources,omitempty"     json:"resources,omitempty"`
	Ingress       *IngressConfig `yaml:"ingress,omitempty"       json:"ingress,omitempty"`

	// RestartAt defines a periodic scheduled restart (e.g. daily at 3am).
	// The container is stopped and redeployed, pulling the latest image.
	// Use with rolling_update to avoid downtime during the restart.
	RestartAt *RestartAt `yaml:"restart_at,omitempty"    json:"restart_at,omitempty"`

	// RegistryAuth provides credentials for pulling from a private registry.
	// References a qm secret (see RegistryAuth type docs).
	RegistryAuth *RegistryAuth `yaml:"registry_auth,omitempty"  json:"registry_auth,omitempty"`

	// RollingUpdate enables zero-downtime deploys: the new container is
	// created and health-checked before the old one is stopped.
	RollingUpdate bool `yaml:"rolling_update,omitempty" json:"rolling_update,omitempty"`

	// ConfigHash is an internal field set by the reconciler for change detection.
	// It is not serialized to YAML or JSON.
	ConfigHash string `yaml:"-" json:"-"`
}

// RegistryAuth holds authentication credentials for pulling images from
// a private registry.  The SecretRef points to a qm secret (encrypted with
// NaCl secretbox) whose plaintext is a JSON object with "username" and
// "password" keys.
//
// Example:
//
//	echo '{"username":"bot","password":"ghp_token"}' | qm secret create ghcr-creds
//
//	registry_auth:
//	  secret_ref: ghcr-creds
type RegistryAuth struct {
	SecretRef string `yaml:"secret_ref" json:"secret_ref"` // qm secret name
}

// RestartAt defines a periodic scheduled restart for a service.  The daemon
// checks every minute whether a restart window has opened and triggers a
// redeploy, pulling the latest image.
//
// Example (daily at 3am local time):
//
//	restart_at:
//	  time: "03:00"
//	  frequency: daily
type RestartAt struct {
	Time         string `yaml:"time"                   json:"time"`                     // "HH:MM" in 24h local time
	Frequency    string `yaml:"frequency"              json:"frequency"`                // "daily" (extensible: weekly, monthly)
	UpdatePolicy string `yaml:"update_policy,omitempty" json:"update_policy,omitempty"` // "always" (default) or "latest"
}

// Resources defines hardware constraints for a service.
type Resources struct {
	GPU *GPUResource `yaml:"gpu,omitempty" json:"gpu,omitempty"`
}

// GPUResource requests GPU access for a container.
type GPUResource struct {
	Type string `yaml:"type" json:"type"` // "nvidia" (default if empty)
}

// Port defines a port mapping between host and container.
type Port struct {
	Host      int    `yaml:"host"                json:"host"`
	Container int    `yaml:"container"           json:"container"`
	Protocol  string `yaml:"protocol,omitempty"  json:"protocol,omitempty"`
}

// Volume defines a volume mapping.  Type "configmap" mounts ConfigMap data
// as individual files at Target.
type Volume struct {
	Source    string           `yaml:"source,omitempty"    json:"source,omitempty"`
	Target    string           `yaml:"target"              json:"target"`
	Type      string           `yaml:"type"                json:"type"`
	ConfigMap *ConfigMapSource `yaml:"configMap,omitempty" json:"configMap,omitempty"`
}

// ConfigMapSource references a ConfigMap to mount as files.
type ConfigMapSource struct {
	Name string `yaml:"name" json:"name"`
}

// EnvVar defines an environment variable.  Value is the default.
// ValueFrom (secret or configmap ref) overrides Value when the
// referenced source exists and the key is found.
type EnvVar struct {
	Name      string          `yaml:"name"                json:"name"`
	Value     string          `yaml:"value,omitempty"     json:"value,omitempty"`
	ValueFrom *EnvValueSource `yaml:"valueFrom,omitempty" json:"valueFrom,omitempty"`
}

// EnvValueSource references a Secret or ConfigMap key to inject as an env var.
// Exactly one of SecretRef or ConfigMapRef must be set.
type EnvValueSource struct {
	SecretRef    string `yaml:"secretRef,omitempty"    json:"secretRef,omitempty"`
	ConfigMapRef string `yaml:"configMapRef,omitempty" json:"configMapRef,omitempty"`
	Key          string `yaml:"key,omitempty"          json:"key,omitempty"`
}

// SecretRef defines a reference to a secret managed by quartermaster.
type SecretRef struct {
	Name      string `yaml:"name"       json:"name"`
	SecretRef string `yaml:"secret_ref" json:"secret_ref"`
}

// HealthCheck defines how to verify if a service is running correctly.
type HealthCheck struct {
	Type     string `yaml:"type"               json:"type"`
	Path     string `yaml:"path,omitempty"      json:"path,omitempty"`
	Port     int    `yaml:"port,omitempty"      json:"port,omitempty"`
	Interval string `yaml:"interval"            json:"interval"`
}

// IngressConfig controls HTTP/HTTPS ingress via Caddy reverse proxy.
type IngressConfig struct {
	Host string `yaml:"host"           json:"host"`
	Port int    `yaml:"port"           json:"port"`
	Auth bool   `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// ── ConfigMap ───────────────────────────────────────────────────────────

// ConfigMap holds non-sensitive key-value configuration that can be injected
// into containers as environment variables or mounted as files.
type ConfigMap struct {
	Version  string            `yaml:"version"  json:"version"`
	Kind     string            `yaml:"kind"     json:"kind"`
	Metadata Metadata          `yaml:"metadata" json:"metadata"`
	Data     map[string]string `yaml:"data"     json:"data"`
}
