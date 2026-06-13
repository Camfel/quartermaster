// Package network — bridge.go
//
// BridgeManager implements NetManager.  It creates a Linux bridge (qm0) with
// NAT for non-host-networked containers, manages IPAM, port forwarding via
// iptables DNAT, and VPN egress via Linux policy routing.
package network

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"quartermaster/pkg/types"
)

const bridgeName = "qm0"
const bridgeSubnet = "10.42.0.0/24"
const bridgeGW = "10.42.0.1"
const vpnRouteTable = 100

// ipsFile persists the service→IP mapping across daemon restarts.
const ipsFile = "/var/lib/quartermaster/bridge-ips.json" // routing table ID for VPN egress

// BridgeManager implements NetManager.
type BridgeManager struct {
	mu     sync.Mutex
	setup  bool
	nextIP byte
	ipt4   *iptables.IPTables
	brLink netlink.Link
	dns    *DNSForwarder

	// ips tracks service-name → bridge IP for LookupIP and DNAT cleanup.
	ips map[string]net.IP
}

// NewBridgeManager creates a BridgeManager with defaults.
func NewBridgeManager() *BridgeManager {
	ipt, _ := iptables.New()
	return &BridgeManager{
		nextIP: 2,
		ipt4:   ipt,
		ips:    make(map[string]net.IP),
	}
}

// ── Setup / Teardown ────────────────────────────────────────────────────

// Setup creates the qm0 bridge, assigns the gateway IP, enables forwarding,
// and adds a masquerade rule for outbound NAT.  Idempotent.
func (b *BridgeManager) Setup() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.setup {
		return nil
	}

	// ── 1. Create the bridge ──────────────────────────────────────
	la := netlink.NewLinkAttrs()
	la.Name = bridgeName
	br := netlink.Link(&netlink.Bridge{LinkAttrs: la})
	if err := netlink.LinkAdd(br); err != nil {
		if !bridgeExists() {
			return fmt.Errorf("create bridge %s: %w", bridgeName, err)
		}
		log.Printf("Bridge %s already exists, reusing", bridgeName)
		br, err = netlink.LinkByName(bridgeName)
		if err != nil {
			return fmt.Errorf("look up existing bridge %s: %w", bridgeName, err)
		}
	}

	// ── 2. Assign gateway IP ──────────────────────────────────────
	gwAddr, _ := netlink.ParseAddr(bridgeGW + "/24")
	if err := netlink.AddrAdd(br, gwAddr); err != nil {
		if !addrExistsOnLink(bridgeName, bridgeGW) {
			return fmt.Errorf("assign IP to %s: %w", bridgeName, err)
		}
	}

	// ── 3. Bring up the bridge ────────────────────────────────────
	if err := netlink.LinkSetUp(br); err != nil {
		return fmt.Errorf("bring %s up: %w", bridgeName, err)
	}

	// ── 4. Enable IP forwarding ───────────────────────────────────
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)

	// ── 5. iptables masquerade for outbound NAT ───────────────────
	if b.ipt4 != nil {
		exists, _ := b.ipt4.Exists("nat", "POSTROUTING", "-s", bridgeSubnet, "!", "-o", bridgeName, "-j", "MASQUERADE")
		if !exists {
			if err := b.ipt4.Append("nat", "POSTROUTING", "-s", bridgeSubnet, "!", "-o", bridgeName, "-j", "MASQUERADE"); err != nil {
				return fmt.Errorf("iptables masquerade rule: %w", err)
			}
		}
	}

	// ── 6. Start DNS forwarder on the bridge gateway ─────────────
	b.dns = NewDNSForwarder(b.ips)
	if err := b.dns.Start(); err != nil {
		return fmt.Errorf("start DNS forwarder: %w", err)
	}

	b.setup = true
	b.brLink = br
	log.Printf("Bridge %s ready (subnet %s, gw %s, DNS on :53)", bridgeName, bridgeSubnet, bridgeGW)
	return nil
}

// Teardown removes the bridge and the masquerade rule.
func (b *BridgeManager) Teardown() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.setup {
		return nil
	}

	// ── Stop DNS forwarder ──────────────────────────────────────
	if b.dns != nil {
		b.dns.Stop()
		b.dns = nil
	}

	if b.ipt4 != nil {
		b.ipt4.Delete("nat", "POSTROUTING", "-s", bridgeSubnet, "!", "-o", bridgeName, "-j", "MASQUERADE")
	}

	br, err := netlink.LinkByName(bridgeName)
	if err == nil {
		netlink.LinkSetDown(br)
		netlink.LinkDel(br)
	}

	b.setup = false
	b.ips = make(map[string]net.IP)
	os.Remove(ipsFile)
	log.Printf("Bridge %s removed", bridgeName)
	return nil
}

// ── Attach / Detach ─────────────────────────────────────────────────────

