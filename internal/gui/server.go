// Package gui provides the embedded HTTP server that serves the GUI dashboard
// and exposes /api/* endpoints for metrics, history, and configuration.
package gui

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/routatic/proxy/internal/catalog"
	"github.com/routatic/proxy/internal/config"
	"github.com/routatic/proxy/internal/daemon"
	"github.com/routatic/proxy/internal/history"
	"github.com/routatic/proxy/internal/metrics"
	"github.com/routatic/proxy/internal/storage"
)

//go:embed assets/*
var assets embed.FS

// Config is the GUI-level configuration that the user can toggle at runtime.
type Config struct {
	Autostart bool `json:"autostart"`
	Notify    bool `json:"notify"`
}

// Server is the embedded HTTP server that backs the webview UI.
type Server struct {
	hist              *history.History
	met               *metrics.Metrics
	atomicCfg         *config.AtomicConfig
	cfg               Config
	cfgMu             sync.RWMutex
	proxyRunning      atomic.Bool
	connectedExisting atomic.Bool
	proxyPort         int
	guiPort           atomic.Int32
	startProxy        func() error
	stopProxy         func() error
	catalogDir        string
	catalogSourceURL  string
	srv               *http.Server
	logger            *slog.Logger
	catalogMu         sync.Mutex
	// configMu serializes config read-merge-write-reload sequences. The write is
	// atomic, but two concurrent saves could read the same file and the last
	// rename would discard the other patch.
	configMu sync.Mutex

	storage *storage.Database
}

// Options configures the GUI server.
type Options struct {
	History          *history.History
	Metrics          *metrics.Metrics
	AtomicConfig     *config.AtomicConfig
	ProxyPort        int
	StartProxy       func() error
	StopProxy        func() error
	CatalogDir       string
	CatalogSourceURL string
	Logger           *slog.Logger
	Storage          *storage.Database
}

// New creates a new GUI server.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{
		hist:             opts.History,
		met:              opts.Metrics,
		atomicCfg:        opts.AtomicConfig,
		proxyPort:        opts.ProxyPort,
		startProxy:       opts.StartProxy,
		stopProxy:        opts.StopProxy,
		catalogDir:       opts.CatalogDir,
		catalogSourceURL: opts.CatalogSourceURL,
		logger:           opts.Logger,

		storage: opts.Storage,
	}
	// Check initial autostart state.
	s.cfg.Autostart = isAutostartEnabled()
	return s
}

// isAutostartEnabled checks whether autostart is currently enabled.
// On macOS it checks ~/Library/LaunchAgents/{LaunchAgent}.plist.
// On Linux it checks ~/.config/autostart/{LaunchAgent}.desktop.
func isAutostartEnabled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	if runtime.GOOS == "darwin" {
		plist := filepath.Join(home, "Library", "LaunchAgents", daemon.LaunchAgent+".plist")
		_, err = os.Stat(plist)
		return err == nil
	}

	if runtime.GOOS == "linux" {
		desktop := filepath.Join(home, ".config", "autostart", daemon.LaunchAgent+".desktop")
		_, err = os.Stat(desktop)
		return err == nil
	}

	return false
}

// SetProxyRunning updates the running state (called by the proxy lifecycle).
func (s *Server) SetProxyRunning(running bool) {
	s.proxyRunning.Store(running)
}

// SetConnectedToExisting updates whether the GUI is monitoring an external proxy
// rather than controlling a locally-started one.
func (s *Server) SetConnectedToExisting(connected bool) {
	s.connectedExisting.Store(connected)
}

// getProxyPort returns the current proxy port from config if available.
func (s *Server) getProxyPort() int {
	if s.atomicCfg != nil {
		return s.atomicCfg.Get().Port
	}
	return s.proxyPort
}

