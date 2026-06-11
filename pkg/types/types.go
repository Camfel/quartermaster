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
	Name          string         `yaml:"name"            json:"name"`
	Image         string         `yaml:"image"           json:"image"`
	RestartPolicy string         `yaml:"restart_policy"  json:"restart_policy"`
	Ports         []Port         `yaml:"ports,omitempty" json:"ports,omitempty"`
	Volumes       []Volume       `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Resources     *Resources     `yaml:"resources,omitempty" json:"resources,omitempty"`
	Env           []EnvVar       `yaml:"env,omitempty"   json:"env,omitempty"`
	Secrets       []SecretRef    `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Network       string         `yaml:"network,omitempty" json:"network,omitempty"`
	User          string         `yaml:"user,omitempty"  json:"user,omitempty"`
	DependsOn     []string       `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	HealthCheck   *HealthCheck   `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
	Expose        *ExposeConfig  `yaml:"expose,omitempty" json:"expose,omitempty"`
	Ingress       *IngressConfig `yaml:"ingress,omitempty" json:"ingress,omitempty"`
	Log           *LogConfig     `yaml:"log,omitempty"     json:"log,omitempty"`
	Command       []string       `yaml:"command,omitempty" json:"command,omitempty"`

	// ConfigHash is an internal field set by the reconciler for change detection.
	// It is not serialized to YAML or JSON.
	ConfigHash string `yaml:"-" json:"-"`
}

// Port defines a port mapping between host and container.
type Port struct {
	Host      int    `yaml:"host"      json:"host"`
	Container int    `yaml:"container" json:"container"`
	Protocol  string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
}

// Volume defines a volume mapping.  Type "configmap" mounts ConfigMap data
// as individual files at Target.
type Volume struct {
	Source    string           `yaml:"source,omitempty"    json:"source,omitempty"`
	Target    string           `yaml:"target"              json:"target"`
	Type      string           `yaml:"type"                json:"type"` // bind, volume, tmpfs, configmap
	ConfigMap *ConfigMapSource `yaml:"configMap,omitempty" json:"configMap,omitempty"`
}

// ConfigMapSource references a ConfigMap to mount as files.
type ConfigMapSource struct {
	Name string `yaml:"name" json:"name"`
}

// Resources defines hardware constraints for a service.
type Resources struct {
	GPU *GPUResource `yaml:"gpu,omitempty" json:"gpu,omitempty"`
}

// GPUResource defines GPU requirements.
type GPUResource struct {
	Type string `yaml:"type" json:"type"`
	ID   string `yaml:"id"   json:"id"`
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

// ── Expose ─────────────────────────────────────────────────────────────

// ExposeConfig controls how a service is exposed via Tailscale.
//
//	tailscale — iptables DNAT in the tailscale container; tailnet members
//	            can access the service at <tailscale-ip>:<port>.
//	serve     — tailscale serve (HTTPS reverse proxy with auto TLS cert);
//	            tailnet members access via https://<host>.ts.net.
//	funnel    — tailscale funnel (like serve but publicly accessible on
//	            the internet, no port forwarding required).
type ExposeConfig struct {
	Type  string `yaml:"type"            json:"type"`            // tailscale, serve, funnel
	Name  string `yaml:"name,omitempty"  json:"name,omitempty"`  // path prefix for serve/funnel (e.g. "jellyfin")
	Ports []int  `yaml:"ports,omitempty" json:"ports,omitempty"` // for tailscale type
	Port  int    `yaml:"port,omitempty"  json:"port,omitempty"`  // for serve/funnel type
}

// ── Ingress ─────────────────────────────────────────────────────────────

// IngressConfig controls HTTP/HTTPS ingress via Caddy reverse proxy.
//
//	host  — domain name (e.g. "jellyfin.boon.blue").  Caddy handles TLS
//	        automatically via Let's Encrypt.
//	port  — the container port to proxy traffic to.
//	auth  — if true, protect with Authelia forward_auth.
type IngressConfig struct {
	Host string `yaml:"host"           json:"host"`
	Port int    `yaml:"port"           json:"port"`
	Auth bool   `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// ── Logging ────────────────────────────────────────────────────────────

// LogConfig controls persistent container log capture and rotation.
// When enabled, container stdout/stderr is written to rotating log files
// at /var/lib/quartermaster/logs/<service-name>/current.log in addition
// to the in-memory ring buffer used by the status API.
//
//	max_size  — rotate when the current log file exceeds this size
//	            (e.g. "10MB", "100MB").  Default: "50MB".
//	max_files — number of rotated log files to keep.  Default: 5.
type LogConfig struct {
	Enabled  bool   `yaml:"enabled"             json:"enabled"`
	MaxSize  string `yaml:"max_size,omitempty"  json:"max_size,omitempty"`
	MaxFiles int    `yaml:"max_files,omitempty" json:"max_files,omitempty"`
}

// ── ConfigMap ───────────────────────────────────────────────────────────

// ConfigMap holds non-sensitive key-value configuration that can be injected
// into containers as environment variables or mounted as files.
type ConfigMap struct {
	Version  string            `yaml:"version"  json:"version"`
	Kind     string            `yaml:"kind"     json:"kind"` // must be "ConfigMap"
	Metadata Metadata          `yaml:"metadata" json:"metadata"`
	Data     map[string]string `yaml:"data"     json:"data"`
}