// Attach implements NetManager.  For public (host-networked) services it
// returns an empty NetInfo.  For internal and vpn services it creates a
// network namespace, wires it to the bridge, assigns an IP, and configures
// DNS.  For vpn services with a gateway, it adds policy routing.
func (b *BridgeManager) Attach(serviceName string, profile string, vpnGateway string) (NetInfo, error) {
	p := NormaliseProfile(profile)
	if p == ProfilePublic {
		return NetInfo{}, nil
	}

	// ── Allocate IP ───────────────────────────────────────────────
	b.mu.Lock()
	ip := net.ParseIP(fmt.Sprintf("10.42.0.%d", b.nextIP))
	b.nextIP++
	b.ips[serviceName] = ip
	b.saveIPs()
	b.mu.Unlock()

	ipStr := ip.String()
	short := ShortName(serviceName)
	nsName := "qm-" + short
	hostVeth := "veth-h-" + short
	ctrVeth := "veth-c-" + short
	nsPath := "/var/run/netns/" + nsName

	// ── 0. Clean up any stale namespace ───────────────────────────
	netns.DeleteNamed(nsName)
	syscall.Unmount(nsPath, syscall.MNT_DETACH)
	os.Remove(nsPath)

	// Save the host network namespace.
	origNs, _ := netns.Get()
	defer origNs.Close()

	// ── 1. Create a new network namespace ─────────────────────────
	nsFd, err := netns.NewNamed(nsName)
	if err != nil {
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("create netns %s: %w", nsName, err)
	}
	nsFd.Close()

	// Restore host namespace.
	if err := netns.Set(origNs); err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("restore host netns: %w", err)
	}

	// ── 2. Clean up stale veth pair from previous run ─────────────
	if hostLink, _ := netlink.LinkByName(hostVeth); hostLink != nil {
		netlink.LinkDel(hostLink)
	}

	// ── 3. Create veth pair ───────────────────────────────────────
	if b.brLink == nil {
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("bridge %s not initialised", bridgeName)
	}
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostVeth},
		PeerName:  ctrVeth,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("create veth pair %s/%s: %w", hostVeth, ctrVeth, err)
	}

	// ── 4. Attach host end to the bridge ──────────────────────────
	hostLink, err := netlink.LinkByName(hostVeth)
	if err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("find %s: %w", hostVeth, err)
	}
	if err := netlink.LinkSetMaster(hostLink, b.brLink); err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("attach %s to bridge: %w", hostVeth, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("bring %s up: %w", hostVeth, err)
	}

	// ── 5. Move container end into the named namespace ────────────
	ctrLink, err := netlink.LinkByName(ctrVeth)
	if err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("find %s: %w", ctrVeth, err)
	}
	targetNs, err := netns.GetFromName(nsName)
	if err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("open netns %s: %w", nsName, err)
	}
	if err := netlink.LinkSetNsFd(ctrLink, int(targetNs)); err != nil {
		targetNs.Close()
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("move %s to netns %s: %w", ctrVeth, nsName, err)
	}
	targetNs.Close()

	// ── 6. Configure namespace: IP, routes, loopback ──────────────
	if err := b.configureNamespace(nsName, ctrVeth, ipStr); err != nil {
		b.cleanupNamespace(nsName, hostVeth)
		b.mu.Lock()
		delete(b.ips, serviceName)
		b.mu.Unlock()
		return NetInfo{}, fmt.Errorf("configure netns %s: %w", nsName, err)
	}

	// ── 7. VPN policy routing ─────────────────────────────────────
	if p == ProfileVPN && vpnGateway != "" {
		gwIP := b.LookupIP(vpnGateway)
		if gwIP != nil {
			if err := b.setupVPNRouting(nsName, ipStr, gwIP.String(), ctrVeth); err != nil {
				log.Printf("Warning: VPN policy routing for %s failed: %v", serviceName, err)
			}
		} else {
			log.Printf("Warning: VPN gateway %s has no bridge IP — routing not configured", vpnGateway)
		}
	}

	log.Printf("Attached netns %s for %s (IP: %s, path: %s)", nsName, serviceName, ipStr, nsPath)
	return NetInfo{NSPath: nsPath, IP: ip}, nil
}

