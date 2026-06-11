package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/ingress"
	"quartermaster/pkg/network"
	"quartermaster/pkg/types"
)

// Reconciler is the core engine that synchronizes the actual state with the desired state.
type Reconciler struct {
	containerClient cri.ContainerClient
	configManager   *config.ConfigManager
	netMgr          network.NetManager

	// serviceProfiles tracks the network profile of each created service
	// so Detach can be called on deletion.
	serviceProfiles map[string]string
}

// NewReconciler creates a new instance of the Reconciler.
func NewReconciler(cc cri.ContainerClient, cm *config.ConfigManager) *Reconciler {
	return &Reconciler{
		containerClient: cc,
		configManager:   cm,
		serviceProfiles: make(map[string]string),
	}
}

// SetNetManager attaches a network manager for bridge/IPAM/VPN routing.
func (r *Reconciler) SetNetManager(nm network.NetManager) {
	r.netMgr = nm
}

// Reconcile performs a single reconciliation pass using a config file path.
func (r *Reconciler) Reconcile(ctx context.Context, configPath string) error {
	stack, err := r.configManager.LoadStack(configPath)
	if err != nil {
		return fmt.Errorf("failed to load desired state: %w", err)
	}
	return r.ReconcileStack(ctx, stack)
}

// ReconcileStack performs a single reconciliation pass from a pre-loaded Stack.
func (r *Reconciler) ReconcileStack(ctx context.Context, stack *types.Stack) error {
	log.Println("Starting reconciliation pass...")

	// 1. Fetch Actual State
	actualContainers, err := r.containerClient.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch actual state: %w", err)
	}

	// Map actual containers by Name for easy lookup
	actualMap := make(map[string]cri.ContainerInfo)
	for _, c := range actualContainers {
		actualMap[c.Name] = c
	}

	// Map desired services by Name for easy lookup
	desiredMap := make(map[string]types.Service)
	for i, s := range stack.Spec.Services {
		stack.Spec.Services[i].ConfigHash = serviceConfigHash(&s)
		desiredMap[s.Name] = stack.Spec.Services[i]
	}

	// 2. Track running bridge IPs for VPN gateway resolution.
	//    The NetManager owns IP allocation; we read from it.
	runningBridgeIPs := make(map[string]string)
	if r.netMgr != nil {
		for name := range actualMap {
			if ip := r.netMgr.LookupIP(name); ip != nil {
				runningBridgeIPs[name] = ip.String()
			}
		}
	}

	// 3. Reconcile: Add or Update (in dependency order).
	ordered := topologicalSort(stack.Spec.Services)
	for _, svc := range ordered {
		actual, exists := actualMap[svc.Name]

		// Bug 2 fix: container exists but task is dead → treat as missing.
		if exists && !actual.Running {
			log.Printf("Service %s has a dead task (pid=%d). Recreating...", svc.Name, actual.PID)
			if err := r.runDeleteFlow(ctx, actual.ID, svc.Name); err != nil {
				log.Printf("Error cleaning up dead container %s: %v", svc.Name, err)
			}
			exists = false
		}

		if !exists {
			// Service is missing -> Create it.  Detach any stale
			// network resources first (veth, DNAT rules) in case a
			// previous container was deleted without proper teardown.
			if prevProfile, ok := r.serviceProfiles[svc.Name]; ok && r.netMgr != nil {
				if err := r.netMgr.Detach(svc.Name, prevProfile); err != nil {
					log.Printf("Warning: defensive detach for %s: %v", svc.Name, err)
				}
				delete(r.serviceProfiles, svc.Name)
			}
			log.Printf("Service %s is missing. Creating...", svc.Name)
			if _, err := r.runCreateFlow(ctx, svc, runningBridgeIPs, desiredMap); err != nil {
				log.Printf("Error creating service %s: %v", svc.Name, err)
				continue
			}
			profile := network.NormaliseProfile(svc.Network)
			r.serviceProfiles[svc.Name] = string(profile)
			if r.netMgr != nil && profile != network.ProfilePublic {
				if ip := r.netMgr.LookupIP(svc.Name); ip != nil {
					runningBridgeIPs[svc.Name] = ip.String()
				}
			}
		} else {
			needsUpdate := actual.ConfigHash != "" && svc.ConfigHash != "" && actual.ConfigHash != svc.ConfigHash

			// Step 4: if the VPN gateway IP changed, live-update routes and DNS
			// instead of recreating containers.  The fwmark routing (Step 3)
			// and central DNS (Step 2) make this possible.
			if !needsUpdate && r.netMgr != nil {
				if gwName, newIP := getStaleGatewayInfo(r.netMgr, svc, actual); gwName != "" && newIP != nil {
					log.Printf("Service %s has stale VPN gateway IP (was %s, now %s). Live-updating routes and DNS...",
						svc.Name, actual.GatewayIP, newIP.String())

					// Update the DNS forwarder's cached gluetun IP
					r.netMgr.UpdateDNSGateway(gwName, newIP)

					// Replace the shared fwmark table 100 default route on the host
					if err := r.netMgr.UpdateGatewayRoute(newIP.String()); err != nil {
						log.Printf("Warning: failed to update gateway route for %s: %v", gwName, err)
					}

					// Re-apply the gateway's FORWARD + MASQUERADE rules (handled
					// async by ConfigureVPNGateway — already triggered on gateway restart)

					// Do NOT set needsUpdate = true — no container recreate needed
				}
			}

			if needsUpdate {
				log.Printf("Service %s config changed (hash: %s -> %s). Updating...",
					svc.Name, actual.ConfigHash[:12], svc.ConfigHash[:12])
				if err := r.runUpdateFlow(ctx, actual.ID, svc); err != nil {
					log.Printf("Error updating service %s: %v", svc.Name, err)
				}
			} else {
				log.Printf("Service %s is already running (no changes detected).", svc.Name)
			}
		}
	}

	// 4. Reconcile: Remove
	for name, actual := range actualMap {
		if _, exists := desiredMap[name]; !exists {
			log.Printf("Service %s is no longer in manifest. Removing...", name)
			if err := r.runDeleteFlow(ctx, actual.ID, name); err != nil {
				log.Printf("Error removing service %s: %v", name, err)
				continue
			}
		}
	}

	log.Println("Reconciliation pass complete.")

	// Regenerate ingress config after updates so IPs reflect current state.
	r.regenerateIngress(desiredMap)

	return nil
}

