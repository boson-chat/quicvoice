package http

import (
	nethttp "net/http"

	"github.com/boson-chat/quicvoice/server/internal/origins"
)

// CORS lets pages on the allowed origins (comma-separated; "*" = any) call
// the public API from a browser: GET/POST/PATCH/DELETE with an
// Authorization bearer token. Credentials (cookies) are never allowed;
// the API doesn't use them. Preflights are answered here and never reach
// the handlers.
func CORS(allowed string, next nethttp.Handler) nethttp.Handler {
	return CORSPolicy(origins.MustParse(allowed, nil), next)
}

// CORSPolicy is CORS for a parsed list (which may follow the directory's
// web origins, see package origins). "*" when it allows any origin, else
// the request's origin when allowed.
func CORSPolicy(p *origins.Policy, next nethttp.Handler) nethttp.Handler {
	any := p.Any()
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		origin := r.Header.Get("Origin")
		ok := origin != "" && p.Allowed(origin)
		h := w.Header()
		if origin != "" {
			h.Add("Vary", "Origin")
		}
		if ok {
			if any {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			h.Set("Access-Control-Expose-Headers", "Retry-After")
		}
		if r.Method == nethttp.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if ok {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				h.Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(nethttp.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