// Detach implements NetManager.  Removes the namespace, veth pair, DNAT
// rules, VPN policy routes, and releases the IP.
// Always attempts cleanup regardless of profile — checks for resources
// defensively so stale state is cleaned even after daemon restarts.
func (b *BridgeManager) Detach(serviceName string, profile string) error {

	short := ShortName(serviceName)
	nsName := "qm-" + short
	hostVeth := "veth-h-" + short
	nsPath := "/var/run/netns/" + nsName

	// ── Remove VPN policy routing ─────────────────────────────────
	b.mu.Lock()
	ip := b.ips[serviceName]
	delete(b.ips, serviceName)
	b.saveIPs()
	b.mu.Unlock()
	if ip != nil {
		b.teardownVPNRouting(nsName, ip.String())
	}

	// ── Remove DNAT rules ─────────────────────────────────────────
	b.removePorts(serviceName, ip)

	// ── Tear down veth + netns ────────────────────────────────────
	netlink.LinkDel(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostVeth}})
	netns.DeleteNamed(nsName)
	syscall.Unmount(nsPath, syscall.MNT_DETACH)
	os.Remove(nsPath)
	os.RemoveAll(fmt.Sprintf("/etc/netns/%s", nsName))

	log.Printf("Detached netns %s for %s", nsName, serviceName)
	return nil
}

// LookupIP implements NetManager.  Falls back to short-name lookup so
// IPs recovered from netns scanning (which only has short names) can be
// matched to full service names.
func (b *BridgeManager) LookupIP(serviceName string) net.IP {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ip := b.ips[serviceName]; ip != nil {
		return ip
	}
	return b.ips[ShortName(serviceName)]
}

// ── DNS delgation ──────────────────────────────────────────────────────

// StartDNS implements NetManager.
func (b *BridgeManager) StartDNS() error {
	if b.dns == nil {
		b.dns = NewDNSForwarder(b.ips)
	}
	return b.dns.Start()
}

// StopDNS implements NetManager.
func (b *BridgeManager) StopDNS() error {
	if b.dns == nil {
		return nil
	}
	return b.dns.Stop()
}

// UpdateDNSGateway implements NetManager.
func (b *BridgeManager) UpdateDNSGateway(gatewayName string, newIP net.IP) {
	if b.dns != nil {
		b.dns.UpdateGluetunIP(newIP)
	}
}

// Recover reads the persisted IP map from disk, falling back to scanning
// existing named network namespaces if the file doesn't exist (upgrade from
// older daemon versions that didn't persist the mapping).
// Must be called after Setup.
func (b *BridgeManager) Recover() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	data, err := os.ReadFile(ipsFile)
	if err == nil {
		var saved map[string]string
		if err := json.Unmarshal(data, &saved); err != nil {
			return fmt.Errorf("parse %s: %w", ipsFile, err)
		}
		maxByte := byte(2)
		for name, ipStr := range saved {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				continue
			}
			b.ips[name] = ip
			if ip4 := ip.To4(); ip4 != nil && ip4[3] > maxByte {
				maxByte = ip4[3]
			}
		}
		b.nextIP = maxByte + 1
		log.Printf("Recovered %d bridge IP(s) from %s (next IP: .%d)", len(b.ips), ipsFile, b.nextIP)

		// Re-apply fwmark VPN routing for existing containers after restart.
		if _, ok := b.ips["gluetun"]; ok {
			b.mu.Unlock()
			if err := b.RecoverVPNRouting("gluetun"); err != nil {
				log.Printf("Warning: recover VPN routing: %v", err)
			}
			b.mu.Lock()
		}

		// Clean stale DNAT rules pointing to IPs not in the current map.
		// Prevents accumulation of dead rules across daemon restarts.
		b.cleanStaleDNAT()

		return nil
	}

	// No persisted file — scan existing netns (upgrade path).
	return b.scanNetns()
}

// cleanStaleDNAT removes DNAT rules that point to bridge IPs not in the
// current IP map.  Prevents dead rules from accumulating after container
// recreates or IP changes.
func (b *BridgeManager) cleanStaleDNAT() {
	if b.ipt4 == nil {
		return
	}
	validIPs := make(map[string]bool)
	for _, ip := range b.ips {
		validIPs[ip.String()] = true
	}
	for _, chain := range []string{"PREROUTING", "OUTPUT"} {
		rules, err := b.ipt4.List("nat", chain)
		if err != nil {
			continue
		}
		for _, rule := range rules {
			if !strings.Contains(rule, "--to-destination") || !strings.Contains(rule, "10.42.0.") {
				continue
			}
			idx := strings.Index(rule, "--to-destination")
			if idx < 0 {
				continue
			}
			rest := strings.Fields(rule[idx:])
			if len(rest) < 2 {
				continue
			}
			dest := rest[1]
			if colon := strings.LastIndex(dest, ":"); colon > 0 {
				dest = dest[:colon]
			}
			if !validIPs[dest] {
				// Parse port and protocol from the iptables list output.
				// Format: "-A CHAIN -p tcp -m tcp --dport 9696 -j DNAT --to-destination 10.42.0.97:9696"
				proto := "tcp"
				if strings.Contains(rule, "-p udp") {
					proto = "udp"
				}
				dport := ""
				if dpIdx := strings.Index(rule, "--dport "); dpIdx >= 0 {
					dport = strings.Fields(rule[dpIdx:])[1]
				}
				if dport != "" {
					args := []string{"-p", proto, "--dport", dport, "-j", "DNAT", "--to-destination", rest[1]}
					if err := b.ipt4.Delete("nat", chain, args...); err != nil {
						log.Printf("Warning: cleanStaleDNAT %s: %v", chain, err)
					} else {
						log.Printf("Cleaned stale DNAT %s rule: %s (IP not in bridge map)", chain, dest)
					}
				}
			}
		}
	}
}