func (r *Reconciler) runCreateFlow(ctx context.Context, svc types.Service, runningBridgeIPs map[string]string, desiredMap map[string]types.Service) (string, error) {
	// Determine if this VPN service is a gateway (no running VPN dep).
	isVPNGateway := false
	if r.netMgr != nil && network.NormaliseProfile(svc.Network) == network.ProfileVPN {
		isVPNGateway = true
		for _, dep := range svc.DependsOn {
			if _, ok := runningBridgeIPs[dep]; ok {
				isVPNGateway = false
				break
			}
		}
	}

	// 1. Pull Image
	fullImage, err := r.containerClient.PullImage(ctx, svc.Image)
	if err != nil {
		return "", fmt.Errorf("pull failed: %w", err)
	}
	svc.Image = fullImage

	// 2. Create Container (netMgr.Attach happens inside)
	containerID, err := r.containerClient.CreateContainer(ctx, svc)
	if err != nil {
		return "", fmt.Errorf("create failed: %w", err)
	}

	// 3. Start Container
	if err := r.containerClient.StartContainer(ctx, containerID); err != nil {
		return "", fmt.Errorf("start failed: %w", err)
	}

	// 4. Configure VPN gateway to forward bridge traffic (after startup,
	//    so the gateway's own firewall/tunnel are initialised).
	if isVPNGateway && r.netMgr != nil {
		if err := r.netMgr.ConfigureVPNGateway(svc.Name); err != nil {
			log.Printf("Warning: configure VPN gateway %s: %v", svc.Name, err)
		}
	}

	// 5. If this is a tailscale container, configure exposures for all
	//    services that opt in.
	if r.netMgr != nil && isTailscaleGateway(svc) {
		exposures := r.collectTailscaleExposures(desiredMap)
		if len(exposures) > 0 {
			execFn := r.tailscaleExecFn(ctx)
			if err := r.netMgr.ConfigureTailscale(svc.Name, exposures, execFn); err != nil {
				log.Printf("Warning: configure tailscale %s: %v", svc.Name, err)
			}
		}
	}

	return containerID, nil
}

