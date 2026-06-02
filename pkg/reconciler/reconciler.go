package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/network"
	"quartermaster/pkg/types"
)

// Reconciler is the core engine that synchronizes the actual state with the desired state.
type Reconciler struct {
	containerClient cri.ContainerClient
	configManager   *config.ConfigManager
	netManager      *network.Manager // optional: for network profile / VPN sidecar support
}

// NewReconciler creates a new instance of the Reconciler.
func NewReconciler(cc cri.ContainerClient, cm *config.ConfigManager) *Reconciler {
	return &Reconciler{
		containerClient: cc,
		configManager:   cm,
	}
}

// SetNetworkManager attaches a network manager for VPN sidecar support.
func (r *Reconciler) SetNetworkManager(nm *network.Manager) {
	r.netManager = nm
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
		// Compute config hash for change detection
		stack.Spec.Services[i].ConfigHash = serviceConfigHash(&s)
		desiredMap[s.Name] = stack.Spec.Services[i]
	}

	// 3. Reconcile: Add or Update (in dependency order).
	// Track running VPN services so dependents can join their namespaces.
	runningVPN := make(map[string]uint32)

	ordered := topologicalSort(stack.Spec.Services)
	for _, svc := range ordered {
		if actual, exists := actualMap[svc.Name]; !exists {
			// Service is missing -> Create it
			log.Printf("Service %s is missing. Creating...", svc.Name)
			containerID, err := r.runCreateFlow(ctx, svc)
			if err != nil {
				log.Printf("Error creating service %s: %v", svc.Name, err)
				continue
			}

			// Register VPN gateway if this service acts as one.
			if r.netManager != nil && svc.Network != "" {
				isGW, _, _ := r.netManager.ResolveProfile(svc.Network, svc.DependsOn, runningVPN)
				if isGW {
					pid, pidErr := r.containerClient.GetContainerPID(ctx, containerID)
					if pidErr != nil {
						log.Printf("Warning: failed to get PID for VPN gateway %s: %v", svc.Name, pidErr)
					} else if pid > 0 {
						r.netManager.RegisterVPNGateway(pid)
						runningVPN[svc.Name] = pid
						log.Printf("Registered VPN gateway %s (pid=%d)", svc.Name, pid)
					}
				}
			}
		} else {
			// Service exists — check if config changed
			if actual.ConfigHash != "" && svc.ConfigHash != "" && actual.ConfigHash != svc.ConfigHash {
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
			// Container is running but not in the manifest -> Delete it
			log.Printf("Service %s is no longer in manifest. Removing...", name)
			if err := r.runDeleteFlow(ctx, actual.ID); err != nil {
				log.Printf("Error removing service %s: %v", name, err)
				continue
			}
		}
	}

	log.Println("Reconciliation pass complete.")
	return nil
}

func (r *Reconciler) runCreateFlow(ctx context.Context, svc types.Service) (string, error) {
	// 1. Pull Image
	_, err := r.containerClient.PullImage(ctx, svc.Image)
	if err != nil {
		return "", fmt.Errorf("pull failed: %w", err)
	}

	// 2. Create Container
	containerID, err := r.containerClient.CreateContainer(ctx, svc)
	if err != nil {
		return "", fmt.Errorf("create failed: %w", err)
	}

	// 3. Start Container
	if err := r.containerClient.StartContainer(ctx, containerID); err != nil {
		return "", fmt.Errorf("start failed: %w", err)
	}

	return containerID, nil
}

func (r *Reconciler) runDeleteFlow(ctx context.Context, containerID string) error {
	return r.containerClient.DeleteContainer(ctx, containerID)
}

// runUpdateFlow stops and deletes the old container, then creates a new one.
func (r *Reconciler) runUpdateFlow(ctx context.Context, oldContainerID string, svc types.Service) error {
	log.Printf("Updating service %s: removing old container %s", svc.Name, oldContainerID)

	// Stop and delete the old container
	if err := r.containerClient.DeleteContainer(ctx, oldContainerID); err != nil {
		return fmt.Errorf("failed to delete old container: %w", err)
	}

	// Create and start the new container
	_, err := r.runCreateFlow(ctx, svc)
	return err
}

// serviceConfigHash computes a SHA256 hash of the service's mutable configuration fields.
// This is used to detect when a running container needs to be recreated.
func serviceConfigHash(svc *types.Service) string {
	// Only hash fields that would require a container recreate
	payload := struct {
		Image    string
		Env      []types.EnvVar
		Ports    []types.Port
		Volumes  []types.Volume
		User     string
		Network  string
		GPU      string
	}{
		Image:   svc.Image,
		Env:     svc.Env,
		Ports:   svc.Ports,
		Volumes: svc.Volumes,
		User:    svc.User,
		Network: svc.Network,
	}
	if svc.Resources != nil && svc.Resources.GPU != nil {
		payload.GPU = svc.Resources.GPU.Type + ":" + svc.Resources.GPU.ID
	}

	data, _ := json.Marshal(payload)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// topologicalSort orders services so that dependencies are created first.
// Uses Kahn's algorithm for topological sorting.
func topologicalSort(services []types.Service) []types.Service {
	if len(services) <= 1 {
		return services
	}

	// Build adjacency and in-degree maps
	inDegree := make(map[string]int)
	dependents := make(map[string][]string) // service -> services that depend on it
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

	// Start with nodes that have no dependencies
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

	// If some services couldn't be sorted (e.g., circular dependency), append remaining
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