// RecoverVPNRouting re-applies the fwmark-based VPN routing for all
// existing containers after a daemon restart.  This ensures the mangle
// mark rules and host-level fwmark infrastructure are present even for
// containers that were created before the Network v2 upgrade.
func (b *BridgeManager) RecoverVPNRouting(vpnGateway string) error {
	log.Printf("RecoverVPNRouting: starting for gateway %s", vpnGateway)
	b.mu.Lock()
	gwIP := b.ips[vpnGateway]
	if gwIP == nil {
		gwIP = b.ips[ShortName(vpnGateway)]
	}
	b.mu.Unlock()

	if gwIP == nil {
		return fmt.Errorf("VPN gateway %s not found in IP map", vpnGateway)
	}

	// Ensure host-level fwmark routing infrastructure.
	if err := b.ensureFwmarkRouting(gwIP.String()); err != nil {
		return fmt.Errorf("ensure fwmark routing: %w", err)
	}

	// Re-apply mangle mark rule in every VPN container's namespace.
	b.mu.Lock()
	names := make([]string, 0, len(b.ips))
	for name := range b.ips {
		names = append(names, name)
	}
	b.mu.Unlock()

	for _, name := range names {
		nsName := "qm-" + ShortName(name)
		nsHandle, err := netns.GetFromName(nsName)
		if err != nil {
			continue
		}
		nsHandle.Close()

		runtime.LockOSThread()
		origNs, _ := netns.Get()
		targetNs, err := netns.GetFromName(nsName)
		if err != nil {
			runtime.UnlockOSThread()
			continue
		}
		netns.Set(targetNs)

		ipt, err := iptables.New()
		if err != nil {
			netns.Set(origNs)
			origNs.Close()
			targetNs.Close()
			runtime.UnlockOSThread()
			continue
		}

		// Rule 1: RETURN for established connections so response
		// packets to the host/laptop aren't routed through the VPN.
		ctArgs := []string{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"}
		if exists, _ := ipt.Exists("mangle", "OUTPUT", ctArgs...); !exists {
			ipt.Insert("mangle", "OUTPUT", 1, ctArgs...)
		}

		// Rule 2: mark new outbound connections for VPN routing.
		mangleArgs := []string{"!", "-d", bridgeSubnet, "-j", "MARK", "--set-mark", "100"}
		if exists, _ := ipt.Exists("mangle", "OUTPUT", mangleArgs...); !exists {
			if err := ipt.Insert("mangle", "OUTPUT", 2, mangleArgs...); err != nil {
				log.Printf("Warning: mangle insert for %s: %v", name, err)
			} else {
				log.Printf("Recovered VPN mangle rule for %s", name)
			}
		}
		netns.Set(origNs)
		origNs.Close()
		targetNs.Close()
		runtime.UnlockOSThread()
	}

	return nil
}

// scanNetns reads bridge IPs from existing named network namespaces.
// This is a fallback for upgrades from daemon versions that didn't
// persist the IP mapping.  The short name mapping means recovered
// names are the first 8 characters of the service name.
func (b *BridgeManager) scanNetns() error {
	entries, err := os.ReadDir("/var/run/netns")
	if err != nil {
		return nil // no netns directory, nothing to recover
	}

	maxByte := byte(2)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "qm-") {
			continue
		}
		short := strings.TrimPrefix(name, "qm-")
		ctrVeth := "veth-c-" + short

		handle, err := getHandle(name)
		if err != nil {
			continue
		}

		link, err := handle.LinkByName(ctrVeth)
		if err != nil {
			handle.Delete()
			continue
		}
		addrs, err := handle.AddrList(link, netlink.FAMILY_V4)
		handle.Delete()

		if err != nil || len(addrs) == 0 {
			continue
		}

		ip := addrs[0].IP
		b.ips[short] = ip
		if ip4 := ip.To4(); ip4 != nil && ip4[3] > maxByte {
			maxByte = ip4[3]
		}
		log.Printf("Recovered bridge IP: %s → %s", short, ip)
	}

	b.nextIP = maxByte + 1
	b.saveIPs() // persist for next restart
	log.Printf("Scanned netns: recovered %d bridge IP(s) (next IP: .%d)", len(b.ips), b.nextIP)
	return nil
}

