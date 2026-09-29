package importmap

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
)

func TestGenerate(t *testing.T) {
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

	importMap, _, err := Generate(FromFS(publicFS, "importmap.json"))
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

func TestGeneratePreservesExternalImports(t *testing.T) {
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

	content, checksum, err := Generate(FromFS(publicFS, "importmap.json"))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if checksum == "" {
		t.Fatal("expected checksum")
	}

	var importMap Map
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

func TestGenerateChecksumsMountedFiles(t *testing.T) {
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

	content, _, err := Generate(Options{
		Files: []File{{FS: appFS, Path: "importmap.json"}},
		Sources: []Source{
			{FS: appFS, Prefix: "/"},
			{FS: adminFS, Prefix: "/ceyebr"},
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	var importMap Map
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

func TestGenerateMergesImportmapFiles(t *testing.T) {
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

	content, _, err := Generate(Options{
		Files: []File{
			{FS: baseFS, Path: "importmap.json"},
			{FS: previewFS, Path: "importmap.json"},
		},
		Sources: []Source{
			{FS: previewFS, Prefix: "/"},
			{FS: adminFS, Prefix: "/ceyebr"},
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	var importMap Map
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

func TestGenerateChecksumsAliases(t *testing.T) {
	adminFS := fstest.MapFS{
		"admin/index.js": {
			Data: []byte(`export const ready = true`),
		},
	}

	content, _, err := Generate(Options{
		Sources: []Source{{FS: adminFS, Prefix: "/_admin/assets"}},
		Aliases: []Alias{
			{Specifier: "@archetype/admin", Target: "/_admin/assets/admin/index.js"},
			{Specifier: "@archetype/admin/", Target: "/_admin/assets/admin/"},
			{Specifier: "preact", Target: "https://esm.sh/preact"},
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	var importMap Map
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

func TestGenerateHandlesSpecialTargets(t *testing.T) {
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

	content, _, err := Generate(FromFS(publicFS, "importmap.json"))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	var importMap Map
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

func TestGenerateScripts(t *testing.T) {
	publicFS := fstest.MapFS{
		"app.js": {Data: []byte(`export {}`)},
	}
	content := []byte(`export const v = 1`)

	generate := func() Map {
		t.Helper()
		out, _, err := Generate(Options{
			Sources: []Source{{FS: publicFS, Prefix: "/"}},
			Scripts: []Script{
				{Path: "/plugin/widget.js", Content: func() ([]byte, error) { return content, nil }},
				{Path: "plugin/versioned.js", Version: "build-42"},
				{Path: "/app.js", Version: "override"},
				{Path: "/broken.js", Content: func() ([]byte, error) { return nil, errors.New("boom") }},
			},
			Aliases: []Alias{
				{Specifier: "widget", Target: "/plugin/widget.js"},
				{Specifier: "versioned", Target: "/plugin/versioned.js?v=1"},
			},
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		var m Map
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("generated invalid JSON: %v", err)
		}
		return m
	}

	m := generate()
	widget := m.Imports["/plugin/widget.js"]
	if !strings.HasPrefix(widget, "/plugin/widget.js?checksum=") {
		t.Errorf("script missing from import map: %q", widget)
	}
	if got := m.Imports["widget"]; got != widget {
		t.Errorf("alias to script = %q, want %q", got, widget)
	}
	if got := m.Imports["versioned"]; !strings.HasPrefix(got, "/plugin/versioned.js?v=1&checksum=") {
		t.Errorf("alias to versioned script not checksummed: %q", got)
	}
	if got := m.Imports["/plugin/versioned.js"]; !strings.HasPrefix(got, "/plugin/versioned.js?checksum=") {
		t.Errorf("path without leading slash not normalized: %q", got)
	}
	if got, file := m.Imports["/app.js"], withChecksum("/app.js", mustChecksum(t, publicFS, "app.js")); got == file {
		t.Errorf("script did not override source file: %q", got)
	}
	if _, ok := m.Imports["/broken.js"]; ok {
		t.Error("script with failing Content should be skipped")
	}

	content = []byte(`export const v = 2`)
	if got := generate().Imports["/plugin/widget.js"]; got == widget {
		t.Errorf("checksum did not change with content: %q", got)
	}
}

func mustChecksum(t *testing.T, fsys fs.FS, name string) string {
	t.Helper()
	sum, err := checksumFile(fsys, name)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}