// Start starts the embedded HTTP server on port 3445 and returns
// the URL that the webview should load. If another routatic-proxy instance
// is using that port, it is killed before binding.
func (s *Server) Start(ctx context.Context) (string, error) {
	s.guiPort.Store(3445)
	// Allow GUI port override via environment variable.
	if envPort := os.Getenv("ROUTATIC_PROXY_GUI_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p < 65536 {
			s.guiPort.Store(int32(p))
		}
	}
	// Ensure port is free, killing any existing routatic-proxy GUI.
	if err := s.ensurePortAvailable(); err != nil {
		return "", fmt.Errorf("gui port check: %w", err)
	}

	mux := http.NewServeMux()

	// Static assets — strip the "assets/" prefix so index.html is served at /.
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return "", fmt.Errorf("gui assets embed: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	// API endpoints.
	mux.HandleFunc("/api/metrics", s.handleMetrics)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/proxy/config", s.handleProxyConfig)
	mux.HandleFunc("/api/proxy/start", s.handleProxyStart)
	mux.HandleFunc("/api/proxy/stop", s.handleProxyStop)
	mux.HandleFunc("/api/catalog/lock", s.handleCatalogLock)
	mux.HandleFunc("/api/catalog/sync", s.handleCatalogSync)
	mux.HandleFunc("/api/test/send", s.handleTestSend)

	// New endpoints for advanced GUI features

	mux.HandleFunc("/api/config/export", s.handleConfigExport)
	mux.HandleFunc("/api/config/import", s.handleConfigImport)
	mux.HandleFunc("/api/config/open", s.handleConfigOpen)
	mux.HandleFunc("/api/config/path", s.handleConfigPath)
	mux.HandleFunc("/api/config/env", s.handleConfigEnv)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/logs/stream", s.handleLogStream)
	mux.HandleFunc("/api/perf/models", s.handlePerformance)
	mux.HandleFunc("/api/perf/aggregate", s.handlePerformanceAggregate)
	mux.HandleFunc("/api/catalog/stats", s.handleCatalogStats)

	// Analytics (only when SQLite storage is available)
	if s.storage != nil {
		ah := NewAnalyticsHandler(s.storage)
		mux.HandleFunc("/api/analytics/summary", ah.Summary)
		mux.HandleFunc("/api/analytics/tokens/trend", ah.TokenTrend)
		mux.HandleFunc("/api/analytics/latency", ah.LatencyStats)
	}

	startPort := int(s.guiPort.Load())
	var ln net.Listener
	for p := startPort; p < startPort+10; p++ {
		var err error
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			s.guiPort.Store(int32(p))
			break
		}
		if p == startPort {
			fmt.Printf("Port %d in use by another app, trying port %d for GUI\n", p, p+1)
		}
	}
	if ln == nil {
		return "", fmt.Errorf("gui server listen: no available port in range %d-%d", startPort, startPort+9)
	}

	// Wrap with security headers middleware. The request guard runs inside it so
	// every request — static assets and API alike — is checked against the
	// dashboard's loopback host before it is routed.
	s.srv = &http.Server{
		Handler: securityHeadersMiddleware(requestGuardMiddleware(int(s.guiPort.Load()), mux)),
		// Bound slow clients, including a stalled request body. WriteTimeout
		// stays unset because /api/logs/stream is a long-lived response.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
	}
	go func() {
		if srvErr := s.srv.Serve(ln); srvErr != nil && srvErr != http.ErrServerClosed {
			s.logger.Error("gui server error", "err", srvErr)
		}
	}()

	go func() {
		<-ctx.Done()
		_ = s.srv.Close()
	}()

	url := "http://" + ln.Addr().String() + "/"
	s.logger.Info("gui server started", "url", url)
	return url, nil
}

// Shutdown gracefully stops the GUI server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// ── API handlers ──────────────────────────────────────────────────────────────

type metricsResponse struct {
	ProxyRunning      bool             `json:"proxy_running"`
	ConnectedExisting bool             `json:"connected_to_existing"`
	Port              int              `json:"port"`
	RequestsReceived  int64            `json:"requests_received"`
	RequestsStreamed  int64            `json:"requests_streamed"`
	RequestsSuccess   int64            `json:"requests_success"`
	RequestsFailed    int64            `json:"requests_failed"`
	StorageDropped    int64            `json:"storage_dropped"`
	ModelCounts       map[string]int64 `json:"model_counts"`
}

// handleMetrics serves the live in-process request counters.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var snap metrics.Snapshot
	if s.met != nil {
		snap = s.met.GetSnapshot()
	}
	resp := metricsResponse{
		ProxyRunning:      s.proxyRunning.Load(),
		ConnectedExisting: s.connectedExisting.Load(),
		Port:              s.getProxyPort(),
		RequestsReceived:  snap.RequestsReceived,
		RequestsStreamed:  snap.RequestsStreamed,
		RequestsSuccess:   snap.RequestsSuccess,
		RequestsFailed:    snap.RequestsFailed,
		StorageDropped:    snap.StorageDropped,
		ModelCounts:       snap.ModelCounts,
	}
	writeJSON(w, resp)
}