// saveIPs persists the current IP map to disk.
func (b *BridgeManager) saveIPs() {
	saved := make(map[string]string, len(b.ips))
	for name, ip := range b.ips {
		saved[name] = ip.String()
	}
	data, err := json.Marshal(saved)
	if err != nil {
		log.Printf("Warning: failed to marshal bridge IPs: %v", err)
		return
	}
	os.MkdirAll(filepath.Dir(ipsFile), 0755)
	if err := os.WriteFile(ipsFile, data, 0644); err != nil {
		log.Printf("Warning: failed to persist bridge IPs: %v", err)
	}
}

// ConfigureVPNGateway enters the gateway's network namespace and configures
// it to accept and NAT forwarded traffic from the bridge subnet.  This is
// needed because VPN gateway containers (like Gluetun) typically have a
// restrictive FORWARD policy and no MASQUERADE rule for external traffic.
//
// The configuration runs asynchronously after a delay because gateway
// containers often reset their firewall during startup, which would wipe
// our rules if applied too early.
func (b *BridgeManager) ConfigureVPNGateway(serviceName string) error {
	go b.configureVPNGatewayAfterDelay(serviceName)
	return nil
}

func (b *BridgeManager) configureVPNGatewayAfterDelay(serviceName string) {
	// Gluetun can take 20-40s to fully initialise its firewall.
	// Retry at increasing intervals to win the race.
	for _, delay := range []time.Duration{10, 20, 30} {
		time.Sleep(delay * time.Second)
		if b.tryConfigureGateway(serviceName) {
			return
		}
	}
	log.Printf("Warning: failed to configure VPN gateway %s after retries", serviceName)
}

// tryConfigureGateway attempts the configuration and returns true if the
// rules were applied (verified by checking FORWARD chain).
func (b *BridgeManager) tryConfigureGateway(serviceName string) bool {
	nsName := "qm-" + ShortName(serviceName)

	nsHandle, err := netns.GetFromName(nsName)
	if err != nil {
		return false
	}
	defer nsHandle.Close()

	origNs, _ := netns.Get()
	defer origNs.Close()

	if err := netns.Set(nsHandle); err != nil {
		return false
	}
	defer netns.Set(origNs)

	ipt, err := iptables.New()
	if err != nil {
		return false
	}

	if err := ipt.Append("filter", "FORWARD", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		log.Printf("Warning: gateway FORWARD conntrack: %v", err)
	}
	if err := ipt.Append("filter", "FORWARD", "-s", bridgeSubnet, "-j", "ACCEPT"); err != nil {
		log.Printf("Warning: gateway FORWARD accept for %s: %v", bridgeSubnet, err)
	}

	exists, _ := ipt.Exists("nat", "POSTROUTING", "-s", bridgeSubnet, "-j", "MASQUERADE")
	if !exists {
		if err := ipt.Append("nat", "POSTROUTING", "-s", bridgeSubnet, "-j", "MASQUERADE"); err != nil {
			log.Printf("Warning: gateway MASQUERADE for %s: %v", bridgeSubnet, err)
		}
	}

	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)

	// Verify the rules actually took effect (gluetun may reset firewall).
	rules, _ := ipt.List("filter", "FORWARD")
	hasRule := false
	for _, r := range rules {
		if strings.Contains(r, bridgeSubnet) && strings.Contains(r, "ACCEPT") {
			hasRule = true
			break
		}
	}
	if !hasRule {
		return false
	}

	log.Printf("Configured VPN gateway %s to forward bridge subnet %s", serviceName, bridgeSubnet)
	return true
}

// ── Port forwarding ─────────────────────────────────────────────────────

// ConfigureTailscale implements NetManager.  Enters the tailscale
// container's namespace and adds iptables DNAT rules for normal exposure,
// then runs tailscale serve/funnel commands as needed.
func (b *BridgeManager) ConfigureTailscale(serviceName string, exposures []TailscaleExposure, execFn func(cmd ...string) error) error {
	nsName := "qm-" + ShortName(serviceName)

	nsHandle, err := netns.GetFromName(nsName)
	if err != nil {
		return fmt.Errorf("open tailscale netns %s: %w", nsName, err)
	}
	defer nsHandle.Close()

	origNs, _ := netns.Get()
	defer origNs.Close()

	if err := netns.Set(nsHandle); err != nil {
		return fmt.Errorf("enter tailscale netns %s: %w", nsName, err)
	}
	defer netns.Set(origNs)

	ipt, err := iptables.New()
	if err != nil {
		return fmt.Errorf("iptables in tailscale ns: %w", err)
	}

	for _, exp := range exposures {
		if exp.ServiceIP == nil {
			continue
		}
		targetIP := exp.ServiceIP.String()

		switch exp.Expose.Type {
		case "tailscale":
			// Add DNAT rules for each declared port.
			for _, port := range exp.Expose.Ports {
				b.addTailscaleDNAT(ipt, port, targetIP, exp.ServiceName)
			}

		case "serve":
			// Add DNAT for the serve port.
			b.addTailscaleDNAT(ipt, exp.Expose.Port, targetIP, exp.ServiceName)
			// Run tailscale serve with path prefix if name is set.
			if execFn != nil {
				args := []string{"tailscale", "serve", "--bg"}
				if exp.Expose.Name != "" {
					args = append(args, "--set-path", "/"+exp.Expose.Name)
				}
				args = append(args, fmt.Sprintf("%s:%d", targetIP, exp.Expose.Port))
				if err := execFn(args...); err != nil {
					log.Printf("Warning: tailscale serve for %s: %v", exp.ServiceName, err)
				}
			}

		case "funnel":
			// Add DNAT for the funnel port.
			b.addTailscaleDNAT(ipt, exp.Expose.Port, targetIP, exp.ServiceName)
			// Run tailscale funnel (which also enables serve).
			if execFn != nil {
				if err := execFn("tailscale", "funnel", "--bg",
					fmt.Sprintf("%d", exp.Expose.Port)); err != nil {
					log.Printf("Warning: tailscale funnel for %s: %v", exp.ServiceName, err)
				}
			}
		}
	}

	log.Printf("Configured tailscale container %s with %d exposure(s)", serviceName, len(exposures))
	return nil
}

