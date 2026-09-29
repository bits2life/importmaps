package importmap

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// countingFS counts how often importmap.json is opened, i.e. how often the
// import map is generated.
type countingFS struct {
	fstest.MapFS
	opens int
}

func (c *countingFS) Open(name string) (fs.File, error) {
	if name == "importmap.json" {
		c.opens++
	}
	return c.MapFS.Open(name)
}

func (c *countingFS) ReadFile(name string) ([]byte, error) {
	if name == "importmap.json" {
		c.opens++
	}
	return c.MapFS.ReadFile(name)
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"importmap.json": {Data: []byte(`{"imports":{"app":"/app.js"}}`)},
		"app.js":         {Data: []byte(`export {}`)},
	}
}

func TestCacheCaches(t *testing.T) {
	fsys := &countingFS{MapFS: testFS()}
	cache := New(FromFS(fsys, "importmap.json"))

	for i := 0; i < 3; i++ {
		if _, _, err := cache.Get(); err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}
	if fsys.opens != 1 {
		t.Fatalf("generated %d times, want 1", fsys.opens)
	}

	cache.Invalidate()
	cache.Get()
	if fsys.opens != 2 {
		t.Fatalf("generated %d times after Invalidate, want 2", fsys.opens)
	}
}

func TestCacheDisableCache(t *testing.T) {
	fsys := &countingFS{MapFS: testFS()}
	opts := FromFS(fsys, "importmap.json")
	opts.DisableCache = true
	cache := New(opts)

	cache.Get()
	cache.Get()
	if fsys.opens != 2 {
		t.Fatalf("generated %d times with cache disabled, want 2", fsys.opens)
	}
}

type errFS struct{}

func (errFS) Open(string) (fs.File, error) { return nil, errors.New("boom") }

func TestCacheGenerateError(t *testing.T) {
	cache := New(Options{
		Sources: []Source{{FS: errFS{}, Prefix: "/"}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if _, _, err := cache.Get(); err == nil {
		t.Fatal("Get() error = nil, want error")
	}
	if got := cache.InlineScript(); got != `<script type="importmap">{"imports":{}}</script>` {
		t.Errorf("InlineScript() = %s", got)
	}
	if got := cache.Script(); got != `<script src="/importmap.js"></script>` {
		t.Errorf("Script() = %s", got)
	}
	rec := httptest.NewRecorder()
	cache.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/importmap.js", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("ServeHTTP status = %d, want 500", rec.Code)
	}
}

func TestCacheTags(t *testing.T) {
	opts := FromFS(testFS(), "importmap.json")
	opts.ScriptPath = "/assets/importmap.js"
	cache := New(opts)
	content, checksum, _ := cache.Get()

	if got, want := string(cache.InlineScript()), `<script type="importmap">`+content+`</script>`; got != want {
		t.Errorf("InlineScript() = %s, want %s", got, want)
	}
	if got, want := string(cache.Script()), `<script src="/assets/importmap.js?checksum=`+checksum+`"></script>`; got != want {
		t.Errorf("Script() = %s, want %s", got, want)
	}
}

func TestCacheServeHTTP(t *testing.T) {
	cache := New(FromFS(testFS(), "importmap.json"))
	_, checksum, _ := cache.Get()
	etag := `"sha256-` + checksum + `"`

	tests := []struct {
		name        string
		target      string
		ifNoneMatch string
		wantStatus  int
		wantCache   string
	}{
		{"versioned", "/importmap.js?checksum=" + checksum, "", http.StatusOK, "public, max-age=31536000, immutable"},
		{"unversioned", "/importmap.js", "", http.StatusOK, "no-cache"},
		{"stale checksum", "/importmap.js?checksum=old", "", http.StatusOK, "no-cache"},
		{"not modified", "/importmap.js?checksum=" + checksum, etag, http.StatusNotModified, "public, max-age=31536000, immutable"},
		{"weak etag in list", "/importmap.js", `"other", W/` + etag, http.StatusNotModified, "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			if tt.ifNoneMatch != "" {
				req.Header.Set("If-None-Match", tt.ifNoneMatch)
			}
			rec := httptest.NewRecorder()
			cache.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
			if got := rec.Header().Get("ETag"); got != etag {
				t.Errorf("ETag = %q, want %q", got, etag)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
				t.Errorf("Content-Type = %q", got)
			}
			if tt.wantStatus == http.StatusOK && !strings.Contains(rec.Body.String(), "document.write(") {
				t.Errorf("body is not a loader script: %q", rec.Body.String())
			}
		})
	}
}
