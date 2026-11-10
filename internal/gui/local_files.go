package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/routatic/proxy/internal/daemon"
)

// Local files the dashboard can surface: the config the running process loaded,
// and the log file launchd redirects the server's stdout into.
const (
	// LogFileEnvVar overrides the log path the dashboard follows, for installs
	// that redirect stdout somewhere other than the daemon default.
	LogFileEnvVar = "ROUTATIC_PROXY_LOG_FILE"

	logTailBytes         = 64 << 10
	logReadChunkMaxBytes = 256 << 10
	logMaxLines          = 500
	logMaxCarry          = 1 << 20
	// A rotated log is renamed away and recreated; a poll that lands between the
	// two sees no file. Only report it gone after about ten seconds of misses at
	// the default interval.
	logFileMissingTicks = 25
)

// Poll interval and follower cap are variables so tests can drive a fast tick.
// The dashboard has one log panel, so the cap only exists to stop a leaked
// EventSource from piling up readers.
var (
	logPollInterval = 400 * time.Millisecond
	logFollowers    atomic.Int64
	logFollowLimit  = int64(4)
)

// runOpener is swapped in tests so they never launch a real viewer.
var runOpener = defaultOpener

func defaultOpener(ctx context.Context, path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", path)
	case "windows":
		// explorer.exe reports success through a non-zero exit status.
		if err := exec.CommandContext(ctx, "explorer", path).Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return nil
			}
			return err
		}
		return nil
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", path)
	}
	return cmd.Run()
}

// logFilePath resolves the log file to read, preferring the explicit override.
func logFilePath() (string, error) {
	if p := os.Getenv(LogFileEnvVar); p != "" {
		return p, nil
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return "", err
	}
	return paths.LogFile, nil
}

// tailFile reads the last limitBytes of path. A partial leading line is dropped
// when the window starts mid-file, so the caller only ever sees complete lines.
func tailFile(path string, limitBytes int64) (lines []string, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	// The export path is operator-controlled, but streaming a device or FIFO
	// would block or spin forever; only plain files are logs.
	if !st.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}
	size := st.Size()
	start := int64(0)
	if size > limitBytes {
		start = size - limitBytes
		truncated = true
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, false, err
	}

	lines = splitLogLines(buf, truncated)
	if len(lines) > logMaxLines {
		lines = lines[len(lines)-logMaxLines:]
	}
	return lines, truncated, nil
}

// splitLogLines turns a byte window into log lines, discarding the leading
// fragment when the window began mid-line.
func splitLogLines(buf []byte, dropFirst bool) []string {
	out := make([]string, 0, 64)
	start := 0
	if dropFirst {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			start = i + 1
		} else {
			start = len(buf)
		}
	}
	for start <= len(buf) {
		end := len(buf)
		if i := bytes.IndexByte(buf[start:], '\n'); i >= 0 {
			end = start + i
		}
		line := trimCR(buf[start:end])
		if end < len(buf) || line != "" {
			out = append(out, line)
		}
		if end == len(buf) {
			break
		}
		start = end + 1
	}
	return out
}

func trimCR(b []byte) string {
	return string(bytes.TrimSuffix(b, []byte("\r")))
}

// handleConfigPath returns the paths the dashboard shows for the local files.
func (s *Server) handleConfigPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]string{}
	if s.atomicCfg != nil {
		resp["config_path"] = s.atomicCfg.Path()
	}
	if p, err := logFilePath(); err == nil {
		resp["log_path"] = p
	}
	writeJSON(w, resp)
}

// handleConfigOpen opens the live config file in the OS default application.
// The path is always the one the process loaded; the request carries none, so
// there is nothing for a caller to steer.
func (s *Server) handleConfigOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.atomicCfg == nil {
		http.Error(w, "config not available", http.StatusServiceUnavailable)
		return
	}

	path := s.atomicCfg.Path()
	if path == "" {
		http.Error(w, "config path is unknown", http.StatusServiceUnavailable)
		return
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, fmt.Sprintf("config file not found: %s", path), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The launcher is a third-party binary: `xdg-open` can wait for the launched
	// application to exit, so the request must not outlive a timeout.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := runOpener(ctx, path); err != nil {
		http.Error(w, fmt.Sprintf("failed to open %s: %v", path, err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"path": path})
}

