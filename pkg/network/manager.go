// Package network manages container network profiles.
//
// Three profiles are supported:
//
//	public   — host networking (container shares host netns)
//	internal — own netns on the qm0 bridge, no host ports exposed
//	vpn      — own netns on the qm0 bridge + CAP_NET_ADMIN; optionally routes
//	           egress through a VPN gateway via policy routing
//
// VPN routing uses Linux policy routing instead of PID-based namespace
// sharing: each vpn-dependent service gets a second routing table that
// sends all traffic through the VPN gateway's bridge IP.  This eliminates
// PID tracking, /proc/<pid>/ns/net lookups, namespace sharing, firewall
// flushing, and all the restart-recovery hacks that approach required.
package network

import (
	"fmt"
	"net"
	"strings"

	"quartermaster/pkg/types"
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

// ── NetInfo ─────────────────────────────────────────────────────────────

// NetInfo is returned by Attach and describes the network configuration
// the container runtime should apply.
type NetInfo struct {
	// NSPath is the path to a pre-created network namespace, or empty
	// for host-networked containers.
	NSPath string

	// IP is the bridge IP assigned to this container, or nil for
	// public (host-networked) containers.
	IP net.IP
}

// ── TailscaleExposure ──────────────────────────────────────────────────

// TailscaleExposure describes a service to expose via a tailscale container.
type TailscaleExposure struct {
	ServiceName string             // name of the target service
	ServiceIP   net.IP             // bridge IP of the target service
	Expose      types.ExposeConfig // the exposure configuration
	Ports       []types.Port       // the service's declared ports
}

// ── NetManager interface ────────────────────────────────────────────────

// NetManager handles all container networking: bridge setup, namespace
// creation, IPAM, port forwarding, and VPN policy routing.
type NetManager interface {
	// Setup creates the bridge, enables forwarding, and configures NAT.
	// Idempotent; safe to call multiple times.
	Setup() error

	// Attach creates a network namespace for the service and wires it
	// to the bridge.  For host-networked services (public), returns an
	// empty NetInfo and performs no setup.
	//
	// vpnGateway is the name of the VPN gateway service whose bridge IP
	// this service should route egress through, or empty if this is the
	// gateway itself or a non-VPN service.
	Attach(serviceName string, profile string, vpnGateway string) (NetInfo, error)

	// Detach removes the namespace, veth pair, DNAT rules, and any VPN
	// policy routes associated with the service.
	Detach(serviceName string, profile string) error

	// ExposePorts adds iptables DNAT rules forwarding host ports to the
	// container's bridge IP.  Rules are tracked for cleanup on Detach.
	ExposePorts(containerName string, containerIP net.IP, ports []types.Port)

	// ConfigureVPNGateway enters the VPN gateway's network namespace
	// and configures it to forward traffic from the bridge subnet through
	// the VPN tunnel.  Must be called after the gateway container has
	// started (so its own tunnel interface and firewall are initialised).
	ConfigureVPNGateway(serviceName string) error

	// ConfigureTailscale configures a tailscale container to expose the
	// given services.  For "tailscale" type it adds iptables DNAT rules.
	// For "serve" and "funnel" types it additionally runs tailscale CLI
	// commands via the provided exec function.
	ConfigureTailscale(serviceName string, exposures []TailscaleExposure, execFn func(cmd ...string) error) error

	// LookupIP returns the bridge IP for a running service, or nil if
	// the service uses host networking or is not known.
	LookupIP(serviceName string) net.IP

	// Recover repopulates the IP map from persistent state after a daemon
	// restart.  Must be called after Setup and before Attach/LookupIP.
	Recover() error

	// StartDNS starts the in-process DNS forwarder on the bridge gateway IP.
	// Must be called after Setup() so the bridge IP is assigned.
	StartDNS() error

	// StopDNS stops the DNS forwarder.
	StopDNS() error

	// UpdateDNSGateway notifies the DNS forwarder that a gateway's bridge IP
	// changed.  Used for live DNS updates when gluetun restarts.
	UpdateDNSGateway(gatewayName string, newIP net.IP)

	// UpdateGatewayRoute replaces the fwmark routing table's default route
	// when the VPN gateway's bridge IP changes.  Only the single shared
	// route in table 100 is updated; no container recreates are needed.
	UpdateGatewayRoute(gatewayIP string) error

	// RecoverVPNRouting re-applies fwmark-based VPN routing for all existing
	// containers after a daemon restart.  Called during recovery to ensure
	// the mangle mark rules and host-level fwmark infrastructure are present
	// even for containers created before a Network v2 upgrade.
	RecoverVPNRouting(vpnGateway string) error

	// Teardown removes the bridge and all iptables rules.
	Teardown() error
}

// ── Profile resolution ──────────────────────────────────────────────────

// ResolveProfile determines the network role for a service.
//
//	isGateway is true when this service should act as the VPN gateway
//	         (first vpn service with no running vpn dependency).
//	gatewayName is the name of the VPN gateway this service should route
//	            through, or empty if it IS the gateway or not a vpn service.
func ResolveProfile(network string, dependsOn []string, runningBridgeIPs map[string]string) (isGateway bool, gatewayName string, err error) {
	p := NormaliseProfile(network)

	switch p {
	case ProfilePublic, ProfileInternal:
		return false, "", nil

	case ProfileVPN:
		for _, dep := range dependsOn {
			if _, ok := runningBridgeIPs[dep]; ok {
				return false, dep, nil
			}
		}
		return true, "", nil

	default:
		return false, "", fmt.Errorf("unknown network profile: %q", network)
	}
}