type historyEntry struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Scenario     string `json:"scenario"`
	StartTime    string `json:"start_time"` // RFC3339
	DurationMs   int64  `json:"duration_ms"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Streaming    bool   `json:"streaming"`
	Success      bool   `json:"success"`
	ErrorMsg     string `json:"error_msg,omitempty"`
}

// handleHistory serves the last 200 requests from the in-memory ring buffer.
func (s *Server) handleHistory(w http.ResponseWriter, _ *http.Request) {
	if s.hist == nil {
		writeJSON(w, []historyEntry{})
		return
	}
	records := s.hist.Last(200)
	out := make([]historyEntry, len(records))
	for i, rec := range records {
		out[i] = historyEntry{
			ID:           rec.ID,
			Model:        rec.Model,
			Provider:     rec.Provider,
			Scenario:     rec.Scenario,
			StartTime:    rec.StartTime.Format("2006-01-02T15:04:05Z07:00"),
			DurationMs:   rec.Duration.Milliseconds(),
			InputTokens:  rec.InputTokens,
			OutputTokens: rec.OutputTokens,
			Streaming:    rec.Streaming,
			Success:      rec.Success,
			ErrorMsg:     rec.ErrorMsg,
		}
	}
	writeJSON(w, out)
}

// handleConfig reads and updates the GUI runtime settings (autostart, notifications).
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.cfgMu.RLock()
		cfg := s.cfg
		s.cfgMu.RUnlock()
		writeJSON(w, cfg)

	case http.MethodPost:
		var req struct {
			Autostart *bool `json:"autostart"`
			Notify    *bool `json:"notify"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.cfgMu.Lock()
		if req.Autostart != nil {
			s.cfg.Autostart = *req.Autostart
			if *req.Autostart {
				_ = daemon.EnableAutostart("", s.getProxyPort())
			} else {
				_ = daemon.DisableAutostart()
			}
		}
		if req.Notify != nil {
			s.cfg.Notify = *req.Notify
		}
		s.cfgMu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleProxyStart starts the managed proxy process.