// handleLogs streams the tail of the local log file as JSON for the initial paint.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path, err := logFilePath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"path":      path,
		"lines":     []string{},
		"truncated": false,
	}
	lines, truncated, err := tailFile(path, logTailBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Not an error: the proxy may be running in a terminal, where stdout
			// never reaches this file. The UI shows the path instead.
			resp["exists"] = false
			writeJSON(w, resp)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	st, err := os.Stat(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp["exists"] = true
	resp["lines"] = lines
	resp["truncated"] = truncated
	resp["size"] = st.Size()
	resp["mod_time"] = st.ModTime().UTC().Format(time.RFC3339)
	writeJSON(w, resp)
}

// handleLogStream follows the log file and pushes each new line as an SSE event.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if logFollowers.Add(1) > logFollowLimit {
		logFollowers.Add(-1)
		http.Error(w, "too many log viewers", http.StatusServiceUnavailable)
		return
	}
	defer logFollowers.Add(-1)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	path, err := logFilePath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, fmt.Sprintf("log file not found: %s", path), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	// Capture EOF before any header is flushed. Once the client sees the response
	// it may append, and a line written before the offset was read would fall
	// behind it and never be delivered.
	st, err := f.Stat()
	if err != nil {
		http.Error(w, fmt.Sprintf("reading log file: %v", err), http.StatusInternalServerError)
		return
	}
	if !st.Mode().IsRegular() {
		http.Error(w, fmt.Sprintf("%s is not a regular file", path), http.StatusUnsupportedMediaType)
		return
	}
	// Start at EOF: the panel paints the window from GET /api/logs first, so a
	// tail here would render the same lines twice. It also makes an EventSource
	// reconnect idempotent.
	offset := st.Size()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Flush the headers now: without this the response does not reach the client
	// until the first event, so a quiet log leaves the panel stuck on "connecting".
	flusher.Flush()

	ticker := time.NewTicker(logPollInterval)
	defer ticker.Stop()

	var carry []byte
	misses := 0
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st, err := f.Stat()
			if err != nil {
				continue
			}
			// f.Stat() is an fstat on the open descriptor: it never reports a
			// missing file and cannot see rename-based rotation, because the
			// descriptor keeps pointing at the old inode. Compare it with the
			// path and reopen when a new file has taken the name.
			ps, err := os.Stat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					misses++
					if misses >= logFileMissingTicks {
						sseEvent(w, flusher, "error", "log file disappeared")
						return
					}
				}
				continue
			}
			misses = 0
			if !os.SameFile(st, ps) {
				nf, err := os.Open(path)
				if err != nil {
					continue
				}
				_ = f.Close()
				f = nf
				offset = 0
				carry = carry[:0]
				if !sseEvent(w, flusher, "rotate", path) {
					return
				}
				continue
			}
			if st.Size() < offset {
				// Truncated in place (copytruncate).
				offset = 0
				carry = carry[:0]
				if !sseEvent(w, flusher, "rotate", path) {
					return
				}
				continue
			}
			if st.Size() == offset {
				continue
			}
			want := st.Size() - offset
			if want > logReadChunkMaxBytes {
				want = logReadChunkMaxBytes
			}
			chunk := make([]byte, want)
			n, err := f.ReadAt(chunk, offset)
			if n <= 0 {
				if err != nil && err != io.EOF {
					sseEvent(w, flusher, "error", err.Error())
					return
				}
				continue
			}
			offset += int64(n)

			carry = append(carry, chunk[:n]...)
			for {
				i := bytes.IndexByte(carry, '\n')
				if i < 0 {
					break
				}
				line := trimCR(carry[:i])
				carry = carry[i+1:]
				if !sseEvent(w, flusher, "line", line) {
					return
				}
			}
			if len(carry) > logMaxCarry {
				// No newline in sight; emit what we have rather than growing forever.
				if !sseEvent(w, flusher, "line", trimCR(carry)) {
					return
				}
				carry = carry[:0]
			}
		}
	}
}

// sseEvent writes one frame and flushes it. The payload is JSON-encoded so that a
// log line containing a newline cannot forge an extra frame. A false return means
// the client is gone.
func sseEvent(w io.Writer, flusher http.Flusher, event string, payload any) bool {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return false
	}
	frame := strings.TrimSuffix(buf.String(), "\n")
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, frame); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
