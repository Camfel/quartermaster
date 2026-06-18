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

	// ConfigHash is an internal field set by the reconciler for change detection.
	// It is not serialized to YAML or JSON.
	ConfigHash string `yaml:"-" json:"-"`
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

// Volume defines a volume mapping.
type Volume struct {
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
	Target string `yaml:"target"           json:"target"`
	Type   string `yaml:"type"             json:"type"`
}

// EnvVar defines an environment variable.
type EnvVar struct {
	Name  string `yaml:"name"            json:"name"`
	Value string `yaml:"value,omitempty" json:"value,omitempty"`
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
