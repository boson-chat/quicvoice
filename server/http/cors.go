package http

import (
	nethttp "net/http"
	"strings"
)

// CORS lets pages on the allowed origins (comma-separated; "*" = any) call
// the public API from a browser: GET/POST/PATCH/DELETE with an
// Authorization bearer token. Credentials (cookies) are never allowed;
// the API doesn't use them. Preflights are answered here and never reach
// the handlers.
func CORS(allowed string, next nethttp.Handler) nethttp.Handler {
	any := false
	set := map[string]bool{}
	for _, o := range strings.Split(allowed, ",") {
		o = strings.TrimSuffix(strings.TrimSpace(o), "/")
		switch o {
		case "":
		case "*":
			any = true
		default:
			set[strings.ToLower(o)] = true
		}
	}
	if strings.TrimSpace(allowed) == "" {
		any = true
	}
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		origin := r.Header.Get("Origin")
		ok := origin != "" && (any || set[strings.ToLower(origin)])
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
