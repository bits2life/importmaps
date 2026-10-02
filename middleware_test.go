package importmap

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func newMiddlewareTest(t *testing.T, disableCache bool) (*Cache, fstest.MapFS, http.Handler) {
	t.Helper()
	fsys := fstest.MapFS{
		"importmap.json": {Data: []byte(`{"imports":{"app":"/js/app.js"}}`)},
		"js/app.js":      {Data: []byte(`export const v = 1`)},
		"style.css":      {Data: []byte(`body {}`)},
	}
	opts := FromFS(fsys, "importmap.json")
	opts.DisableCache = disableCache
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cache := New(opts)
	return cache, fsys, cache.Middleware(http.FileServerFS(fsys))
}

func serve(h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware(t *testing.T) {
	cache, _, h := newMiddlewareTest(t, false)
	url, ok := cache.URL("/js/app.js")
	if !ok || !strings.HasPrefix(url, "/js/app.js?checksum=") {
		t.Fatalf("URL() = %q, %v", url, ok)
	}

	versioned := serve(h, http.MethodGet, url)
	if versioned.Code != http.StatusOK || versioned.Body.String() != "export const v = 1" {
		t.Fatalf("versioned request: %d %q", versioned.Code, versioned.Body.String())
	}
	if got := versioned.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("versioned Cache-Control = %q", got)
	}
	etag := versioned.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"sha256-`) {
		t.Errorf("ETag = %q", etag)
	}

	for _, target := range []string{"/js/app.js", "/js/app.js?checksum=stale"} {
		rec := serve(h, http.MethodGet, target)
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", target, got)
		}
		if rec.Header().Get("ETag") != etag {
			t.Errorf("%s ETag = %q, want %q", target, rec.Header().Get("ETag"), etag)
		}
	}

	if rec := serve(h, http.MethodGet, "/js/app.js", "If-None-Match", etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("conditional request: %d %q", rec.Code, rec.Body.String())
	}

	if rec := serve(h, http.MethodGet, "/style.css"); rec.Header().Get("Cache-Control") != "" || rec.Header().Get("ETag") != "" {
		t.Errorf("unknown file got caching headers: %v", rec.Header())
	}

	loader := serve(h, http.MethodGet, "/importmap.js")
	if !strings.Contains(loader.Body.String(), "document.write(") {
		t.Errorf("loader not served at ScriptPath: %q", loader.Body.String())
	}
}

func TestMiddlewareDisableCache(t *testing.T) {
	_, _, h := newMiddlewareTest(t, true)
	rec := serve(h, http.MethodGet, "/js/app.js?checksum=anything")
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRefreshHandler(t *testing.T) {
	cache, fsys, h := newMiddlewareTest(t, false)
	refresh := cache.RefreshHandler()
	oldURL, _ := cache.URL("/js/app.js")

	fsys["js/app.js"] = &fstest.MapFile{Data: []byte(`export const v = 2`)}

	// The cached checksum still describes the old content until refreshed.
	if rec := serve(h, http.MethodGet, oldURL); rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("checksum changed before refresh")
	}

	post := func(body string) (*httptest.ResponseRecorder, RefreshResponse) {
		req := httptest.NewRequest(http.MethodPost, "/_importmap/refresh", strings.NewReader(body))
		rec := httptest.NewRecorder()
		refresh.ServeHTTP(rec, req)
		var resp RefreshResponse
		if rec.Code != http.StatusBadRequest {
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("invalid response %q: %v", rec.Body.String(), err)
			}
		}
		return rec, resp
	}

	rec, resp := post(`{"paths":["/js/app.js","/js/missing.js"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	newURL := resp.URLs["/js/app.js"]
	if newURL == oldURL || !strings.HasPrefix(newURL, "/js/app.js?checksum=") {
		t.Errorf("URL not refreshed: %q (was %q)", newURL, oldURL)
	}
	if got, ok := resp.URLs["/js/missing.js"]; !ok || got != "" {
		t.Errorf("missing script URL = %q, %v; want empty", got, ok)
	}
	if _, checksum, _ := cache.Get(); resp.Checksum != checksum {
		t.Errorf("Checksum = %q, want %q", resp.Checksum, checksum)
	}
	if got, _ := cache.URL("/js/app.js"); got != newURL {
		t.Errorf("cache URL = %q, want %q", got, newURL)
	}
	if rec := serve(h, http.MethodGet, oldURL); rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("stale URL still immutable after refresh")
	}

	if rec, _ := post(`not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid body status = %d", rec.Code)
	}
	if rec := serve(refresh, http.MethodGet, "/_importmap/refresh"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d", rec.Code)
	}

	prefixed := New(Options{
		Sources: []Source{{FS: fsys, Prefix: "/assets"}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	prefixed.Get()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"paths":["/elsewhere/x.js"]}`))
	rec = httptest.NewRecorder()
	prefixed.RefreshHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("path outside sources: %d %q", rec.Code, rec.Body.String())
	}
}
