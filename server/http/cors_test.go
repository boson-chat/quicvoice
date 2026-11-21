package http_test

import (
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	qvhttp "github.com/boson-chat/quicvoice/server/http"
)

func corsReq(t *testing.T, h nethttp.Handler, method, path, origin string, preflight bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if preflight {
		req.Header.Set("Access-Control-Request-Method", "PATCH")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCORSAllowList(t *testing.T) {
	reached := 0
	h := qvhttp.CORS("https://chat.example.test, https://b.test/", nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		reached++
		w.WriteHeader(nethttp.StatusTeapot)
	}))

	// An allowed page's preflight: answered here, never reaches the API.
	rec := corsReq(t, h, nethttp.MethodOptions, "/v1/rooms/x", "https://chat.example.test", true)
	if rec.Code != nethttp.StatusNoContent || reached != 0 {
		t.Fatalf("preflight: %d, reached %d", rec.Code, reached)
	}
	hd := rec.Header()
	if hd.Get("Access-Control-Allow-Origin") != "https://chat.example.test" ||
		hd.Get("Access-Control-Allow-Methods") != "GET, POST, PATCH, DELETE" ||
		hd.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" ||
		hd.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight headers %v", hd)
	}

	// The real request gets the origin back.
	rec = corsReq(t, h, nethttp.MethodGet, "/v1/rooms", "https://b.test", false)
	if rec.Code != nethttp.StatusTeapot || rec.Header().Get("Access-Control-Allow-Origin") != "https://b.test" {
		t.Fatalf("get: %d %v", rec.Code, rec.Header())
	}

	// Another page: no CORS headers (the browser blocks it), preflight
	// still short-circuits.
	rec = corsReq(t, h, nethttp.MethodOptions, "/v1/rooms", "https://evil.test", true)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Methods") != "" {
		t.Fatalf("evil preflight allowed: %v", rec.Header())
	}
	rec = corsReq(t, h, nethttp.MethodGet, "/v1/rooms", "https://evil.test", false)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("evil get allowed: %v", rec.Header())
	}

	// Native clients send no Origin: untouched.
	rec = corsReq(t, h, nethttp.MethodGet, "/v1/rooms", "", false)
	if rec.Code != nethttp.StatusTeapot || rec.Header().Get("Vary") != "" {
		t.Fatalf("no-origin: %d %v", rec.Code, rec.Header())
	}
}

func TestCORSAny(t *testing.T) {
	for _, list := range []string{"", "*"} {
		h := qvhttp.CORS(list, nethttp.NotFoundHandler())
		rec := corsReq(t, h, nethttp.MethodGet, "/v1/rooms", "https://anything.test", false)
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%q: %v", list, rec.Header())
		}
	}
}

func TestInfoEndpoint(t *testing.T) {
	h := qvhttp.WithInfo(qvhttp.PublicOptions{QUICPort: 7700, DevCertHash: "abcd"}, nethttp.NotFoundHandler())
	rec := corsReq(t, h, nethttp.MethodGet, "/v1/info", "", false)
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["quic_port"] != float64(7700) || info["webtransport_path"] != "/wt" || info["dev_cert_hash"] != "abcd" {
		t.Fatalf("info %v", info)
	}
	// A real certificate: no hash.
	h = qvhttp.WithInfo(qvhttp.PublicOptions{QUICPort: 7700}, nethttp.NotFoundHandler())
	rec = corsReq(t, h, nethttp.MethodGet, "/v1/info", "", false)
	info = map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if _, ok := info["dev_cert_hash"]; ok {
		t.Fatalf("hash with a real cert: %v", info)
	}
	// Everything else still reaches the API.
	if rec := corsReq(t, h, nethttp.MethodGet, "/v1/rooms", "", false); rec.Code != nethttp.StatusNotFound {
		t.Fatalf("passthrough %d", rec.Code)
	}
}
