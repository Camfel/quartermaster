// Package health performs runtime health checks against running containers.
// Supports HTTP and TCP probe types. Results are used by the daemon to
// detect crashed or unhealthy containers and trigger restarts or rollbacks.
package health

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"quartermaster/pkg/types"
)

// Result represents the outcome of a health check probe.
type Result struct {
	ServiceName string
	Healthy     bool
	Error       error
	Type        string // "http" or "tcp"
	Duration    time.Duration
}

// Checker performs health checks against running containers.
type Checker struct {
	httpClient *http.Client
}

// NewChecker creates a new health Checker with sensible defaults.
func NewChecker() *Checker {
	return &Checker{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return nil // follow redirects
			},
		},
	}
}

// RunCheck executes a single health check against a container using the
// service's HealthCheck spec. For HTTP and TCP checks, it probes localhost
// on the first mapped host port (or the healthcheck port if specified).
func (c *Checker) RunCheck(svc types.Service) Result {
	hc := svc.HealthCheck
	start := time.Now()

	if hc == nil {
		return Result{ServiceName: svc.Name, Healthy: true, Type: "none"}
	}

	switch hc.Type {
	case "http":
		err := c.checkHTTP(svc)
		return Result{
			ServiceName: svc.Name,
			Healthy:     err == nil,
			Error:       err,
			Type:        "http",
			Duration:    time.Since(start),
		}
	case "tcp":
		err := c.checkTCP(svc)
		return Result{
			ServiceName: svc.Name,
			Healthy:     err == nil,
			Error:       err,
			Type:        "tcp",
			Duration:    time.Since(start),
		}
	default:
		return Result{
			ServiceName: svc.Name,
			Healthy:     false,
			Error:       fmt.Errorf("unknown health check type: %s", hc.Type),
			Type:        hc.Type,
			Duration:    time.Since(start),
		}
	}
}

// checkHTTP performs an HTTP GET request against the container's health endpoint.
// It uses the first mapped host port, or the healthcheck port if specified.
func (c *Checker) checkHTTP(svc types.Service) error {
	hc := svc.HealthCheck
	port := resolvePort(svc, hc)
	url := fmt.Sprintf("http://localhost:%d%s", port, hc.Path)

	resp, err := c.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("HTTP probe failed for %s: %w", svc.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP probe for %s returned status %d", svc.Name, resp.StatusCode)
	}

	return nil
}

// checkTCP attempts a TCP connection to the container's port.
func (c *Checker) checkTCP(svc types.Service) error {
	hc := svc.HealthCheck
	port := resolvePort(svc, hc)
	addr := fmt.Sprintf("localhost:%d", port)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("TCP probe failed for %s: %w", svc.Name, err)
	}
	conn.Close()
	return nil
}

// resolvePort returns the port to use for health checks.
// Priority: healthcheck port > first host-mapped port > 80.
func resolvePort(svc types.Service, hc *types.HealthCheck) int {
	if hc.Port > 0 {
		return hc.Port
	}
	if len(svc.Ports) > 0 {
		return svc.Ports[0].Host
	}
	return 80
}

// RunAll executes health checks for all services that have a healthcheck configured.
// Returns results for services with health checks only.
func (c *Checker) RunAll(services []types.Service) []Result {
	var results []Result
	for _, svc := range services {
		if svc.HealthCheck != nil {
			results = append(results, c.RunCheck(svc))
		}
	}
	return results
}

// ParseInterval parses a health check interval string (e.g., "10s", "1m")
// and returns the duration. Returns 30s as default if parsing fails.
func ParseInterval(interval string) time.Duration {
	if interval == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(interval)
	if err != nil {
		return 30 * time.Second
	}
	if d < time.Second {
		return time.Second
	}
	return d
}
