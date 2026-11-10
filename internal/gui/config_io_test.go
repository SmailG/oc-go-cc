package gui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/routatic/proxy/internal/config"
)

// An import is a full config, so it gets a larger cap than a patch — but a
// bounded one.
func TestHandleConfigImport_RejectsOversizedBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"host":"127.0.0.1","port":3456}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{atomicCfg: config.NewAtomicConfig(&config.Config{Host: "127.0.0.1", Port: 3456}, path)}

	body := bytes.Repeat([]byte("x"), maxConfigImportBytes+1)
	rec := httptest.NewRecorder()
	s.handleConfigImport(rec, httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}
