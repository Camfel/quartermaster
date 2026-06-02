// Package network manages container network profiles.
//
// Three profiles are supported:
//
//	public   — standard bridge, host ports exposed (game servers, Jellyfin)
//	internal — standard bridge, NO host ports (databases, internal services)
//	vpn      — shares the network namespace of a VPN gateway container
//
// VPN namespace sharing is determined by depends_on: a service with
// network: vpn that depends_on another vpn service will join that
// service's network namespace.  The root vpn service (no vpn dependency)
// becomes the gateway automatically.
package network

import (
	"fmt"
	"strings"
)

// Profile is a named network profile.
type Profile string

const (
	ProfilePublic   Profile = "public"
	ProfileInternal Profile = "internal"
	ProfileVPN      Profile = "vpn"
)

// ValidProfile checks whether a profile name is recognised (case-insensitive).
func ValidProfile(p string) bool {
	switch NormaliseProfile(p) {
	case ProfilePublic, ProfileInternal, ProfileVPN:
		return true
	default:
		return false
	}
}

// NormaliseProfile returns the canonical Profile for a string.  An empty
// string maps to ProfilePublic for backward compatibility with stacks that
// don't specify a network.
func NormaliseProfile(p string) Profile {
	if p == "" {
		return ProfilePublic
	}
	return Profile(strings.ToLower(p))
}

// Manager tracks network profiles and resolves VPN gateway PIDs.
type Manager struct {
	// gatewayPID is the PID of the VPN gateway container's task, or 0 if
	// no gateway is registered yet.
	gatewayPID uint32
}

// NewManager creates a new network manager.
func NewManager() *Manager {
	return &Manager{}
}

// RegisterVPNGateway records the PID of the VPN gateway container.
// Called by the reconciler after creating the root vpn service.
func (m *Manager) RegisterVPNGateway(pid uint32) {
	m.gatewayPID = pid
}

// UnregisterVPNGateway clears the VPN gateway registration.
func (m *Manager) UnregisterVPNGateway() {
	m.gatewayPID = 0
}

// GatewayPID returns the PID of the VPN gateway, and whether one is
// registered.
func (m *Manager) GatewayPID() (uint32, bool) {
	return m.gatewayPID, m.gatewayPID != 0
}

// NetworkNamespacePath returns the /proc path to the network namespace
// of a process, for use with OCI namespace sharing.
func NetworkNamespacePath(pid uint32) string {
	return fmt.Sprintf("/proc/%d/ns/net", pid)
}

// ResolveProfile determines what to do for a given profile and dependency
// list.
//
//	isGateway is true when this service should be registered as the VPN
//	         gateway (first vpn service with no vpn dependency).
//	sharePID  is the non-zero PID of the gateway whose namespace this
//	         service should join.
func (m *Manager) ResolveProfile(network string, dependsOn []string, runningServices map[string]uint32) (isGateway bool, sharePID uint32, err error) {
	p := NormaliseProfile(network)

	switch p {
	case ProfilePublic, ProfileInternal:
		return false, 0, nil

	case ProfileVPN:
		// Walk depends_on to find a vpn service that's already running.
		for _, dep := range dependsOn {
			if pid, ok := runningServices[dep]; ok && pid != 0 {
				return false, pid, nil // dependent — share the dependency's namespace
			}
		}
		// No running vpn dependency — this service IS the gateway.
		return true, 0, nil

	default:
		return false, 0, fmt.Errorf("unknown network profile: %q", network)
	}
}