func (b *BridgeManager) addTailscaleDNAT(ipt *iptables.IPTables, port int, targetIP, name string) {
	proto := "tcp"
	args := []string{"-p", proto, "--dport", fmt.Sprintf("%d", port),
		"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", targetIP, port)}
	exists, _ := ipt.Exists("nat", "PREROUTING", args...)
	if exists {
		return
	}
	if err := ipt.Append("nat", "PREROUTING", args...); err != nil {
		log.Printf("Warning: tailscale DNAT for %s:%d: %v", name, port, err)
	} else {
		log.Printf("Tailscale exposed %s port %d → %s:%d", name, port, targetIP, port)
	}
}

// ExposePorts adds iptables DNAT rules so host ports forward to the
// container's bridge IP.  The rules are tracked by container name for
// cleanup on Detach.
func (b *BridgeManager) ExposePorts(containerName string, containerIP net.IP, ports []types.Port) {
	if containerIP == nil || b.ipt4 == nil {
		return
	}
	ipStr := containerIP.String()
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		dport := fmt.Sprintf("%d", p.Host)
		to := fmt.Sprintf("%s:%d", ipStr, p.Container)

		// Clean any stale DNAT rules for this port that point to a
		// different IP.  Prevents accumulation of dead rules when a
		// container's bridge IP changes across recreates.
		for _, chain := range []string{"PREROUTING", "OUTPUT"} {
			rules, _ := b.ipt4.List("nat", chain)
			for _, rule := range rules {
				if strings.Contains(rule, fmt.Sprintf("dpt:%s", dport)) &&
					strings.Contains(rule, "--to-destination") &&
					!strings.Contains(rule, ipStr) {
					args := ruleToDeleteArgs(rule, ipStr)
					if args != nil {
						b.ipt4.Delete("nat", chain, args...)
						log.Printf("Cleaned stale DNAT %s rule for port %s (was pointing to old IP)", chain, dport)
					}
				}
			}
		}

		args := []string{"-p", proto, "--dport", dport, "-j", "DNAT", "--to-destination", to}
		exists, _ := b.ipt4.Exists("nat", "PREROUTING", args...)
		if exists {
			continue
		}
		if err := b.ipt4.Append("nat", "PREROUTING", args...); err != nil {
			log.Printf("Warning: DNAT %s:%d → %s: %v", containerName, p.Host, to, err)
		} else {
			log.Printf("Exposed %s port %d/%s → %s (PREROUTING)", containerName, p.Host, proto, to)
		}

		// Also add OUTPUT chain DNAT so host-local traffic works
		// (e.g. curl localhost:8080) — rule #39.
		argsOut := []string{"-p", proto, "--dport", dport, "-j", "DNAT", "--to-destination", to}
		existsOut, _ := b.ipt4.Exists("nat", "OUTPUT", argsOut...)
		if !existsOut {
			if err := b.ipt4.Append("nat", "OUTPUT", argsOut...); err != nil {
				log.Printf("Warning: DNAT OUTPUT %s:%d → %s: %v", containerName, p.Host, to, err)
			} else {
				log.Printf("Exposed %s port %d/%s → %s (OUTPUT)", containerName, p.Host, proto, to)
			}
		}
	}
}