func (s *Server) handleProxyStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.proxyRunning.Load() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.startProxy != nil {
		if err := s.startProxy(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProxyStop stops the managed proxy process.
func (s *Server) handleProxyStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.proxyRunning.Load() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.stopProxy != nil {
		if err := s.stopProxy(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProxyConfig returns the live config and applies partial updates to it.
func (s *Server) handleProxyConfig(w http.ResponseWriter, r *http.Request) {
	if s.atomicCfg == nil {
		http.Error(w, "proxy config not available", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		cfg := s.atomicCfg.Get()
		writeJSON(w, cfg)

	case http.MethodPost:
		// Read and decode the body before taking the config lock: a client that
		// stalls mid-body must not block other saves or imports.
		r.Body = http.MaxBytesReader(w, r.Body, maxConfigPatchBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "config patch too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, fmt.Sprintf("failed to read config patch: %v", err), http.StatusBadRequest)
			return
		}
		var patch map[string]any
		if err := decodeJSONMap(body, &patch); err != nil {
			http.Error(w, fmt.Sprintf("invalid config format: %v", err), http.StatusBadRequest)
			return
		}

		// From here the sequence reads config.json, so serialize it with other
		// saves and imports.
		s.configMu.Lock()
		defer s.configMu.Unlock()

		path := s.atomicCfg.Path()
		// Merge onto the file as stored, so ${VAR} references the patch did not
		// replace (at any depth) and unknown keys survive the save.
		data, err := mergeConfigPatch(path, patch)
		if err != nil {
			s.logInternal("merging config patch", err)
			http.Error(w, "could not apply the config patch", http.StatusInternalServerError)
			return
		}

		// Validate the merged document with the real loader before it can replace
		// config.json: every loader check (API keys, URLs, extra_body, model
		// overrides, ...) must pass, not just the ones checked here. A rejected
		// patch used to leave an unbootable file on disk when Reload failed.
		tmp, err := writeTempConfig(path, data)
		if err != nil {
			s.logInternal("staging config", err)
			http.Error(w, "could not stage the config", http.StatusInternalServerError)
			return
		}
		defer func() { _ = os.Remove(tmp) }()

		merged, err := config.LoadFromPath(tmp)
		if err != nil {
			http.Error(w, fmt.Sprintf("config rejected: %v", err), http.StatusBadRequest)
			return
		}

		// Validate essential fields.
		if merged.Host == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}
		if merged.Port < 1 || merged.Port > 65535 {
			http.Error(w, "port must be between 1 and 65535", http.StatusBadRequest)
			return
		}

		// A disabled jev_routing block is inert and skipped by the loader, so the
		// block this patch just enabled may be one the loader would reject.
		if err := config.CheckJevRouting(merged.JevRouting); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Atomic replace: the validated document becomes config.json in one rename.
		if err := os.Rename(tmp, path); err != nil {
			s.logInternal("replacing config file", err)
			http.Error(w, "could not replace the config file", http.StatusInternalServerError)
			return
		}

		// Reload configuration atomically so the running proxy picks up changes.
		if err := s.atomicCfg.Reload(); err != nil {
			s.logInternal("reloading config", err)
			http.Error(w, "could not reload the config", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type catalogLockResponse struct {
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	Bytes      int64      `json:"bytes,omitempty"`
	TTLHours   int        `json:"ttl_hours,omitempty"`
	AgeSeconds int64      `json:"age_seconds"`
	Synced     bool       `json:"synced"`
}

const maxTestRequestBody = 1 << 20

// maxConfigPatchBytes bounds the partial config update body. The dashboard sends
// a handful of changed fields, so anything larger is a mistake or an abuse.
const maxConfigPatchBytes = 1 << 20

// maxConfigImportBytes bounds a config import. A full exported config is much
// larger than a partial patch, so it gets its own, more generous cap.
const maxConfigImportBytes = 4 << 20

// handleTestSend proxies a chat request to the proxy server and streams the
// response back. This avoids CORS issues that would arise from the browser
// calling the proxy port directly.
func (s *Server) handleTestSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	proxyPort := s.getProxyPort()
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d/v1/messages", proxyPort)

	r.Body = http.MaxBytesReader(w, r.Body, maxTestRequestBody)
	defer func() { _ = r.Body.Close() }()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	// Forward to the proxy.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, proxyURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed to create proxy request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("proxy request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy status code and headers.
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	written, err := io.Copy(w, resp.Body)
	if err != nil {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("failed to stream proxy response", "err", err, "bytes_written", written)
	}
}

// handleCatalogLock reports the on-disk catalog's sync metadata.
func (s *Server) handleCatalogLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lock, err := catalog.ReadLock(s.catalogDir)
	if err != nil {
		writeJSON(w, catalogLockResponse{Synced: false, AgeSeconds: -1})
		return
	}

	age := time.Since(lock.SyncedAt)
	resp := catalogLockResponse{
		SyncedAt:   &lock.SyncedAt,
		SHA256:     lock.SHA256,
		Bytes:      lock.Bytes,
		TTLHours:   lock.TTLHours,
		AgeSeconds: int64(age.Seconds()),
		Synced:     true,
	}
	writeJSON(w, resp)
}

// handleCatalogSync refreshes the catalog from its configured source.
func (s *Server) handleCatalogSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.catalogSourceURL == "" || s.catalogDir == "" {
		http.Error(w, "catalog sync is not configured", http.StatusServiceUnavailable)
		return
	}

	// Serialize manual syncs so the lock file and on-disk catalog stay consistent.
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	lock, err := catalog.Sync(s.catalogSourceURL, s.catalogDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("catalog sync failed: %v", err), http.StatusInternalServerError)
		return
	}

	age := time.Since(lock.SyncedAt)
	writeJSON(w, catalogLockResponse{
		SyncedAt:   &lock.SyncedAt,
		SHA256:     lock.SHA256,
		Bytes:      lock.Bytes,
		TTLHours:   lock.TTLHours,
		AgeSeconds: int64(age.Seconds()),
		Synced:     true,
	})
}

// handleCatalogStats serves catalog size and provider metrics.
func (s *Server) handleCatalogStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.storage == nil {
		writeJSON(w, map[string]any{
			"available": false,
			"error":     "storage not configured",
		})
		return
	}

	repo := storage.NewCatalogRepo(s.storage)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	idx, err := repo.Load(ctx)
	if err != nil {
		writeJSON(w, map[string]any{
			"available": false,
			"error":     err.Error(),
		})
		return
	}

	lastSync, _ := repo.LastSync(ctx)

	providersByEnabled := make(map[string]int)
	for _, p := range idx.Providers {
		enabled := "disabled"
		if p.Enabled != nil && *p.Enabled {
			enabled = "enabled"
		}
		providersByEnabled[enabled]++
	}

	modelsByProvider := make(map[string]int)
	for prov := range idx.Providers {
		modelsByProvider[prov] = len(idx.ProviderModels[prov])
	}

	totalModels := len(idx.Models)
	modelsWithTools := 0
	modelsWithVision := 0
	modelsWithReasoning := 0
	for _, m := range idx.Models {
		if m.ToolCall {
			modelsWithTools++
		}
		if m.Vision {
			modelsWithVision++
		}
		if m.Reasoning {
			modelsWithReasoning++
		}
	}

	resp := map[string]any{
		"available":             true,
		"last_sync":             lastSync,
		"total_providers":       len(idx.Providers),
		"providers_enabled":     providersByEnabled["enabled"],
		"providers_disabled":    providersByEnabled["disabled"],
		"total_models":          totalModels,
		"models_with_tools":     modelsWithTools,
		"models_with_vision":    modelsWithVision,
		"models_with_reasoning": modelsWithReasoning,
		"models_by_provider":    modelsByProvider,
	}

	writeJSON(w, resp)
}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// logInternal records a handler's internal failure without echoing filesystem
// paths or parser detail to the client.
func (s *Server) logInternal(msg string, err error) {
	if s.logger != nil {
		s.logger.Error(msg, "err", err)
	}
}

// ensurePortAvailable checks if an existing routatic-proxy GUI is running on
// the configured port and kills it if found. It does not test-bind — the caller
// handles binding and will retry with higher ports on failure.
func (s *Server) ensurePortAvailable() error {
	client := &http.Client{Timeout: 2 * time.Second}
	startPort := int(s.guiPort.Load())

	for p := startPort; p < startPort+10; p++ {
		// Check if this port responds to the routatic-proxy metrics endpoint
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/metrics", p))
		if err != nil {
			// No HTTP response → port is free or another app
			continue
		}

		var m struct {
			ProxyRunning bool `json:"proxy_running"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&m)
		_ = resp.Body.Close()

		if decodeErr == nil {
			// Our GUI — kill the existing instance
			s.logger.Info("killing existing routatic-proxy GUI on port", "port", p)
			if err := s.killProcessOnPort(p); err != nil {
				return fmt.Errorf("failed to kill existing instance: %w", err)
			}
			s.guiPort.Store(int32(p))
			return nil
		}

		// Responded but not our metrics format → another app
		if p == startPort {
			fmt.Printf("Port %d in use by another app, trying port %d for GUI\n", p, p+1)
		}
	}

	return nil
}

// killProcessOnPort terminates the process listening on the given port.
// Platform-aware: uses lsof+ps on Unix, netstat+tasklist on Windows.
func (s *Server) killProcessOnPort(port int) error {
	pids, err := s.findPIDsOnPort(port)
	if err != nil || len(pids) == 0 {
		return nil
	}

	killed := false
	for _, pid := range pids {
		if !s.isRoutaticProxyProcess(pid) {
			continue
		}
		s.logger.Info("terminating routatic-proxy process", "pid", pid)
		if s.killProcess(pid) {
			killed = true
		}
	}

	if !killed {
		return fmt.Errorf("no routatic-proxy process found on port %d", port)
	}

	// Wait for port to be released
	for i := 0; i < 10; i++ {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("port %d not released after killing process", port)
}

// findPIDsOnPort returns PIDs listening on the given port.
func (s *Server) findPIDsOnPort(port int) ([]int, error) {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("cmd", "/c", fmt.Sprintf("netstat -ano | findstr /r /c:\":%d \"", port))
		output, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		var pids []int
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil && pid > 0 {
					pids = append(pids, pid)
				}
			}
		}
		return pids, nil
	default:
		cmd := exec.Command("lsof", "-t", "-i", fmt.Sprintf(":%d", port))
		output, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		var pids []int
		for _, pidStr := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if pidStr == "" {
				continue
			}
			if pid, err := strconv.Atoi(pidStr); err == nil {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
}

// isRoutaticProxyProcess checks if the PID belongs to routatic-proxy.
func (s *Server) isRoutaticProxyProcess(pid int) bool {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
		output, err := cmd.Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(output), "routatic-proxy")
	case "linux":
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return false
		}
		return strings.Contains(string(cmdline), "routatic-proxy")
	default:
		output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
		if err != nil {
			return false
		}
		name := strings.TrimSpace(string(output))
		return strings.Contains(name, "routatic-proxy")
	}
}

// killProcess terminates a process by PID. Returns true if successful.
func (s *Server) killProcess(pid int) bool {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
		return cmd.Run() == nil
	default:
		p, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		_ = p.Signal(os.Interrupt)
		time.Sleep(500 * time.Millisecond)
		_ = p.Kill()
		return true
	}
}

// securityHeadersMiddleware adds security headers to all responses.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent MIME-type sniffing.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Restrict all resources to same-origin (all assets bundled in binary).
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}
