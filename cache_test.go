package importmap

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

func TestCacheRefresh(t *testing.T) {
	fsys := &countingFS{MapFS: fstest.MapFS{
		"importmap.json": {Data: []byte(`{"imports":{"app":"/js/app.js"},"scopes":{"/legacy/":{"app":"/js/app.js"}}}`)},
		"js/app.js":      {Data: []byte(`export const v = 1`)},
		"js/other.js":    {Data: []byte(`export {}`)},
		"js/gone.js":     {Data: []byte(`export {}`)},
	}}
	widget := []byte(`export const w = 1`)
	opts := FromFS(fsys, "importmap.json")
	opts.Scripts = []Script{{Path: "/plugin/widget.js", Content: func() ([]byte, error) { return widget, nil }}}
	opts.Aliases = []Alias{{Specifier: "widget", Target: "/plugin/widget.js"}}
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cache := New(opts)

	if err := cache.Refresh("/js/app.js"); err != nil {
		t.Fatalf("Refresh() before first use error = %v", err)
	}
	if fsys.opens != 0 {
		t.Fatalf("Refresh() before first use generated the import map")
	}

	before := mustMap(t, cache)

	fsys.MapFS["js/app.js"] = &fstest.MapFile{Data: []byte(`export const v = 2`)}
	fsys.MapFS["js/new.js"] = &fstest.MapFile{Data: []byte(`export {}`)}
	delete(fsys.MapFS, "js/gone.js")
	widget = []byte(`export const w = 2`)

	if got := mustMap(t, cache); got.Imports["app"] != before.Imports["app"] {
		t.Fatal("cached import map changed before Refresh")
	}

	if err := cache.Refresh("/js/app.js?checksum=old", "js/new.js", "/js/gone.js", "/plugin/widget.js"); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	after := mustMap(t, cache)

	if fsys.opens != 1 {
		t.Errorf("Refresh() rebuilt the import map (%d generations)", fsys.opens)
	}
	for _, k := range []string{"app", "/js/app.js", "widget", "/plugin/widget.js"} {
		if after.Imports[k] == before.Imports[k] {
			t.Errorf("%s not refreshed: %q", k, after.Imports[k])
		}
	}
	if after.Imports["app"] != after.Imports["/js/app.js"] || after.Scopes["/legacy/"]["app"] != after.Imports["/js/app.js"] {
		t.Errorf("targets of /js/app.js disagree: %v", after)
	}
	if after.Imports["/js/other.js"] != before.Imports["/js/other.js"] {
		t.Errorf("unrelated script changed: %q", after.Imports["/js/other.js"])
	}
	if got := after.Imports["/js/new.js"]; !strings.HasPrefix(got, "/js/new.js?checksum=") {
		t.Errorf("new script not added: %q", got)
	}
	if _, ok := after.Imports["/js/gone.js"]; ok {
		t.Error("removed script still in import map")
	}

	_, checksum, _ := cache.Get()
	if got := string(cache.Script()); !strings.Contains(got, checksum) {
		t.Errorf("Script() not updated to new checksum: %s", got)
	}

	prefixed := New(Options{
		Sources: []Source{{FS: testFS(), Prefix: "/assets"}},
		Logger:  opts.Logger,
	})
	prefixed.Get()
	if err := prefixed.Refresh("/other/x.js"); err == nil {
		t.Error("Refresh() of a path outside any source: error = nil")
	}
}

func mustMap(t *testing.T, cache *Cache) Map {
	t.Helper()
	content, _, err := cache.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	var m Map
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return m
}

func TestCacheSetScript(t *testing.T) {
	cache := New(Options{
		Scripts: []Script{{Path: "/plugin/a.js", Version: "1"}},
		Aliases: []Alias{{Specifier: "a", Target: "/plugin/a.js"}, {Specifier: "b", Target: "/plugin/b.js"}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// Before first use, the script is simply registered.
	if err := cache.SetScript(Script{Path: "plugin/b.js", Version: "1"}); err != nil {
		t.Fatalf("SetScript() error = %v", err)
	}
	before := mustMap(t, cache)
	if !strings.HasPrefix(before.Imports["b"], "/plugin/b.js?checksum=") {
		t.Fatalf("script set before first use missing: %v", before.Imports)
	}

	if err := cache.SetScript(Script{Path: "/plugin/a.js", Version: "2"}); err != nil {
		t.Fatalf("SetScript() error = %v", err)
	}
	if err := cache.SetScript(Script{Path: "/plugin/c.js", Content: func() ([]byte, error) { return []byte("c"), nil }}); err != nil {
		t.Fatalf("SetScript() error = %v", err)
	}
	after := mustMap(t, cache)
	if after.Imports["a"] == before.Imports["a"] || after.Imports["/plugin/a.js"] != after.Imports["a"] {
		t.Errorf("new version not applied: %v", after.Imports)
	}
	if after.Imports["b"] != before.Imports["b"] {
		t.Errorf("unrelated script changed: %q", after.Imports["b"])
	}
	if !strings.HasPrefix(after.Imports["/plugin/c.js"], "/plugin/c.js?checksum=") {
		t.Errorf("script added after first use missing: %v", after.Imports)
	}

	cache.Invalidate()
	if rebuilt := mustMap(t, cache); rebuilt.Imports["a"] != after.Imports["a"] || rebuilt.Imports["/plugin/c.js"] != after.Imports["/plugin/c.js"] {
		t.Errorf("scripts set with SetScript lost after Invalidate: %v", rebuilt.Imports)
	}
}

func TestCacheConcurrentUpdates(t *testing.T) {
	cache := New(Options{
		Scripts: []Script{{Path: "/a.js", Version: "0"}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				cache.Get()
				cache.SetScript(Script{Path: "/a.js", Version: strconv.Itoa(i*100 + j)})
				cache.Refresh("/a.js")
				if j%10 == 0 {
					cache.Invalidate()
				}
			}
		}(i)
	}
	wg.Wait()
}
