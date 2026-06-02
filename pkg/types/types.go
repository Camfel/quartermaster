package types

// Stack represents the top-level structure of a quartermaster configuration.
type Stack struct {
	Version  string      `yaml:"version"`
	Kind     string      `yaml:"kind"`
	Metadata Metadata    `yaml:"metadata"`
	Spec     StackSpec   `yaml:"spec"`
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
	Name           string            `yaml:"name"`
	Image          string            `yaml:"image"`
	RestartPolicy  string            `yaml:"restart_policy"`
	Ports          []Port            `yaml:"ports,omitempty"`
	Volumes        []Volume          `yaml:"volumes,omitempty"`
	Resources      *Resources        `yaml:"resources,omitempty"`
	Env            []EnvVar          `yaml:"env,omitempty"`
	Secrets        []SecretRef       `yaml:"secrets,omitempty"`
	Network        string            `yaml:"network,omitempty"`
	User           string            `yaml:"user,omitempty"`
	DependsOn      []string          `yaml:"depends_on,omitempty"`
	HealthCheck    *HealthCheck      `yaml:"healthcheck,omitempty"`

	// ConfigHash is an internal field set by the reconciler for change detection.
	// It is not serialized to YAML.
	ConfigHash string `yaml:"-" json:"-"`
}

// Port defines a port mapping between host and container.
type Port struct {
	Host      int    `yaml:"host"`
	Container int    `yaml:"container"`
	Protocol  string `yaml:"protocol,omitempty"` // "tcp", "udp", "sctp". Default "tcp" when empty.
}

// Volume defines a volume mapping.
type Volume struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
	Type   string `yaml:"type"` // e.g., bind, volume
}

// Resources defines hardware constraints for a service.
type Resources struct {
	GPU *GPUResource `yaml:"gpu,omitempty"`
}

// GPUResource defines GPU requirements.
type GPUResource struct {
	Type string `yaml:"type"` // e.g., nvidia
	ID   string `yaml:"id"`   // e.g., "all" or a specific UUID
}

// EnvVar defines an environment variable.
type EnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// SecretRef defines a reference to a secret managed by quartermaster.
type SecretRef struct {
	Name      string `yaml:"name"`
	SecretRef string `yaml:"secret_ref"`
}

// HealthCheck defines how to verify if a service is running correctly.
type HealthCheck struct {
	Type     string `yaml:"type"` // e.g., http, tcp, exec
	Path     string `yaml:"path,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Interval string `yaml:"interval"`
}
