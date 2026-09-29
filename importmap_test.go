package importmap

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestGenerateImportMap(t *testing.T) {
	publicFS := fstest.MapFS{
		"importmap.json": {
			Data: []byte(`{
				"imports": {
					"@test/module": "/js/test-module.js"
				}
			}`),
		},
		"js/test-module.js": {
			Data: []byte(`console.log("test module");`),
		},
		"js/other-script.js": {
			Data: []byte(`console.log("other script");`),
		},
	}

	importMap, _, err := GenerateImportMap(publicFS, "importmap.json")
	if err != nil {
		t.Fatalf("Failed to generate import map: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(importMap), &result); err != nil {
		t.Fatalf("Failed to parse import map: %v", err)
	}

	imports, ok := result["imports"].(map[string]interface{})
	if !ok {
		t.Fatal("Import map missing 'imports' field")
	}

	if path, ok := imports["@test/module"].(string); !ok {
		t.Error("Missing explicit module mapping")
	} else if !strings.HasPrefix(path, "/js/test-module.js") {
		t.Errorf("Module path should be web-root relative, got: %s", path)
	}

	// Check that test-module.js is present as a key
	if _, ok := imports["/js/test-module.js"]; !ok {
		t.Error("Missing test-module.js as a key")
	}

	// Check that other-script.js is present with correct path
	if _, ok := imports["/js/other-script.js"]; !ok {
		t.Error("Missing other-script.js mapping")
	}

	// Verify that all paths include checksums of at least 12 characters
	for _, path := range imports {
		if str, ok := path.(string); ok {
			parts := strings.Split(str, "?checksum=")
			if len(parts) != 2 {
				t.Errorf("Path %s does not contain a checksum", str)
			}
			if len(parts[1]) < 12 {
				t.Errorf("Checksum too short in path %s", str)
			}
		}
	}
}

func TestGenerateImportMapPreservesExternalImports(t *testing.T) {
	publicFS := fstest.MapFS{
		"importmap.json": {
			Data: []byte(`{"imports":{"/js/app.js":"/js/app.js","preact":"https://esm.sh/preact"}}`),
		},
		"js/app.js": {
			Data: []byte(`console.log("app")`),
		},
		"js/lib/util.js": {
			Data: []byte(`export const ok = true`),
			Mode: fs.ModePerm,
		},
	}

	content, checksum, err := GenerateImportMap(publicFS, "importmap.json")
	if err != nil {
		t.Fatalf("GenerateImportMap() error = %v", err)
	}
	if checksum == "" {
		t.Fatal("expected checksum")
	}

	var importMap ImportMap
	if err := json.Unmarshal([]byte(content), &importMap); err != nil {
		t.Fatalf("generated invalid JSON: %v", err)
	}

	if got := importMap.Imports["preact"]; got != "https://esm.sh/preact" {
		t.Fatalf("external import changed: %q", got)
	}
	if got := importMap.Imports["/js/app.js"]; got == "/js/app.js" || got == "" {
		t.Fatalf("expected checksum on /js/app.js, got %q", got)
	}
	if got := importMap.Imports["/js/lib/util.js"]; got == "" {
		t.Fatal("expected recursive JS file in import map")
	}
}

func TestGenerateImportMapFromSourcesChecksumsMountedFiles(t *testing.T) {
	appFS := fstest.MapFS{
		"importmap.json": {
			Data: []byte(`{"imports":{"admin":"/ceyebr/admin/index.js","core":"/main.js"}}`),
		},
		"main.js": {
			Data: []byte(`import "admin"`),
		},
	}
	adminFS := fstest.MapFS{
		"admin/index.js": {
			Data: []byte(`import { cmsSession } from "admin/cms-session"`),
		},
		"admin/cms-session.js": {
			Data: []byte(`export const cmsSession = {}`),
		},
	}

	content, _, err := GenerateImportMapFromSources(
		[]ImportmapFile{{FS: appFS, Path: "importmap.json"}},
		[]FileSource{
			{FS: appFS, Prefix: "/"},
			{FS: adminFS, Prefix: "/ceyebr"},
		},
	)
	if err != nil {
		t.Fatalf("GenerateImportMapFromSources() error = %v", err)
	}

	var importMap ImportMap
	if err := json.Unmarshal([]byte(content), &importMap); err != nil {
		t.Fatalf("generated invalid JSON: %v", err)
	}

	if got := importMap.Imports["admin"]; got == "" || !strings.HasPrefix(got, "/ceyebr/admin/index.js?checksum=") {
		t.Fatalf("admin import was not checksummed from mounted source: %q", got)
	}
	if got := importMap.Imports["/ceyebr/admin/cms-session.js"]; got == "" || !strings.HasPrefix(got, "/ceyebr/admin/cms-session.js?checksum=") {
		t.Fatalf("mounted cms-session file missing from import map: %q", got)
	}
}

func TestGenerateImportMapFromSourcesMergesImportmapFiles(t *testing.T) {
	baseFS := fstest.MapFS{
		"importmap.json": {
			Data: []byte(`{"imports":{"admin/cms-session":"/ceyebr/admin/cms-session.js","core":"/old-main.js"}}`),
		},
		"old-main.js": {
			Data: []byte(`console.log("old")`),
		},
	}
	previewFS := fstest.MapFS{
		"importmap.json": {
			Data: []byte(`{"imports":{"core":"/main.js"}}`),
		},
		"main.js": {
			Data: []byte(`console.log("preview")`),
		},
	}
	adminFS := fstest.MapFS{
		"admin/cms-session.js": {
			Data: []byte(`export const cmsSession = {}`),
		},
	}

	content, _, err := GenerateImportMapFromSources(
		[]ImportmapFile{
			{FS: baseFS, Path: "importmap.json"},
			{FS: previewFS, Path: "importmap.json"},
		},
		[]FileSource{
			{FS: previewFS, Prefix: "/"},
			{FS: adminFS, Prefix: "/ceyebr"},
		},
	)
	if err != nil {
		t.Fatalf("GenerateImportMapFromSources() error = %v", err)
	}

	var importMap ImportMap
	if err := json.Unmarshal([]byte(content), &importMap); err != nil {
		t.Fatalf("generated invalid JSON: %v", err)
	}

	if got := importMap.Imports["admin/cms-session"]; got == "" || !strings.HasPrefix(got, "/ceyebr/admin/cms-session.js?checksum=") {
		t.Fatalf("base import missing after merge: %q", got)
	}
	if got := importMap.Imports["core"]; got == "" || !strings.HasPrefix(got, "/main.js?checksum=") {
		t.Fatalf("later importmap did not override core mapping: %q", got)
	}
}

func TestGenerateImportMapFromSourcesChecksumsAliases(t *testing.T) {
	adminFS := fstest.MapFS{
		"admin/index.js": {
			Data: []byte(`export const ready = true`),
		},
	}

	content, _, err := GenerateImportMapFromSources(
		nil,
		[]FileSource{{FS: adminFS, Prefix: "/_admin/assets"}},
		Alias{Specifier: "@archetype/admin", Target: "/_admin/assets/admin/index.js"},
		Alias{Specifier: "@archetype/admin/", Target: "/_admin/assets/admin/"},
		Alias{Specifier: "preact", Target: "https://esm.sh/preact"},
	)
	if err != nil {
		t.Fatalf("GenerateImportMapFromSources() error = %v", err)
	}

	var importMap ImportMap
	if err := json.Unmarshal([]byte(content), &importMap); err != nil {
		t.Fatalf("generated invalid JSON: %v", err)
	}

	if got := importMap.Imports["@archetype/admin"]; got == "" || !strings.HasPrefix(got, "/_admin/assets/admin/index.js?checksum=") {
		t.Fatalf("alias target was not checksummed: %q", got)
	}
	if got := importMap.Imports["@archetype/admin/"]; got != "/_admin/assets/admin/" {
		t.Fatalf("prefix alias should stay unchecksummed, got %q", got)
	}
	if got := importMap.Imports["preact"]; got != "https://esm.sh/preact" {
		t.Fatalf("external alias changed: %q", got)
	}
}

func TestGenerateImportMapHandlesSpecialTargets(t *testing.T) {
	publicFS := fstest.MapFS{
		"importmap.json": {
			Data: []byte(`{
				"imports": {
					"versioned": "/js/app.js?v=2",
					"cdn": "//cdn.example.com/lib.js"
				},
				"scopes": {
					"/legacy/": {"app": "/js/app.js"}
				},
				"integrity": {
					"https://cdn.example.com/lib.js": "sha384-abc"
				}
			}`),
		},
		"js/app.js":     {Data: []byte(`export {}`)},
		"js/module.mjs": {Data: []byte(`export {}`)},
	}

	content, _, err := GenerateImportMap(publicFS, "importmap.json")
	if err != nil {
		t.Fatalf("GenerateImportMap() error = %v", err)
	}

	var importMap ImportMap
	if err := json.Unmarshal([]byte(content), &importMap); err != nil {
		t.Fatalf("generated invalid JSON: %v", err)
	}

	if got := importMap.Imports["versioned"]; !strings.HasPrefix(got, "/js/app.js?v=2&checksum=") {
		t.Errorf("target with query not checksummed correctly: %q", got)
	}
	if got := importMap.Imports["cdn"]; got != "//cdn.example.com/lib.js" {
		t.Errorf("protocol-relative target changed: %q", got)
	}
	if got := importMap.Imports["/js/module.mjs"]; !strings.HasPrefix(got, "/js/module.mjs?checksum=") {
		t.Errorf(".mjs file missing from import map: %q", got)
	}
	if got := importMap.Scopes["/legacy/"]["app"]; !strings.HasPrefix(got, "/js/app.js?checksum=") {
		t.Errorf("scoped target not preserved and checksummed: %q", got)
	}
	if got := importMap.Integrity["https://cdn.example.com/lib.js"]; got != "sha384-abc" {
		t.Errorf("integrity not preserved: %q", got)
	}
}

func TestImportmapCache(t *testing.T) {
	calls := 0
	cache := NewImportmapCache(func() (string, string, error) {
		calls++
		return `{"imports":{}}`, "abc", nil
	}, false)

	for i := 0; i < 3; i++ {
		if _, checksum, ok := cache.Get(); !ok || checksum != "abc" {
			t.Fatalf("Get() = %q, %v", checksum, ok)
		}
	}
	if calls != 1 {
		t.Fatalf("generator called %d times, want 1", calls)
	}

	cache.Invalidate()
	cache.Get()
	if calls != 2 {
		t.Fatalf("generator called %d times after Invalidate, want 2", calls)
	}

	cache.DisableCache = true
	cache.Get()
	cache.Get()
	if calls != 4 {
		t.Fatalf("generator called %d times with cache disabled, want 4", calls)
	}
}

func TestImportmapCacheGeneratorError(t *testing.T) {
	cache := NewImportmapCache(func() (string, string, error) {
		return "", "", errors.New("boom")
	}, false)

	if _, _, ok := cache.Get(); ok {
		t.Fatal("Get() ok = true, want false")
	}
	if got := cache.GetInlineScriptTag(); got != `<script type="importmap">{"imports":{}}</script>` {
		t.Fatalf("unexpected fallback tag: %s", got)
	}
}

func TestHTTPHandler(t *testing.T) {
	cache := NewCacheFromFS(fstest.MapFS{
		"importmap.json": {Data: []byte(`{"imports":{}}`)},
		"app.js":         {Data: []byte(`export {}`)},
	}, "importmap.json", false)
	content, checksum, _ := cache.Get()
	handler := HTTPHandler(cache)
	etag := `"sha256-` + checksum + `"`

	tests := []struct {
		name        string
		target      string
		ifNoneMatch string
		wantStatus  int
		wantCache   string
	}{
		{"versioned", "/importmap.json?checksum=" + checksum, "", http.StatusOK, "public, max-age=31536000, immutable"},
		{"unversioned", "/importmap.json", "", http.StatusOK, "no-cache"},
		{"stale checksum", "/importmap.json?checksum=old", "", http.StatusOK, "no-cache"},
		{"not modified", "/importmap.json?checksum=" + checksum, etag, http.StatusNotModified, "public, max-age=31536000, immutable"},
		{"weak etag in list", "/importmap.json", `"other", W/` + etag, http.StatusNotModified, "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			if tt.ifNoneMatch != "" {
				req.Header.Set("If-None-Match", tt.ifNoneMatch)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
			if got := rec.Header().Get("ETag"); got != etag {
				t.Errorf("ETag = %q, want %q", got, etag)
			}
			if tt.wantStatus == http.StatusOK && rec.Body.String() != content {
				t.Errorf("body = %q, want %q", rec.Body.String(), content)
			}
		})
	}
}