// removePorts deletes all DNAT rules for a container by matching its IP
// in both PREROUTING and OUTPUT chains.
func (b *BridgeManager) removePorts(containerName string, ip net.IP) {
	if ip == nil || b.ipt4 == nil {
		return
	}
	ipStr := ip.String()

	for _, chain := range []string{"PREROUTING", "OUTPUT"} {
		rules, err := b.ipt4.List("nat", chain)
		if err != nil {
			continue
		}
		for _, rule := range rules {
			if strings.Contains(rule, "--to-destination") && strings.Contains(rule, ipStr) {
				args := ruleToDeleteArgs(rule, ipStr)
				if args != nil {
					if err := b.ipt4.Delete("nat", chain, args...); err != nil {
						log.Printf("Warning: failed to delete DNAT %s rule for %s: %v", chain, containerName, err)
					} else {
						log.Printf("Removed DNAT %s rule for %s (%s)", chain, containerName, ipStr)
					}
				}
			}
		}
	}
}

// ruleToDeleteArgs converts an iptables rule line to deletion args.
// Input:  "-A PREROUTING -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.42.0.5:80"
// Output: ["-p", "tcp", "--dport", "8080", "-j", "DNAT", "--to-destination", "10.42.0.5:80"]
func ruleToDeleteArgs(rule, ipStr string) []string {
	rule = strings.TrimSpace(rule)
	parts := strings.Fields(rule)
	var args []string
	skipNext := false
	for _, p := range parts {
		if skipNext {
			skipNext = false
			continue
		}
		switch p {
		case "-A", "PREROUTING", "-m", "tcp", "udp":
			// Skip chain name and redundant proto module spec.
			if p == "-m" {
				skipNext = true
			}
			continue
		case "-p", "--dport", "-j", "--to-destination":
			args = append(args, p)
		default:
			// Argument to previous flag.
			args = append(args, p)
		}
	}
	if len(args) < 6 {
		return nil
	}
	return args
}

// ── VPN policy routing ──────────────────────────────────────────────────

// setupVPNRouting adds a connection-mark mangle rule inside the container's
// namespace so non-bridge, non-LAN egress is marked with fwmark 100.  The
// host's shared fwmark-based routing table 100 directs marked packets through
// the VPN gateway.  This replaces per-container ip rules with one shared
// routing table for the whole VPN zone.
func (b *BridgeManager) setupVPNRouting(nsName, containerIP, gatewayIP, ctrVeth string) error {
	// ── 1. Add mangle mark rule inside the container's netns ─────
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get host netns: %w", err)
	}
	defer origNs.Close()
	defer netns.Set(origNs)

	targetNs, err := netns.GetFromName(nsName)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", nsName, err)
	}
	defer targetNs.Close()

	if err := netns.Set(targetNs); err != nil {
		return fmt.Errorf("enter netns %s: %w", nsName, err)
	}

	// iptables runs inside the container's namespace.
	// Rule 1: RETURN established connections so responses to host/laptop
	// are not routed through the VPN tunnel.
	ctArgs := []string{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"}
	if exists, _ := b.ipt4.Exists("mangle", "OUTPUT", ctArgs...); !exists {
		b.ipt4.Insert("mangle", "OUTPUT", 1, ctArgs...)
	}

	// Rule 2: mark all non-bridge traffic with fwmark 100.
	// Bridge-local (10.42.0.0/24) stays unmarked, routed through main table.
	mangleArgs := []string{"!", "-d", bridgeSubnet, "-j", "MARK", "--set-mark", "100"}
	if exists, _ := b.ipt4.Exists("mangle", "OUTPUT", mangleArgs...); !exists {
		if err := b.ipt4.Insert("mangle", "OUTPUT", 1, mangleArgs...); err != nil {
			return fmt.Errorf("add mangle mark rule in %s: %w", nsName, err)
		}
		log.Printf("VPN mangle mark: %s → fwmark 100 (bridge/LAN direct)", containerIP)
	}

	// ── 2. Ensure host-level fwmark routing infrastructure ───────
	if err := b.ensureFwmarkRouting(gatewayIP); err != nil {
		return fmt.Errorf("host fwmark routing: %w", err)
	}

	log.Printf("VPN routing: %s → fwmark 100 → table %d via %s", containerIP, vpnRouteTable, gatewayIP)
	return nil
}

