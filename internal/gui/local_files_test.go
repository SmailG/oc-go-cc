package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// logTestServer builds a Server without starting the HTTP machinery; the handlers
// under test only need the config path and the log path.
func logTestServer(t *testing.T, logContents string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "routatic-proxy.log")
	if err := os.WriteFile(logPath, []byte(logContents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(LogFileEnvVar, logPath)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"host":"127.0.0.1","port":3456}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Server{atomicCfg: config.NewAtomicConfig(&config.Config{Host: "127.0.0.1", Port: 3456}, cfgPath)}, logPath
}

// The env override wins over the daemon default path.
func TestLogFilePath_PrefersEnvOverride(t *testing.T) {
	t.Setenv(LogFileEnvVar, "/tmp/elsewhere.log")
	p, err := logFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if p != "/tmp/elsewhere.log" {
		t.Errorf("logFilePath = %q, want the override", p)
	}

	// Without the override the daemon default applies, which is the path the
	// launchd plist redirects stdout into.
	t.Setenv(LogFileEnvVar, "")
	p, err = logFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(p); base != "routatic-proxy.log" {
		t.Errorf("default log path = %q, want a routatic-proxy.log file", p)
	}
}

// Complete lines survive; a window starting mid-line drops its fragment.
func TestSplitLogLines(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		dropFirst bool
		want      []string
	}{
		{"complete lines", "a\nb\n", false, []string{"a", "b"}},
		{"no trailing newline", "a\nb", false, []string{"a", "b"}},
		{"blank lines survive", "a\n\nc\n", false, []string{"a", "", "c"}},
		{"crlf", "a\r\nb\r\n", false, []string{"a", "b"}},
		{"empty", "", false, nil},
		{"mid-line window drops the fragment", "ment\nnext\n", true, []string{"next"}},
		{"window with no newline is all fragment", "one huge line", true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitLogLines([]byte(tc.in), tc.dropFirst)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The initial paint returns the tail, its path, and no truncation.
func TestHandleLogs(t *testing.T) {
	s, _ := logTestServer(t, "line1\nline2\nline3\n")

	rec := httptest.NewRecorder()
	s.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Path      string   `json:"path"`
		Exists    bool     `json:"exists"`
		Lines     []string `json:"lines"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Exists || strings.Join(got.Lines, ",") != "line1,line2,line3" {
		t.Errorf("got %#v", got)
	}
	if got.Truncated {
		t.Error("a file smaller than the window must not report truncation")
	}
	if got.Path == "" {
		t.Error("the response must carry the path so the UI can show it")
	}
}

// A proxy started from a terminal writes stdout to the tty, not the log file, so
// the file can legitimately be absent. That is a hint for the UI, not an error.
func TestHandleLogs_MissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(LogFileEnvVar, filepath.Join(dir, "absent.log"))
	s := &Server{}

	rec := httptest.NewRecorder()
	s.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Exists bool     `json:"exists"`
		Lines  []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Exists || len(got.Lines) != 0 {
		t.Errorf("got %#v, want exists=false with an empty line array", got)
	}
}

// The opener receives the live config path.
func TestHandleConfigOpen(t *testing.T) {
	s, _ := logTestServer(t, "x\n")
	var opened []string
	restore := runOpener
	runOpener = func(_ context.Context, path string) error { opened = append(opened, path); return nil }
	t.Cleanup(func() { runOpener = restore })

	rec := httptest.NewRecorder()
	s.handleConfigOpen(rec, httptest.NewRequest(http.MethodPost, "/api/config/open", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(opened) != 1 || opened[0] != s.atomicCfg.Path() {
		t.Errorf("opened %#v, want exactly %q", opened, s.atomicCfg.Path())
	}
}

// Non-POST requests and a missing config file are refused.
func TestHandleConfigOpen_Rejects(t *testing.T) {
	s, _ := logTestServer(t, "x\n")
	called := false
	restore := runOpener
	runOpener = func(context.Context, string) error { called = true; return nil }
	t.Cleanup(func() { runOpener = restore })

	t.Run("get", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handleConfigOpen(rec, httptest.NewRequest(http.MethodGet, "/api/config/open", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		missing := &Server{atomicCfg: config.NewAtomicConfig(&config.Config{}, filepath.Join(dir, "gone.json"))}
		rec := httptest.NewRecorder()
		missing.handleConfigOpen(rec, httptest.NewRequest(http.MethodPost, "/api/config/open", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	if called {
		t.Error("the opener ran for a rejected request")
	}
}

// The paths endpoint reports both local files.
func TestHandleConfigPath(t *testing.T) {
	s, logPath := logTestServer(t, "x\n")

	rec := httptest.NewRecorder()
	s.handleConfigPath(rec, httptest.NewRequest(http.MethodGet, "/api/config/path", nil))

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["config_path"] != s.atomicCfg.Path() {
		t.Errorf("config_path = %q, want %q", got["config_path"], s.atomicCfg.Path())
	}
	if got["log_path"] != logPath {
		t.Errorf("log_path = %q, want %q", got["log_path"], logPath)
	}
}

// readSSEFrame pulls one "event: ...\ndata: ..." frame off the stream.
func readSSEFrame(t *testing.T, sc *bufio.Scanner) (event, data string) {
	t.Helper()
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "event: ") {
			t.Fatalf("unexpected frame line %q", line)
		}
		event = strings.TrimPrefix(line, "event: ")
		if !sc.Scan() {
			t.Fatalf("frame %q has no data line: %v", event, sc.Err())
		}
		return event, strings.TrimPrefix(sc.Text(), "data: ")
	}
	t.Fatalf("stream ended: %v", sc.Err())
	return "", ""
}

// Appends are delivered and in-place truncation emits rotate.
func TestHandleLogStream_FollowsAppendsAndTruncation(t *testing.T) {
	old := logPollInterval
	logPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { logPollInterval = old })

	s, logPath := logTestServer(t, "first\nsecond\n")
	srv := httptest.NewServer(http.HandlerFunc(s.handleLogStream))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("appended\npart"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// Only the complete line may be delivered; "part" has no newline yet.
	event, data := readSSEFrame(t, sc)
	if event != "line" || data != `"appended"` {
		t.Fatalf("event = %q data = %q, want line/appended", event, data)
	}

	// Truncation is how a log file gets replaced in place.
	if err := os.WriteFile(logPath, []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	event, _ = readSSEFrame(t, sc)
	if event != "rotate" {
		t.Fatalf("event = %q, want rotate after truncation", event)
	}
	event, data = readSSEFrame(t, sc)
	if event != "line" || data != `"fresh"` {
		t.Fatalf("after rotate: event = %q data = %q, want line/fresh", event, data)
	}
}

// The panel paints the tail from GET /api/logs and then follows from EOF, so the
// stream must start at the end and never replay the window already rendered.
func TestHandleLogStream_StartsAtEOF(t *testing.T) {
	old := logPollInterval
	logPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { logPollInterval = old })

	s, logPath := logTestServer(t, "before\n")
	srv := httptest.NewServer(http.HandlerFunc(s.handleLogStream))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	event, data := readSSEFrame(t, sc)
	if event != "line" || data != `"after"` {
		t.Fatalf("event = %q data = %q, want the first frame to be line/after", event, data)
	}
}

// Rename-based rotation is the common logrotate mode: the descriptor keeps
// pointing at the old inode, so only a path comparison can notice the swap.
func TestHandleLogStream_ReopensAfterRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.SameFile itself works on Windows (volume + file index), but Go opens
		// files without FILE_SHARE_DELETE, so the rename fails while the stream
		// holds the old handle; Windows rotation is copytruncate (covered above).
		t.Skip("cannot rename an open log file on Windows")
	}
	old := logPollInterval
	logPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { logPollInterval = old })

	s, logPath := logTestServer(t, "old\n")
	srv := httptest.NewServer(http.HandlerFunc(s.handleLogStream))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	event, _ := readSSEFrame(t, sc)
	if event != "rotate" {
		t.Fatalf("event = %q, want rotate after the path was replaced", event)
	}
	event, data := readSSEFrame(t, sc)
	if event != "line" || data != `"new"` {
		t.Fatalf("after rotate: event = %q data = %q, want line/new", event, data)
	}
}

// A directory is not a log; opening it must fail before streaming starts, and
// the 5xx body must not echo the filesystem path.
func TestHandleLogs_RejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(LogFileEnvVar, dir)
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a non-regular log path", rec.Code)
	}
	if strings.Contains(rec.Body.String(), dir) {
		t.Errorf("the response leaked the log path: %s", rec.Body.String())
	}
}
