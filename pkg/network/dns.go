// Package network — dns.go
//
// DNSForwarder provides an in-process DNS server on the bridge gateway
// IP (10.42.0.1:53).  It resolves container hostnames from the bridge
// IP map, forwards VPN-zone queries through the VPN gateway, and falls
// back to host DNS for everything else.
package network

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// resolvConfPath is the single resolv.conf shared by all bridge containers.
const resolvConfPath = "/var/lib/quartermaster/resolv.conf"

// DNSForwarder runs an in-process DNS server on the bridge gateway IP.
// It answers queries for container hostnames using the bridge IP map,
// forwards VPN-zone queries through the VPN gateway (gluetun), and
// forwards everything else to the host's configured upstream DNS.
type DNSForwarder struct {
	mu        sync.RWMutex
	server    *dns.Server
	ipMap     map[string]net.IP // service name → bridge IP (owned by BridgeManager)
	gluetunIP net.IP            // current VPN gateway IP, updated live
	hostDNS   []string          // upstream DNS servers from /etc/resolv.conf
}

// NewDNSForwarder creates a DNS forwarder that reads host DNS servers
// from /etc/resolv.conf and uses the provided IP map for container
// hostname resolution.
func NewDNSForwarder(ipMap map[string]net.IP) *DNSForwarder {
	return &DNSForwarder{
		ipMap:   ipMap,
		hostDNS: parseHostDNS(),
	}
}

// ── Lifecycle ───────────────────────────────────────────────────────────

// Start begins listening on the bridge gateway IP (UDP port 53).
func (f *DNSForwarder) Start() error {
	// Bind the UDP socket synchronously so permission errors are caught
	// immediately rather than surfacing asynchronously in a goroutine.
	pc, err := net.ListenPacket("udp", bridgeGW+":53")
	if err != nil {
		return fmt.Errorf("bind DNS on %s:53: %w", bridgeGW, err)
	}

	f.server = &dns.Server{
		PacketConn: pc,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			f.handleDNS(w, r)
		}),
	}

	log.Printf("DNS forwarder starting on %s:53", bridgeGW)
	go func() {
		if err := f.server.ActivateAndServe(); err != nil {
			log.Printf("DNS forwarder stopped: %v", err)
		}
	}()

	// Write the single resolv.conf for all bridge containers.
	os.MkdirAll("/var/lib/quartermaster", 0755)
	if err := os.WriteFile(resolvConfPath, []byte("nameserver "+bridgeGW+"\n"), 0644); err != nil {
		return fmt.Errorf("write %s: %w", resolvConfPath, err)
	}
	log.Printf("Wrote %s (nameserver %s)", resolvConfPath, bridgeGW)

	return nil
}

// Stop gracefully shuts down the DNS server.
func (f *DNSForwarder) Stop() error {
	if f.server != nil {
		return f.server.Shutdown()
	}
	return nil
}

// ── Live update ─────────────────────────────────────────────────────────

// UpdateGluetunIP sets the VPN gateway IP used for forwarding VPN-zone
// DNS queries.  Safe to call concurrently with DNS lookups.
func (f *DNSForwarder) UpdateGluetunIP(ip net.IP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gluetunIP = ip
	log.Printf("DNS forwarder: gluetun IP updated to %s", ip)
}

// ── DNS handler ─────────────────────────────────────────────────────────

func (f *DNSForwarder) handleDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = false

	if len(r.Question) == 0 {
		w.WriteMsg(m)
		return
	}

	q := r.Question[0]
	name := strings.TrimSuffix(strings.ToLower(q.Name), ".")

	// ── Rule 1: container hostname → bridge IP ──────────────────
	f.mu.RLock()
	if ip, ok := f.ipMap[name]; ok {
		f.mu.RUnlock()
		rr := &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   ip,
		}
		m.Answer = append(m.Answer, rr)
		w.WriteMsg(m)
		return
	}

	// ── Rule 1b: short hostname → bridge gateway (host) ─────────
	//    Services on host networking (sonarr, radarr, jellyfin, etc.)
	//    don't have bridge IPs but are reachable via the bridge GW.
	if !strings.Contains(name, ".") {
		f.mu.RUnlock()
		rr := &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP(bridgeGW),
		}
		m.Answer = append(m.Answer, rr)
		w.WriteMsg(m)
		return
	}

	gwIP := f.gluetunIP
	f.mu.RUnlock()

	// ── Rule 2: VPN-zone query → forward to gluetun ─────────────
	//    Only forward when gluetun IP is known.
	if gwIP != nil {
		resp, err := forwardDNS(r, gwIP.String()+":53")
		if err == nil && resp != nil {
			resp.SetReply(r)
			w.WriteMsg(resp)
			return
		}
		log.Printf("DNS forward: gluetun %s failed for %s: %v — falling back to host DNS", gwIP, name, err)
	}

	// ── Rule 3: everything else → host DNS ──────────────────────
	resp, err := forwardToHost(r, f.hostDNS)
	if err != nil {
		log.Printf("DNS: host forward failed for %s: %v", name, err)
		m.SetRcode(r, dns.RcodeServerFailure)
		w.WriteMsg(m)
		return
	}
	if resp != nil {
		resp.SetReply(r)
		w.WriteMsg(resp)
		return
	}
	w.WriteMsg(m)
}

// ── Forwarding helpers ──────────────────────────────────────────────────

// forwardDNS sends a query to a specific upstream server.
func forwardDNS(r *dns.Msg, addr string) (*dns.Msg, error) {
	c := new(dns.Client)
	c.Timeout = 5 * time.Second
	resp, _, err := c.Exchange(r, addr)
	return resp, err
}

// forwardToHost sends a query to the first reachable host DNS server.
func forwardToHost(r *dns.Msg, servers []string) (*dns.Msg, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("no host DNS servers configured")
	}
	for _, s := range servers {
		addr := net.JoinHostPort(s, "53")
		resp, err := forwardDNS(r, addr)
		if err == nil && resp != nil {
			return resp, nil
		}
	}
	return nil, fmt.Errorf("all host DNS servers unreachable")
}

// ── Host DNS parsing ────────────────────────────────────────────────────

// parseHostDNS reads nameserver lines from /etc/resolv.conf.
func parseHostDNS() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		// Fallback if the host has no resolv.conf (unlikely but safe).
		return []string{"1.1.1.1", "8.8.8.8"}
	}
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "nameserver") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				servers = append(servers, fields[1])
			}
		}
	}
	if len(servers) == 0 {
		return []string{"1.1.1.1", "8.8.8.8"}
	}
	return servers
}
