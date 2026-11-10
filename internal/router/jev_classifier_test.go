package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/routatic/proxy/internal/config"
)

// jevTestKey is a canary, not a credential. Tests assert it never reaches a
// log line or an error string.
const (
	jevTestKeyEnv = "JEV_TEST_KEY_ENV"
	jevTestKey    = "canary-key-9f3a1c77"
)

// jevStub is an httptest server standing in for api.typesafe.ai. It records the
// last request it saw; hits counts requests that reached it.
type jevStub struct {
	srv   *httptest.Server
	hits  atomic.Int32
	last  atomic.Pointer[jevSeen]
	serve func(w http.ResponseWriter, r *http.Request)
}

type jevSeen struct {
	method, path, auth, contentType string
	body                            []byte
}

func newJevStub(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) *jevStub {
	t.Helper()
	s := &jevStub{serve: serve}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		s.last.Store(&jevSeen{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: body,
		})
		s.serve(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// jevOKBody builds a 200 response body with a Score and a Noul answer.
func jevOKBody(score, noul float64) string {
	return fmt.Sprintf(`{"model":"jev-1.14.0","answers":{
		"action":{"type":"score","score":%v,"confidence":0.9,"probabilities":{"0":0.1,"1":0.2,"2":0.7}},
		"architecture":{"type":"noul","noul":%v}},
		"usage":{"input_tokens":10,"output_tokens":4}}`, score, noul)
}

func respondJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func jevTestConfig(baseURL string) config.JevRoutingConfig {
	return config.JevRoutingConfig{
		Enabled: true, Mode: config.JevModeShadow, Model: "jev-1.14.0",
		TimeoutMs: 1000, APIKeyEnv: jevTestKeyEnv, BaseURL: baseURL,
	}
}

func classifyWith(t *testing.T, stub *jevStub, text string) (JevResult, error) {
	t.Helper()
	t.Setenv(jevTestKeyEnv, jevTestKey)
	c := newJevClassifier(http.DefaultClient, jevTestConfig(stub.srv.URL))
	return c.Classify(context.Background(), text)
}

func TestJevClassifier_LevelFromScore(t *testing.T) {
	cases := []struct {
		name  string
		score float64
		want  int
	}{
		{"level 0", 0.0, 0},
		{"level 1", 1.0, 1},
		{"level 2", 2.0, 2},
		{"rounds down", 0.49, 0},
		{"rounds up", 0.5, 1},
		{"between 1 and 2 rounds to 2", 1.6, 2},
		// A 50/50 split between levels 0 and 2 also scores 1.0: the hedge is the
		// default level, which is what rounding the weighted mean gives.
		{"split between 0 and 2", 1.0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newJevStub(t, respondJSON(jevOKBody(tc.score, 0.1)))
			got, err := classifyWith(t, stub, "do something")
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got.Level != tc.want {
				t.Errorf("Level = %d, want %d", got.Level, tc.want)
			}
		})
	}
}

func TestJevClassifier_ParsesNoulAndLatency(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.83)))
	got, err := classifyWith(t, stub, "do something")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.ArchitectureProb != 0.83 {
		t.Errorf("ArchitectureProb = %v, want 0.83", got.ArchitectureProb)
	}
	if got.Latency <= 0 {
		t.Errorf("Latency = %v, want > 0", got.Latency)
	}
}

