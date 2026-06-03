package daemon

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"quartermaster/pkg/config"
	"quartermaster/pkg/cri"
	"quartermaster/pkg/types"
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
	Name      string     `json:"name"`
	Image     string     `json:"image"`
	Running   bool       `json:"running"`
	PID       uint32     `json:"pid,omitempty"`
	Healthy   *bool      `json:"healthy,omitempty"` // nil = no healthcheck configured
	StartedAt *time.Time `json:"started_at,omitempty"`
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

// startAPI listens on a Unix socket and serves the status, component, WebSocket,
// and service detail APIs.
// reloadCh is written to when a client requests a config reload (enable/disable).
// settingsPath is the path to settings.json, used by /v1/components.
// hub is the EventHub for WebSocket subscriptions.
// stackFn returns the latest merged stack for service detail lookups.
// logFn maps a service name to its log output.  tail is the number of
// bytes to fetch (e.g. "4096") or "all" for everything.
// restartFn stops and deletes a container by service name so the
// reconciler will redeploy it.
type logFn func(ctx context.Context, serviceName string, tail string) (string, error)
type restartFn func(ctx context.Context, serviceName string) error

func startAPI(socketPath string, status *Status, mu *sync.RWMutex, reloadCh chan struct{}, reconcileCh chan struct{}, settingsPath string, hub *EventHub, stackFn func() *types.Stack, logsFn logFn, restartFn restartFn) error {
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

	// /v1/reconcile — triggers an immediate reconciliation pass
	mux.HandleFunc("/v1/reconcile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		select {
		case reconcileCh <- struct{}{}:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true,"message":"reconciliation triggered"}`))
		default:
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"ok":false,"error":"reconciliation already pending"}`))
		}
	})

	// /v1/configmaps/<name> — create or update a ConfigMap (POST)
	mux.HandleFunc("/v1/configmaps/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v1/configmaps/")
		if name == "" || strings.Contains(name, "/") {
			http.Error(w, "bad name", 400)
			return
		}

		switch r.Method {
		case http.MethodPost, http.MethodPut:
			var body struct {
				Data map[string]string `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}

			cm := &types.ConfigMap{
				Version: "1",
				Kind:    "ConfigMap",
				Metadata: types.Metadata{Name: name},
				Data:    body.Data,
			}

			path := filepath.Join("/etc/quartermaster/configmaps", name+".yaml")
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}

			data, _ := yaml.Marshal(cm)
			if err := os.WriteFile(path, data, 0640); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}

			// Trigger reconcile to pick up the new ConfigMap.
			select {
			case reconcileCh <- struct{}{}:
			default:
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"message": "configmap " + name + " saved",
			})

		case http.MethodGet:
			// List keys or return full configmap.
			stack := stackFn()
			_ = stack // future: return configmap details
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"name": name,
			})

		default:
			http.Error(w, "method not allowed", 405)
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

	// WebSocket upgrader
	wsUpgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// /v1/events — WebSocket stream of daemon events
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade failed: %v", err)
			return
		}

		sub := hub.Subscribe(conn)
		defer hub.Unsubscribe(sub)

		// Send initial status snapshot.
		mu.RLock()
		s := *status
		s.Uptime = time.Since(s.StartedAt).Truncate(time.Second).String()
		if s.Containers == nil {
			s.Containers = []ContainerStatus{}
		}
		if s.Watchers == nil {
			s.Watchers = []WatcherStatus{}
		}
		mu.RUnlock()

		hub.PublishEvent("status", StatusData{
			Version:            s.Version,
			StartedAt:          s.StartedAt,
			Uptime:             s.Uptime,
			LastReconcile:      s.LastReconcile,
			LastReconcileError: s.LastReconcileError,
			ReconcileCount:     s.ReconcileCount,
			Containers:         s.Containers,
			Watchers:           s.Watchers,
			LKGHealthy:         s.LKGHealthy,
			LKGError:           s.LKGError,
		})

		// Read loop — keeps the connection alive until client disconnects.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	})

	// /v1/services/<name> — detailed config for a single service (GET)
	// /v1/services/<name>/logs — trailing logs for a running service (GET)
	// /v1/services/<name>/restart — stop + delete container for redeploy (POST)
	mux.HandleFunc("/v1/services/", func(w http.ResponseWriter, r *http.Request) {
		// Parse path: /v1/services/<name>[/logs|/restart]
		path := strings.TrimPrefix(r.URL.Path, "/v1/services/")
		wantLogs := strings.HasSuffix(path, "/logs")
		wantRestart := strings.HasSuffix(path, "/restart")
		name := strings.TrimSuffix(strings.TrimSuffix(path, "/logs"), "/restart")
		if name == "" || strings.Contains(name, "/") {
			http.Error(w, "bad service name", 400)
			return
		}

		// ── Restart endpoint (POST) ────────────────────────────────
		if wantRestart {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			if restartFn == nil {
				w.WriteHeader(http.StatusNotImplemented)
				json.NewEncoder(w).Encode(map[string]string{"error": "restart not configured"})
				return
			}
			if err := restartFn(r.Context(), name); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"ok": "true", "message": "restarting " + name})
			return
		}

		// ── Only GET beyond this point ─────────────────────────────
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}

		// ── Logs endpoint ──────────────────────────────────────────
		if wantLogs {
			if logsFn == nil {
				w.WriteHeader(http.StatusNotImplemented)
				json.NewEncoder(w).Encode(map[string]string{"error": "log streaming not configured"})
				return
			}
			tail := r.URL.Query().Get("tail")
			if tail == "" {
				tail = "4096" // default: last 4KB
			}
			logText, err := logsFn(r.Context(), name, tail)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(logText))
			return
		}

		// ── Service config endpoint ────────────────────────────────
		stack := stackFn()
		if stack == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "no stack loaded yet"})
			return
		}

		for _, svc := range stack.Spec.Services {
			if svc.Name == name {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(svc)
				return
			}
		}

		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "service not found"})
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
// Tracks the first time a container becomes running or gets a new PID.
func updateContainerHealth(status *Status, mu *sync.RWMutex, name string, running bool, pid uint32) {
	mu.Lock()
	defer mu.Unlock()

	now := time.Now()
	for i := range status.Containers {
		if status.Containers[i].Name == name {
			wasRunning := status.Containers[i].Running
			oldPID := status.Containers[i].PID
			status.Containers[i].Running = running
			status.Containers[i].PID = pid

			// Record start time when container first starts or restarts.
			if running && (!wasRunning || (pid != oldPID)) {
				status.Containers[i].StartedAt = &now
			}
			if !running {
				status.Containers[i].StartedAt = nil
			}
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
