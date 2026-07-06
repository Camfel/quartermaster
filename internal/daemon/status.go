package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"quartermaster/pkg/cri"
	"quartermaster/pkg/types"
)

// ── Status types ─────────────────────────────────────────────────────────

// Status holds the daemon's observable state.
type Status struct {
	Version string `json:"version"`

	StartedAt time.Time `json:"started_at"`
	Uptime    string    `json:"uptime"`

	LastReconcile      *time.Time `json:"last_reconcile"`
	LastReconcileError string     `json:"last_reconcile_error,omitempty"`
	ReconcileCount     int64      `json:"reconcile_count"`

	Containers []ContainerStatus `json:"containers"`

	// currentStack holds the most recent merged stack for service detail lookups.
	currentStack *types.Stack

	LKGHealthy bool   `json:"lkg_healthy"`
	LKGError   string `json:"lkg_error,omitempty"`
}

// ContainerStatus is a lightweight snapshot of a managed container.
type ContainerStatus struct {
	Name      string     `json:"name"`
	Image     string     `json:"image"`
	Running   bool       `json:"running"`
	PID       uint32     `json:"pid,omitempty"`
	Healthy   *bool      `json:"healthy,omitempty"`
	Ports     []string   `json:"ports,omitempty"`
	Network   string     `json:"network,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// ── HTTP server ──────────────────────────────────────────────────────────

var apiVersion = "dev"

// logFn maps a service name to its log output.
type logFn func(ctx context.Context, serviceName string, tail string) (string, error)

// restartFn stops and deletes a container by service name so the
// reconciler will redeploy it.
type restartFn func(ctx context.Context, serviceName string) error

// serviceSpecFn returns the full Service spec from the current stack for a
// given service name, or nil if not found.
type serviceSpecFn func(name string) *types.Service

// startAPI listens on a Unix socket and serves the status API.
func startAPI(socketPath string, status *Status, reloadCh chan struct{}, reconcileCh chan struct{}, logsFn logFn, restartFn restartFn, serviceSpecFn serviceSpecFn, metricsHandler http.Handler) error {
	if err := os.MkdirAll(socketPath, 0755); err != nil {
		return err
	}
	socketFile := socketPath + "/daemon.sock"
	os.Remove(socketFile)

	ln, err := net.Listen("unix", socketFile)
	if err != nil {
		return err
	}

	os.Chmod(socketFile, 0660)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}

		// Compute uptime on read.
		s := *status
		s.Uptime = time.Since(s.StartedAt).Truncate(time.Second).String()
		if s.Containers == nil {
			s.Containers = []ContainerStatus{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})

	// /v1/dashboard — live HTML status page (auto-refreshing)
	mux.HandleFunc("/v1/dashboard", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})

	// /v1/metrics — Prometheus-compatible metrics (VictoriaMetrics, etc.)
	if metricsHandler != nil {
		mux.Handle("/v1/metrics", metricsHandler)
	}

	// /v1/reload — triggers a config reload
	mux.HandleFunc("/v1/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		select {
		case reloadCh <- struct{}{}:
			w.Header().Set("Content-Type", "application/json")
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

	// /v1/services/<name> — service detail (GET), logs (GET .../logs), restart (POST .../restart)
	mux.HandleFunc("/v1/services/", func(w http.ResponseWriter, r *http.Request) {
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
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "restarting " + name})
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
				tail = "4096"
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

		// ── Service detail: merge manifest spec with runtime status ─
		if serviceSpecFn != nil {
			if svc := serviceSpecFn(name); svc != nil {
				// Build response with full service manifest + runtime info.
				resp := struct {
					Name          string               `json:"name"`
					Image         string               `json:"image"`
					RestartPolicy string               `json:"restart_policy"`
					Ports         []types.Port         `json:"ports,omitempty"`
					Volumes       []types.Volume       `json:"volumes,omitempty"`
					Env           []types.EnvVar       `json:"env,omitempty"`
					Secrets       []types.SecretRef    `json:"secrets,omitempty"`
					Network       string               `json:"network,omitempty"`
					User          string               `json:"user,omitempty"`
					DependsOn     []string             `json:"depends_on,omitempty"`
					HealthCheck   *types.HealthCheck   `json:"healthcheck,omitempty"`
					Resources     *types.Resources     `json:"resources,omitempty"`
					Command       []string             `json:"command,omitempty"`
					Ingress       *types.IngressConfig `json:"ingress,omitempty"`
					RegistryAuth  *types.RegistryAuth  `json:"registry_auth,omitempty"`
					RestartAt     *types.RestartAt     `json:"restart_at,omitempty"`
					RollingUpdate bool                 `json:"rolling_update,omitempty"`
					// Runtime fields (from container snapshot).
					Running bool   `json:"running"`
					PID     uint32 `json:"pid,omitempty"`
					Healthy *bool  `json:"healthy,omitempty"`
				}{
					Name:          svc.Name,
					Image:         svc.Image,
					RestartPolicy: svc.RestartPolicy,
					Ports:         svc.Ports,
					Volumes:       svc.Volumes,
					Env:           svc.Env,
					Secrets:       svc.Secrets,
					Network:       svc.Network,
					User:          svc.User,
					DependsOn:     svc.DependsOn,
					Command:       svc.Command,
					Ingress:       svc.Ingress,
					RegistryAuth:  svc.RegistryAuth,
					RestartAt:     svc.RestartAt,
					RollingUpdate: svc.RollingUpdate,
				}
				if svc.HealthCheck != nil {
					resp.HealthCheck = svc.HealthCheck
				}
				if svc.Resources != nil {
					resp.Resources = svc.Resources
				}
				// Merge runtime status from container snapshot.
				for _, c := range status.Containers {
					if c.Name == name {
						resp.Running = c.Running
						resp.PID = c.PID
						resp.Healthy = c.Healthy
						break
					}
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
		}

		// ── Fallback: return container snapshot if no spec lookup ──
		for _, c := range status.Containers {
			if c.Name == name {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(c)
				return
			}
		}

		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "service not found"})
	})

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
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
func recordReconcile(status *Status, err error) {
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
// Preserves existing health-check results that may have been set by
// runHealthChecks between reconcile passes.
func recordContainers(status *Status, containers []cri.ContainerInfo, stack *types.Stack) {
	status.currentStack = stack
	svcMap := make(map[string]types.Service, len(stack.Spec.Services))
	for _, svc := range stack.Spec.Services {
		svcMap[svc.Name] = svc
	}

	// Snapshot current health states before rebuilding.
	prevHealthy := make(map[string]*bool, len(status.Containers))
	for _, c := range status.Containers {
		prevHealthy[c.Name] = c.Healthy
	}

	out := make([]ContainerStatus, 0, len(containers))
	for _, c := range containers {
		cs := ContainerStatus{
			Name:    c.Name,
			Image:   c.Image,
			Running: c.Running,
			PID:     c.PID,
		}
		if svc, ok := svcMap[c.Name]; ok {
			cs.Network = svc.Network
			cs.Ports = formatPorts(svc)
		}
		// Preserve existing health state if available.
		if h, ok := prevHealthy[c.Name]; ok {
			cs.Healthy = h
		}
		out = append(out, cs)
	}
	status.Containers = out
}

// dashboardHTML is a self-contained single-page dashboard that polls
// /v1/status every 3 seconds and renders a live status table.
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Quartermaster Dashboard</title>
<style>
  *{margin:0;padding:0;box-sizing:border-box}
  body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#0f172a;color:#e2e8f0;min-height:100vh}
  .header{background:#1e293b;padding:16px 24px;display:flex;align-items:center;justify-content:space-between;border-bottom:2px solid #334155}
  .header h1{font-size:20px;font-weight:600}
  .header .info{font-size:13px;color:#94a3b8}
  .header .dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
  .dot.green{background:#22c55e}
  .dot.red{background:#ef4444}
  .container{max-width:1100px;margin:0 auto;padding:24px}
  .card{background:#1e293b;border-radius:8px;margin-bottom:12px;overflow:hidden;border:1px solid #334155}
  .card-header{display:flex;align-items:center;padding:12px 16px;gap:12px;border-bottom:1px solid #334155}
  .card-header .name{font-weight:600;font-size:15px}
  .card-header .image{font-size:13px;color:#94a3b8;font-family:monospace}
  .status-badge{padding:2px 8px;border-radius:4px;font-size:12px;font-weight:600;margin-left:auto}
  .badge-up{background:#064e3b;color:#34d399}
  .badge-down{background:#450a0a;color:#f87171}
  .card-body{padding:12px 16px;display:flex;gap:20px;flex-wrap:wrap;font-size:13px}
  .card-body .field{display:flex;gap:6px}
  .field-label{color:#64748b}
  .field-value{color:#e2e8f0;font-family:monospace}
  .health-ok{color:#22c55e}
  .health-fail{color:#ef4444}
  .health-unknown{color:#64748b}
  .empty{padding:40px;text-align:center;color:#64748b}
  .footer{padding:16px 24px;font-size:12px;color:#475569;text-align:center}
  @keyframes pulse{0%,100%{opacity:1}50%{opacity:.5}}
  .loading{animation:pulse 1.5s infinite}
</style>
</head>
<body>
<div class="header">
  <div>
    <h1>Quartermaster Dashboard</h1>
    <div class="info" id="headerInfo">connecting...</div>
  </div>
  <div class="info" id="refreshInfo"></div>
</div>
<div class="container" id="container">
  <div class="empty loading">Connecting to daemon...</div>
</div>
<div class="footer">Quartermaster — lightweight container orchestrator</div>
<script>
function fmtDuration(secs){
  if(!secs||secs<0)return'-';
  var d=Math.floor(secs/86400),h=Math.floor((secs%86400)/3600),m=Math.floor((secs%3600)/60),s=Math.floor(secs%60);
  var p=[];if(d>0)p.push(d+'d');if(h>0)p.push(h+'h');if(m>0)p.push(m+'m');p.push(s+'s');return p.join(' ');
}
function icon(b){return b?'&#x2713;':'&#x2717;'}
function refresh(){
  var x=new XMLHttpRequest();
  x.open('GET','/v1/status',true);
  x.onload=function(){
    if(x.status!==200){document.getElementById('container').innerHTML='<div class="empty">API error: '+x.status+'</div>';return}
    var s=JSON.parse(x.responseText);
    document.getElementById('headerInfo').innerHTML='<span class="dot green"></span>connected &middot; uptime '+s.uptime+' &middot; '+s.reconcile_count+' reconcile(s)';
    if(s.last_reconcile_error){
      document.getElementById('headerInfo').innerHTML+='<br><span style="color:#f87171">&#9888; '+s.last_reconcile_error+'</span>';
    }
    document.getElementById('refreshInfo').textContent='last refresh: '+new Date().toLocaleTimeString();
    var cs=s.containers||[];
    if(cs.length===0){
      document.getElementById('container').innerHTML='<div class="empty">No containers managed yet. Add a stack to get started.</div>';
      return;
    }
    var html='';
    for(var i=0;i<cs.length;i++){
      var c=cs[i];
      var badge=c.running?'<span class="status-badge badge-up">RUNNING</span>':'<span class="status-badge badge-down">DOWN</span>';
      var health='<span class="health-unknown">-</span>';
      if(c.healthy===true)health='<span class="health-ok">&#x2713; healthy</span>';
      else if(c.healthy===false)health='<span class="health-fail">&#x2717; unhealthy</span>';
      var ports=c.ports&&c.ports.length?c.ports.join(', '):'-';
      var net=c.network||'-';
      var img=c.image||'';
      html+='<div class="card">'+
        '<div class="card-header">'+
          '<span class="name">'+c.name+'</span>'+
          '<span class="image">'+img+'</span>'+
          badge+
        '</div>'+
        '<div class="card-body">'+
          '<div class="field"><span class="field-label">Network:</span><span class="field-value">'+net+'</span></div>'+
          '<div class="field"><span class="field-label">Ports:</span><span class="field-value">'+ports+'</span></div>'+
          '<div class="field"><span class="field-label">Health:</span>'+health+'</div>'+
          '<div class="field"><span class="field-label">PID:</span><span class="field-value">'+(c.pid||'-')+'</span></div>'+
        '</div>'+
      '</div>';
    }
    document.getElementById('container').innerHTML=html;
  };
  x.onerror=function(){document.getElementById('container').innerHTML='<div class="empty">Cannot reach daemon</div>'}
  x.send();
}
refresh();setInterval(refresh,3000);
</script>
</body>
</html>
`

// formatPorts returns a human-readable list of port mappings for a service.
func formatPorts(svc types.Service) []string {
	if len(svc.Ports) == 0 {
		return nil
	}
	out := make([]string, len(svc.Ports))
	for i, p := range svc.Ports {
		if svc.Network == "public" || svc.Network == "host" {
			out[i] = fmt.Sprintf("%d", p.Container)
		} else if p.Host == p.Container {
			out[i] = fmt.Sprintf("%d", p.Host)
		} else {
			out[i] = fmt.Sprintf("%d→%d", p.Host, p.Container)
		}
	}
	return out
}