// The wire shape is checked against a real client talking to the stub, field by
// field: only model, state and questions, and state is only {"request": text}.
func TestJevClassifier_RequestShape(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	if _, err := classifyWith(t, stub, "fix the bug in parse()"); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	seen := stub.last.Load()
	if seen.method != http.MethodPost || seen.path != "/v1/systemone" {
		t.Errorf("got %s %s, want POST /v1/systemone", seen.method, seen.path)
	}
	if seen.auth != "Bearer "+jevTestKey {
		t.Errorf("Authorization header not sent as a bearer token")
	}
	if seen.contentType != "application/json" {
		t.Errorf("Content-Type = %q", seen.contentType)
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(seen.body, &req); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(req) != 3 {
		t.Errorf("top-level keys = %d, want exactly model/state/questions: %s", len(req), seen.body)
	}
	if string(req["model"]) != `"jev-1.14.0"` {
		t.Errorf("model = %s", req["model"])
	}
	if string(req["state"]) != `{"request":"fix the bug in parse()"}` {
		t.Errorf("state = %s, want only the request field", req["state"])
	}
	assertQuestions(t, req["questions"])
}

func assertQuestions(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var qs map[string]struct {
		Type         string   `json:"type"`
		Instructions string   `json:"instructions"`
		Criteria     []string `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &qs); err != nil {
		t.Fatalf("questions: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("questions = %d, want 2", len(qs))
	}
	action, arch := qs["action"], qs["architecture"]
	if action.Type != "score" || len(action.Criteria) != 3 || action.Instructions == "" {
		t.Errorf("action question malformed: %+v", action)
	}
	if arch.Type != "noul" || !strings.Contains(arch.Instructions, "`request`") {
		t.Errorf("architecture question malformed: %+v", arch)
	}
}

func TestTruncateChars(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want int // rune count of the result
	}{
		{"shorter", "abc", 600, 3},
		{"exactly max", strings.Repeat("a", 600), 600, 600},
		{"one over", strings.Repeat("a", 601), 600, 600},
		{"multibyte cut on a rune boundary", strings.Repeat("é", 2000), 600, 600},
		{"four-byte runes", strings.Repeat("😀", 700), 600, 600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateChars(tc.in, tc.max)
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8")
			}
			if n := utf8.RuneCountInString(got); n != tc.want {
				t.Errorf("rune count = %d, want %d", n, tc.want)
			}
		})
	}
}

func TestJevClassifier_SendsAtMost600CharsAndNothingElse(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	long := strings.Repeat("é", 5000) // 2 bytes per char, so a byte cut would split one
	if _, err := classifyWith(t, stub, long); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	var req struct {
		State map[string]string `json:"state"`
	}
	if err := json.Unmarshal(stub.last.Load().body, &req); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(req.State) != 1 {
		t.Fatalf("state has %d keys, want 1: %v", len(req.State), req.State)
	}
	if n := utf8.RuneCountInString(req.State["request"]); n != 600 {
		t.Errorf("request chars = %d, want 600", n)
	}
}

func TestJevClassifier_Failures(t *testing.T) {
	status := func(code int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			// The body echoes the credential, as a hostile or buggy server might.
			_, _ = io.WriteString(w, `{"error":"`+jevTestKey+`"}`)
		}
	}
	wrongType := `{"answers":{"action":{"type":"noul","noul":0.5},"architecture":{"type":"noul","noul":0.1}}}`
	cases := []struct {
		name    string
		serve   func(http.ResponseWriter, *http.Request)
		wantErr string
	}{
		{"http 429", status(429), "429"},
		{"http 500", status(500), "500"},
		{"http 401", status(401), "401"},
		{"malformed json", respondJSON(`{"answers":`), "malformed"},
		{"not json at all", respondJSON(`<html>`), "malformed"},
		{"no answers", respondJSON(`{"answers":{}}`), "missing"},
		{"action answer has wrong type", respondJSON(wrongType), "missing"},
		{"score below range", respondJSON(jevOKBody(-0.2, 0.1)), "out of range"},
		{"score above range", respondJSON(jevOKBody(2.4, 0.1)), "out of range"},
		{"noul below range", respondJSON(jevOKBody(1, -0.1)), "out of range"},
		{"noul above range", respondJSON(jevOKBody(1, 1.2)), "out of range"},
		{"action typed noul but carrying a score", respondJSON(`{"answers":{"action":{"type":"noul","score":1},"architecture":{"type":"noul","noul":0.1}}}`), "missing"},
		{"architecture typed score but carrying a noul", respondJSON(`{"answers":{"action":{"type":"score","score":1},"architecture":{"type":"score","noul":0.1}}}`), "missing"},
		{"noul null", respondJSON(`{"answers":{"action":{"type":"score","score":1},"architecture":{"type":"noul","noul":null}}}`), "missing"},
		{"score null", respondJSON(`{"answers":{"action":{"type":"score","score":null},"architecture":{"type":"noul","noul":0.1}}}`), "missing"},
		{"noul missing", respondJSON(`{"answers":{"action":{"type":"score","score":1}}}`), "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newJevStub(t, tc.serve)
			_, err := classifyWith(t, stub, "do something")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), jevTestKey) {
				t.Errorf("error leaks the credential or a response body: %v", err)
			}
		})
	}
}

func TestJevClassifier_Timeout(t *testing.T) {
	release := make(chan struct{})
	stub := newJevStub(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) }) // runs before srv.Close (LIFO)
	t.Setenv(jevTestKeyEnv, jevTestKey)
	c := newJevClassifier(http.DefaultClient, jevTestConfig(stub.srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Classify(ctx, "do something")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Classify took %v with a 40ms deadline", elapsed)
	}
}

func TestJevClassifier_NoRequestWhenUnusable(t *testing.T) {
	cases := []struct {
		name    string
		keyEnv  string // value of the key variable
		text    string
		wantErr string
	}{
		{"missing api key", "", "do something", "api key"},
		{"empty text", jevTestKey, "", "no request text"},
		{"whitespace-only text", jevTestKey, " \n\t ", "no request text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
			t.Setenv(jevTestKeyEnv, tc.keyEnv)
			c := newJevClassifier(http.DefaultClient, jevTestConfig(stub.srv.URL))
			_, err := c.Classify(context.Background(), tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
			if n := stub.hits.Load(); n != 0 {
				t.Errorf("stub saw %d requests, want 0", n)
			}
		})
	}
}

func TestJevClassifier_BaseURLTrailingSlash(t *testing.T) {
	stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
	t.Setenv(jevTestKeyEnv, jevTestKey)
	cfg := jevTestConfig(stub.srv.URL + "/")
	if _, err := newJevClassifier(http.DefaultClient, cfg).Classify(context.Background(), "x"); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got := stub.last.Load().path; got != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", got)
	}
}

// A response larger than the read cap is cut off and fails to parse, rather than
// being buffered whole.
func TestJevClassifier_ResponseSizeIsCapped(t *testing.T) {
	padding := strings.Repeat("x", jevResponseMaxBytes+1024)
	body := `{"model":"` + padding + `","answers":{}}`
	stub := newJevStub(t, respondJSON(body))
	_, err := classifyWith(t, stub, "do something")
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("error = %v, want a malformed-response error", err)
	}
}

func TestJevClassifier_TransportFailureClasses(t *testing.T) {
	t.Setenv(jevTestKeyEnv, jevTestKey)
	t.Run("canceled", func(t *testing.T) {
		stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newJevClassifier(http.DefaultClient, jevTestConfig(stub.srv.URL)).Classify(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Errorf("error = %v, want canceled", err)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		stub := newJevStub(t, respondJSON(jevOKBody(1, 0.1)))
		url := stub.srv.URL
		stub.srv.Close() // nothing listens there now
		_, err := newJevClassifier(http.DefaultClient, jevTestConfig(url)).Classify(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "transport error") {
			t.Errorf("error = %v, want transport error", err)
		}
		if strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("error carries the URL: %v", err)
		}
	})
}

// JSON cannot carry NaN, but the range check is the last line of defence for
// values that reach it by any other path.
func TestParseJevResponse_RejectsNaN(t *testing.T) {
	nan, one := math.NaN(), 1.0
	body := func(score, noul *float64) jevResponseBody {
		return jevResponseBody{Answers: map[string]jevAnswer{
			jevActionID:       {Type: "score", Score: score},
			jevArchitectureID: {Type: "noul", Noul: noul},
		}}
	}
	if _, err := parseJevResponse(body(&nan, &one)); err == nil {
		t.Error("NaN score was accepted")
	}
	if _, err := parseJevResponse(body(&one, &nan)); err == nil {
		t.Error("NaN noul was accepted")
	}
	if _, err := parseJevResponse(body(&one, &one)); err != nil {
		t.Errorf("control: finite values rejected: %v", err)
	}
}
