package gui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"

	"github.com/routatic/proxy/internal/config"
)

// envRefPattern matches the ${VAR} placeholders the loader interpolates.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// decodeJSONMap decodes JSON without going through float64, so numbers the
// config file wrote are re-encoded exactly as they were.
func decodeJSONMap(data []byte, dst *map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(dst)
}

// restoreEnvRefs puts the ${VAR} references back where the patch did not reach.
//
// The dashboard form is filled from the live config, which the loader has already
// interpolated, so saving any field would otherwise write every resolved secret
// into config.json. raw is the file as stored, merged is the post-patch config,
// and patch holds the keys the client actually sent.
func restoreEnvRefs(raw, merged, patch map[string]any) {
	for k, rv := range raw {
		pv, inPatch := patch[k]
		switch r := rv.(type) {
		case string:
			if !inPatch && envRefPattern.MatchString(r) {
				merged[k] = r
			}
		case []any:
			if !inPatch && containsEnvRef(r) {
				merged[k] = r
			}
		case map[string]any:
			mm, ok := merged[k].(map[string]any)
			if !ok {
				continue
			}
			sub, _ := pv.(map[string]any)
			restoreEnvRefs(r, mm, sub)
		}
	}
}

func containsEnvRef(list []any) bool {
	for _, v := range list {
		if s, ok := v.(string); ok && envRefPattern.MatchString(s) {
			return true
		}
	}
	return false
}

// handleConfigEnv reports which env vars the stored config references and
// whether each is currently non-empty. Values are never returned — only the
// variable names and their presence.
func (s *Server) handleConfigEnv(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.atomicCfg == nil {
		http.Error(w, "config not available", http.StatusServiceUnavailable)
		return
	}

	data, err := os.ReadFile(s.atomicCfg.Path())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var raw map[string]any
	if err := decodeJSONMap(data, &raw); err != nil {
		http.Error(w, fmt.Sprintf("invalid config file: %v", err), http.StatusInternalServerError)
		return
	}

	refs := map[string][]string{}
	collectEnvRefs("", raw, refs)
	// jev_routing.api_key_env holds a bare variable name rather than a ${VAR},
	// because it names the variable the classifier reads at call time.
	if name := s.atomicCfg.Get().JevRouting.APIKeyEnv; name != "" {
		refs["jev_routing.api_key_env"] = []string{name}
	}

	env := map[string]bool{}
	for _, names := range refs {
		for _, name := range names {
			if _, seen := env[name]; !seen {
				env[name] = os.Getenv(name) != ""
			}
		}
	}

	writeJSON(w, map[string]any{"refs": refs, "env": env})
}

func collectEnvRefs(path string, v any, out map[string][]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			collectEnvRefs(childPath, child, out)
		}
	case []any:
		for _, child := range t {
			collectEnvRefs(path, child, out)
		}
	case string:
		for _, m := range envRefPattern.FindAllStringSubmatch(t, -1) {
			out[path] = appendUnique(out[path], m[1])
		}
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// mergeConfigPatch applies patch onto the config stored at path and returns the
// JSON to write, with untouched ${VAR} references left in place.
func mergeConfigPatch(path string, patchBytes []byte) ([]byte, *config.Config, error) {
	rawFile, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	current, err := config.LoadFromPath(path)
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(patchBytes, current); err != nil {
		return nil, nil, fmt.Errorf("failed to apply patch: %w", err)
	}

	mergedBytes, err := json.Marshal(current)
	if err != nil {
		return nil, nil, err
	}
	var raw, merged, patch map[string]any
	if err := decodeJSONMap(rawFile, &raw); err != nil {
		return nil, nil, fmt.Errorf("invalid config file: %w", err)
	}
	if err := decodeJSONMap(mergedBytes, &merged); err != nil {
		return nil, nil, err
	}
	if err := decodeJSONMap(patchBytes, &patch); err != nil {
		return nil, nil, err
	}
	restoreEnvRefs(raw, merged, patch)

	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return out, current, nil
}
