package gui

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// requestGuardMiddleware rejects requests that did not come from the dashboard
// itself.
//
// The GUI binds loopback, but that alone stops neither cross-site requests from
// the user's browser nor DNS rebinding (an attacker hostname that resolves to
// 127.0.0.1 after the page loads). Two checks close both holes:
//
//   - Host must name a loopback host at the GUI's own port. A rebound request
//     carries the attacker's hostname in Host, so it is refused before routing
//     and before any handler can read a secret or the log.
//   - Requests that mutate state must carry an Origin that matches that same
//     loopback origin. Browsers send Origin on non-GET requests and a page on
//     another origin cannot forge it, so this blocks cross-site writes even if
//     one were made as a "simple" request without a preflight.
//
// Non-browser clients calling the API must send Origin for unsafe methods too.
func requestGuardMiddleware(port int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHostPort(r.Host, port) {
			http.Error(w, "request rejected: host is not the dashboard", http.StatusForbidden)
			return
		}
		if isMutating(r.Method) && strings.HasPrefix(r.URL.Path, "/api/") && !isSameOrigin(r, port) {
			http.Error(w, "request rejected: cross-origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// isSameOrigin reports whether the request's Origin names the dashboard. A
// missing Origin is untrusted: browsers always send it on unsafe methods, so its
// absence means a non-browser client, not the dashboard.
func isSameOrigin(r *http.Request, port int) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isLoopbackHostPort(u.Host, port)
}

// isLoopbackHostPort reports whether hostport names a loopback host at port.
func isLoopbackHostPort(hostport string, port int) bool {
	host, p, err := net.SplitHostPort(hostport)
	if err != nil || p != strconv.Itoa(port) {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