func (r *Reconciler) runDeleteFlow(ctx context.Context, containerID, name string) error {
	// Detach network namespace (bridge veth, DNAT rules, VPN policy routes).
	if r.netMgr != nil {
		profile := r.serviceProfiles[name]
		if profile == "" {
			profile = "public"
		}
		if err := r.netMgr.Detach(name, profile); err != nil {
			log.Printf("Warning: network detach for %s failed: %v", name, err)
		}
		delete(r.serviceProfiles, name)
	}
	return r.containerClient.DeleteContainer(ctx, containerID)
}

// runUpdateFlow stops and deletes the old container, then creates a new one.
func (r *Reconciler) runUpdateFlow(ctx context.Context, oldContainerID string, svc types.Service) error {
	log.Printf("Updating service %s: removing old container %s", svc.Name, oldContainerID)

	if err := r.runDeleteFlow(ctx, oldContainerID, svc.Name); err != nil {
		return fmt.Errorf("failed to delete old container: %w", err)
	}

	_, err := r.runCreateFlow(ctx, svc, nil, nil)
	return err
}

// serviceConfigHash computes a SHA256 hash of the service's mutable configuration fields.
func serviceConfigHash(svc *types.Service) string {
	payload := struct {
		Image   string
		Env     []types.EnvVar
		Ports   []types.Port
		Volumes []types.Volume
		User    string
		Network string
		GPU     string
		Expose  *types.ExposeConfig
		Ingress *types.IngressConfig
	}{
		Image:   svc.Image,
		Env:     svc.Env,
		Ports:   svc.Ports,
		Volumes: svc.Volumes,
		User:    svc.User,
		Network: svc.Network,
		Expose:  svc.Expose,
		Ingress: svc.Ingress,
	}
	if svc.Resources != nil && svc.Resources.GPU != nil {
		payload.GPU = svc.Resources.GPU.Type + ":" + svc.Resources.GPU.ID
	}

	data, _ := json.Marshal(payload)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// getStaleGatewayInfo checks whether a VPN-dependent service's gateway IP
// has changed since the container was created.  Returns the gateway name
// and the current (new) IP if stale, or empty/nil if up-to-date.
func getStaleGatewayInfo(nm network.NetManager, svc types.Service, actual cri.ContainerInfo) (string, net.IP) {
	if nm == nil || actual.GatewayIP == "" {
		return "", nil
	}
	profile := network.NormaliseProfile(svc.Network)
	if profile != network.ProfileVPN {
		return "", nil
	}
	for _, dep := range svc.DependsOn {
		gwIP := nm.LookupIP(dep)
		if gwIP == nil {
			// Gateway not yet running — can't determine staleness.
			return "", nil
		}
		if gwIP.String() != actual.GatewayIP {
			log.Printf("VPN gateway %s IP changed: stored=%s current=%s",
				dep, actual.GatewayIP, gwIP.String())
			return dep, gwIP
		}
		return "", nil
	}
	return "", nil
}

// isTailscaleGateway returns true if the service is a tailscale container
// (detected by image name containing "tailscale").
func isTailscaleGateway(svc types.Service) bool {
	return strings.Contains(strings.ToLower(svc.Image), "tailscale")
}

// collectTailscaleExposures gathers all services that have expose configured.
func (r *Reconciler) collectTailscaleExposures(desiredMap map[string]types.Service) []network.TailscaleExposure {
	if r.netMgr == nil {
		return nil
	}
	var exposures []network.TailscaleExposure
	for name, svc := range desiredMap {
		if svc.Expose == nil {
			continue
		}
		profile := network.NormaliseProfile(svc.Network)
		if profile == network.ProfilePublic {
			continue // host-networked services don't use tailscale container
		}
		ip := r.netMgr.LookupIP(name)
		if ip == nil {
			continue
		}
		exposures = append(exposures, network.TailscaleExposure{
			ServiceName: name,
			ServiceIP:   ip,
			Expose:      *svc.Expose,
			Ports:       svc.Ports,
		})
	}
	return exposures
}

// tailscaleExecFn returns a function that execs commands inside a container.
func (r *Reconciler) tailscaleExecFn(ctx context.Context) func(cmd ...string) error {
	return func(cmd ...string) error {
		if len(cmd) == 0 {
			return nil
		}
		// Use ctr CLI for exec into tailscale container.
		args := append([]string{"-n", "quartermaster", "task", "exec", "--exec-id",
			fmt.Sprintf("ts-%d", time.Now().UnixNano()), "tailscale"}, cmd...)
		c := exec.Command("ctr", args...)
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// regenerateIngress rebuilds the Caddyfile and hosts file from all services
// that have ingress blocks, then triggers a graceful Caddy reload.
// Always writes a minimal config so the file exists when the container starts.
func (r *Reconciler) regenerateIngress(desiredMap map[string]types.Service) {
	if r.netMgr == nil {
		return
	}

	// Read domain and TLS mode from caddy ConfigMap.
	domain := ""
	tlsMode := ""
	if r.configManager != nil {
		if d, err := r.configManager.ResolveConfigMap("caddy", "domain"); err == nil {
			domain = d
		}
		if t, err := r.configManager.ResolveConfigMap("caddy", "tls"); err == nil {
			tlsMode = t
		}
	}

	var entries []ingress.ServiceEntry
	autheliaIP := ""
	for name, svc := range desiredMap {
		if strings.Contains(strings.ToLower(svc.Image), "authelia") {
			ip := r.netMgr.LookupIP(name)
			if ip == nil {
				ip = net.ParseIP("127.0.0.1")
			}
			if ip != nil {
				autheliaIP = ip.String()
			}
		}
	}
	for name, svc := range desiredMap {
		ip := r.netMgr.LookupIP(name)
		// Host-networked services use localhost (they bind to host ports).
		if ip == nil && (svc.Network == "public" || svc.Network == "") {
			ip = net.ParseIP("127.0.0.1")
		}
		entry := ingress.ServiceEntry{
			Name:       name,
			IP:         ip,
			Ports:      svc.Ports,
			Ingress:    svc.Ingress,
			AutheliaIP: autheliaIP,
		}
		entries = append(entries, entry)
	}
	if err := ingress.GenerateAll(entries, domain, tlsMode); err != nil {
		log.Printf("Warning: ingress generation failed: %v", err)
		return
	}
	if err := ingress.ReloadCaddy(); err != nil {
		log.Printf("Warning: caddy reload: %v", err)
	}
}

// topologicalSort uses Kahn's algorithm for topological sorting.
func topologicalSort(services []types.Service) []types.Service {
	if len(services) <= 1 {
		return services
	}

	inDegree := make(map[string]int)
	dependents := make(map[string][]string)
	nameIndex := make(map[string]int)

	for i, svc := range services {
		nameIndex[svc.Name] = i
		if _, ok := inDegree[svc.Name]; !ok {
			inDegree[svc.Name] = 0
		}
		for _, dep := range svc.DependsOn {
			inDegree[svc.Name]++
			dependents[dep] = append(dependents[dep], svc.Name)
		}
	}

	var queue []string
	for _, svc := range services {
		if inDegree[svc.Name] == 0 {
			queue = append(queue, svc.Name)
		}
	}

	var sorted []types.Service
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		if idx, ok := nameIndex[name]; ok {
			sorted = append(sorted, services[idx])
		}

		for _, dep := range dependents[name] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(sorted) < len(services) {
		for _, svc := range services {
			found := false
			for _, s := range sorted {
				if s.Name == svc.Name {
					found = true
					break
				}
			}
			if !found {
				sorted = append(sorted, svc)
			}
		}
	}

	return sorted
}