// teardownVPNRouting removes the iptables mangle mark rule from the
// container's namespace.
func (b *BridgeManager) teardownVPNRouting(nsName, containerIP string) {
	nsHandle, err := netns.GetFromName(nsName)
	if err != nil {
		return
	}
	defer nsHandle.Close()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origNs, _ := netns.Get()
	defer origNs.Close()
	defer netns.Set(origNs)

	netns.Set(nsHandle)
	ctArgs := []string{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"}
	b.ipt4.Delete("mangle", "OUTPUT", ctArgs...)
	mangleArgs := []string{"!", "-d", bridgeSubnet, "-j", "MARK", "--set-mark", "100"}
	b.ipt4.Delete("mangle", "OUTPUT", mangleArgs...)
	log.Printf("Removed VPN mangle rule for %s", containerIP)
}

// ── Fwmark routing infrastructure (host-level) ─────────────────────────

// ensureFwmarkRouting sets up the shared fwmark-based routing on the host.
// A single ip rule (fwmark 100 → table 100) and a single default route
// via the VPN gateway replace the old per-container policy rules.
// Idempotent; safe to call multiple times.
func (b *BridgeManager) ensureFwmarkRouting(gatewayIP string) error {
	rule := netlink.NewRule()
	rule.Mark = 100
	rule.Table = vpnRouteTable
	if err := netlink.RuleAdd(rule); err != nil && !os.IsExist(err) {
		return fmt.Errorf("add fwmark 100 rule: %w", err)
	}

	gw := net.ParseIP(gatewayIP)
	if gw == nil {
		return fmt.Errorf("invalid gateway IP %q", gatewayIP)
	}
	route := &netlink.Route{
		Dst:   &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Gw:    gw,
		Table: vpnRouteTable,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("add fwmark default route via %s: %w", gatewayIP, err)
	}
	log.Printf("Fwmark routing: table %d default via %s", vpnRouteTable, gatewayIP)
	return nil
}

// UpdateGatewayRoute implements NetManager.  Replaces the default route in
// table 100 when the VPN gateway's bridge IP changes.  No container
// recreates needed.
func (b *BridgeManager) UpdateGatewayRoute(gatewayIP string) error {
	gw := net.ParseIP(gatewayIP)
	if gw == nil {
		return fmt.Errorf("invalid gateway IP %q", gatewayIP)
	}
	route := &netlink.Route{
		Dst:   &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Gw:    gw,
		Table: vpnRouteTable,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("replace fwmark route via %s: %w", gatewayIP, err)
	}
	log.Printf("Updated fwmark route: table %d default via %s", vpnRouteTable, gatewayIP)
	return nil
}

// ── Namespace-scoped netlink helpers ────────────────────────────────────

// getHandle returns a netlink.Handle bound to a named network namespace.
// Using NewHandleAt is thread-safe and prevents namespace leakage to the
// host (unlike netns.Set which can leak due to goroutine scheduling).
func getHandle(nsName string) (*netlink.Handle, error) {
	nsHandle, err := netns.GetFromName(nsName)
	if err != nil {
		return nil, err
	}
	// NewHandleAt takes ownership of the fd; don't close nsHandle.
	handle, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		nsHandle.Close()
		return nil, err
	}
	return handle, nil
}

func (b *BridgeManager) configureNamespace(nsName, ctrVeth, ip string) error {
	handle, err := getHandle(nsName)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", nsName, err)
	}
	defer handle.Delete()

	ctrLink, err := handle.LinkByName(ctrVeth)
	if err != nil {
		return fmt.Errorf("find %s in ns: %w", ctrVeth, err)
	}

	addr, _ := netlink.ParseAddr(ip + "/24")
	if err := handle.AddrAdd(ctrLink, addr); err != nil {
		return fmt.Errorf("assign IP %s to %s: %w", ip, ctrVeth, err)
	}
	if err := handle.LinkSetUp(ctrLink); err != nil {
		return fmt.Errorf("bring %s up: %w", ctrVeth, err)
	}

	// ── Disable rp_filter on the container veth so return-path
	//    traffic from VPN gateway works correctly (rule #5).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origNs, _ := netns.Get()
	defer origNs.Close()
	defer netns.Set(origNs)
	if targetNs, err := netns.GetFromName(nsName); err == nil {
		defer targetNs.Close()
		netns.Set(targetNs)
		rpPath := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", ctrVeth)
		os.WriteFile(rpPath, []byte("0"), 0644)
	}

	lo, _ := handle.LinkByName("lo")
	if lo != nil {
		handle.LinkSetUp(lo)
	}

	gw := net.ParseIP(bridgeGW)
	route := &netlink.Route{
		Dst: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Gw:  gw,
	}
	if err := handle.RouteAdd(route); err != nil && !os.IsExist(err) {
		return fmt.Errorf("add default route via %s: %w", bridgeGW, err)
	}

	return nil
}

func (b *BridgeManager) cleanupNamespace(nsName, hostVeth string) {
	netlink.LinkDel(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostVeth}})
	netns.DeleteNamed(nsName)
}

// ── Static helpers ──────────────────────────────────────────────────────

func bridgeExists() bool {
	_, err := netlink.LinkByName(bridgeName)
	return err == nil
}

func addrExistsOnLink(linkName, ip string) bool {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return false
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if strings.HasPrefix(a.IPNet.String(), ip+"/") {
			return true
		}
	}
	return false
}

// ShortName truncates a name to 8 characters for interface naming.
func ShortName(name string) string {
	if name == "" {
		return "ctr"
	}
	n := len(name)
	if n > 8 {
		n = 8
	}
	return name[:n]
}
