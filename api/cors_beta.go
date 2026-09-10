package api

import (
	"net/http"
	"strings"
)

// withCors wraps the api router so browsers running the dashboard against the
// beta deployment can call it cross-origin.
//
// Beta-only. Upstream serves the dashboard and the api from the same origin, so
// it needs none of this; here they are separate hosts and without these headers
// every dashboard call fails the browser's preflight before it is ever sent.
//
// It lives in its own file, rather than inline in run.go, so that the fork
// carries a single added file plus a one-line wrap instead of a divergent copy
// of upstream's runner -- which is what made this conflict on every merge.
func withCors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// treat empty origin as a same-site/non-browser request
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Requested-With")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if strings.EqualFold(r.Method, http.MethodOptions) {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
