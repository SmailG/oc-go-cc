package router

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/routatic/proxy/internal/config"
)

// TestJevLive_LabelledCases sends testdata/jev_eval_cases.jsonl to the REAL TypeSafe
// API and compares Jev's level with the label and with the keyword matcher's
// level. It is opt-in (JEV_LIVE_TEST=1 plus the key in the environment variable
// named by JEV_LIVE_KEY_ENV, default TYPESAFE_AI_API_KEY) and costs well under a cent.
// It reports and only fails when the API is unusable: the labels are one person's
// reading of 30 requests taken from the existing router tests, so the numbers
// are evidence to read, not a gate.
//
//	JEV_LIVE_TEST=1 go test ./internal/router/ -run JevLive -v -count=1
func TestJevLive_LabelledCases(t *testing.T) {
	if os.Getenv("JEV_LIVE_TEST") != "1" {
		t.Skip("set JEV_LIVE_TEST=1 to call the real TypeSafe API")
	}
	keyEnv := os.Getenv("JEV_LIVE_KEY_ENV")
	if keyEnv == "" {
		keyEnv = "TYPESAFE_AI_API_KEY"
	}
	if os.Getenv(keyEnv) == "" {
		t.Fatalf("%s is empty", keyEnv)
	}
	jcfg := config.JevRoutingConfig{
		Enabled: true, Mode: config.JevModeShadow, Model: os.Getenv("JEV_LIVE_MODEL"),
		TimeoutMs: 5000, APIKeyEnv: keyEnv, BaseURL: "https://api.typesafe.ai",
	}
	if jcfg.Model == "" {
		jcfg.Model = "jev-1.14.0"
	}
	var rt jevRuntime
	rt.init()
	clf := newJevClassifier(rt.client, jcfg)

	cases := loadEvalCases(t)
	type row struct {
		text                      string
		label, jev, jev2, keyword int
		arch                      float64
		ms                        int64
	}
	var rows []row
	failures := 0
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := clf.Classify(ctx, c.Text)
		res2, err2 := clf.Classify(ctx, c.Text)
		cancel()
		if err != nil || err2 != nil {
			failures++
			t.Logf("API error for %q: %v / %v", c.Text, err, err2)
			continue
		}
		rows = append(rows, row{c.Text, c.Level, res.Level, res2.Level, keywordLevel(c.Text), res.ArchitectureProb, res.Latency.Milliseconds()})
	}
	if len(rows) == 0 {
		t.Fatalf("no successful call out of %d cases", len(cases))
	}

	var jevOK, kwOK, stable int
	var lat []int64
	for _, r := range rows {
		if r.jev == r.label {
			jevOK++
		}
		if r.keyword == r.label {
			kwOK++
		}
		if r.jev == r.jev2 {
			stable++
		}
		lat = append(lat, r.ms)
		mark := " "
		if r.jev != r.label {
			mark = "J" // Jev disagrees with the label
		}
		if r.keyword != r.label {
			mark += "K" // keyword matcher disagrees with the label
		}
		t.Logf("%-3s label=%d jev=%d keyword=%d arch=%.2f %4dms  %s", mark, r.label, r.jev, r.keyword, r.arch, r.ms, truncate(r.text, 70))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	n := len(rows)
	t.Logf("model=%s cases=%d api_errors=%d", jcfg.Model, n, failures)
	t.Logf("exact level: jev %d/%d, keyword matcher %d/%d", jevOK, n, kwOK, n)
	t.Logf("jev repeat-call stability: %d/%d same level", stable, n)
	t.Logf("latency ms: p50=%d max=%d", lat[n/2], lat[n-1])
}

// keywordLevel maps the keyword matcher's scenario to the same 0..2 scale as the labels.
func keywordLevel(text string) int {
	switch detectKeywordScenario(userMsg(text), 50).Scenario {
	case ScenarioComplex, ScenarioThink:
		return 2
	case ScenarioBackground:
		return 0
	default:
		return 1
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
