package daemon

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
)

// ── Status types ─────────────────────────────────────────────────────────

// Status holds the daemon's observable state.  Fields are populated by the
// reconciliation and health-check loops and served by the HTTP API.
type Status struct {
	Version string `json:"version"`

	StartedAt    time.Time `json:"started_at"`
	Uptime       string    `json:"uptime"`

	LastReconcile       *time.Time `json:"last_reconcile"`
	LastReconcileError  string     `json:"last_reconcile_error,omitempty"`
	ReconcileCount      int64      `json:"reconcile_count"`

	Containers []ContainerStatus `json:"containers"`

	Watchers []WatcherStatus `json:"watchers"`

	LKGHealthy bool   `json:"lkg_healthy"`
	LKGError   string `json:"lkg_error,omitempty"`
}

// ContainerStatus is a lightweight snapshot of a managed container.
type ContainerStatus struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Running bool   `json:"running"`
	PID     uint32 `json:"pid,omitempty"`
	Healthy *bool  `json:"healthy,omitempty"` // nil = no healthcheck configured
}

// WatcherStatus describes a Git watcher's current state.
type WatcherStatus struct {
	RepoURL  string     `json:"repo_url"`
	Branch   string     `json:"branch"`
	LastHash string     `json:"last_hash"`
	LastPoll *time.Time `json:"last_poll,omitempty"`
}

// ── HTTP server ──────────────────────────────────────────────────────────

const apiVersion = "0.4.0"

// startAPI listens on a Unix socket and serves the status + component API.
// reloadCh is written to when a client requests a config reload (enable/disable).
// settingsPath is the path to settings.json, used by /v1/components.
func startAPI(socketPath string, status *Status, mu *sync.RWMutex, reloadCh chan struct{}, settingsPath string) error {
	if err := os.MkdirAll(socketPath, 0755); err != nil {
		return err
	}
	socketFile := socketPath + "/daemon.sock"
	os.Remove(socketFile) // clean up stale socket

	ln, err := net.Listen("unix", socketFile)
	if err != nil {
		return err
	}

	// Restrict access to the quartermaster group — allows co-located
	// services (e.g. the GUI dashboard) to read operational metrics
	// without granting world access.  The API is read-only (GET only).
	os.Chmod(socketFile, 0660)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}

		mu.RLock()
		defer mu.RUnlock()

		// Compute uptime on read.
		s := *status // shallow copy
		s.Uptime = time.Since(s.StartedAt).Truncate(time.Second).String()
		if s.Containers == nil {
			s.Containers = []ContainerStatus{}
		}
		if s.Watchers == nil {
			s.Watchers = []WatcherStatus{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})

	// /v1/reload — triggers a config reload
	mux.HandleFunc("/v1/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		select {
		case reloadCh <- struct{}{}:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"ok":false,"error":"reload already pending"}`))
		}
	})

	// /v1/components — list components and their enabled state
	mux.HandleFunc("/v1/components", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		settings, err := config.LoadSettings(settingsPath)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"components_repo": settings.ComponentsRepo,
			"components":      settings.Components,
		})
	})

	srv := &http.Server{Handler: mux}
	go func() {
		log.Printf("Status API listening on %s", socketFile)
		if err := srv.Serve(ln); err != http.ErrServerClosed {
			log.Printf("Status API error: %v", err)
		}
	}()

	return nil
}

// ── State helpers ────────────────────────────────────────────────────────

// recordReconcile updates the status after a reconciliation attempt.
func recordReconcile(status *Status, mu *sync.RWMutex, err error) {
	mu.Lock()
	defer mu.Unlock()

	now := time.Now()
	status.LastReconcile = &now
	status.ReconcileCount++
	if err != nil {
		status.LastReconcileError = err.Error()
	} else {
		status.LastReconcileError = ""
	}
}

// recordContainers updates the container list from containerd state.
func recordContainers(status *Status, mu *sync.RWMutex, containers []cri.ContainerInfo) {
	mu.Lock()
	defer mu.Unlock()

	out := make([]ContainerStatus, 0, len(containers))
	for _, c := range containers {
		out = append(out, ContainerStatus{
			Name:  c.Name,
			Image: c.Image,
			// Running/PID filled in by separate PID lookup.
		})
	}
	status.Containers = out
}

// updateContainerHealth sets running + pid for a single container.
func updateContainerHealth(status *Status, mu *sync.RWMutex, name string, running bool, pid uint32) {
	mu.Lock()
	defer mu.Unlock()

	for i := range status.Containers {
		if status.Containers[i].Name == name {
			status.Containers[i].Running = running
			status.Containers[i].PID = pid
			return
		}
	}
}

// setContainerHealthy sets the health-check result for a container.
func setContainerHealthy(status *Status, mu *sync.RWMutex, name string, healthy bool) {
	mu.Lock()
	defer mu.Unlock()

	for i := range status.Containers {
		if status.Containers[i].Name == name {
			h := healthy
			status.Containers[i].Healthy = &h
			return
		}
	}
}

// recordWatchers updates the git watcher status list.
func recordWatchers(status *Status, mu *sync.RWMutex, ws []WatcherStatus) {
	mu.Lock()
	defer mu.Unlock()
	status.Watchers = ws
}
