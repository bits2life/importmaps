// Package importmap generates cache-busting import maps for JavaScript
// modules served from Go.
//
// Every local JavaScript file is mapped to a URL carrying a content checksum
// ("/js/app.js" -> "/js/app.js?checksum=…"), so browsers can cache scripts
// forever and still pick up changes as soon as the import map changes.
package importmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
)

// Map is the JSON structure of an import map.
type Map struct {
	Imports   map[string]string            `json:"imports"`
	Scopes    map[string]map[string]string `json:"scopes,omitempty"`
	Integrity map[string]string            `json:"integrity,omitempty"`
}

// File is an import map JSON file to merge into the generated import map.
type File struct {
	FS   fs.FS
	Path string // Defaults to "importmap.json".
}

// Source is a filesystem served at a URL prefix. Its JavaScript files are
// added to the import map, and local import targets are checksummed from it.
type Source struct {
	FS     fs.FS
	Prefix string // URL prefix, such as "/" or "/_admin/assets".
}

// Alias maps an import specifier to a target URL. Targets inside a Source are
// checksummed like entries from import map files.
type Alias struct {
	Specifier string
	Target    string
}

// Options configures import map generation.
type Options struct {
	// Files are merged in order; later files override earlier ones for the
	// same specifier.
	Files []File

	// Sources whose .js and .mjs files are added to the import map. When
	// prefixes overlap, the longest prefix owns a file.
	Sources []Source

	// Aliases are applied after Files and override them.
	Aliases []Alias

	// DisableCache makes a Cache regenerate the import map on every use,
	// which is convenient during development. Generate ignores it.
	DisableCache bool

	// ScriptPath is the URL the Cache is served at, used by Cache.Script.
	// Defaults to "/importmap.js".
	ScriptPath string

	// Logger receives warnings about missing files and unresolvable
	// targets. Defaults to slog.Default().
	Logger *slog.Logger
}

// FromFS returns Options for the common case of a single filesystem served
// at "/" with an import map file at importmapPath.
func FromFS(fsys fs.FS, importmapPath string) Options {
	return Options{
		Files:   []File{{FS: fsys, Path: importmapPath}},
		Sources: []Source{{FS: fsys, Prefix: "/"}},
	}
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// Generate builds the import map described by opts and returns it as
// indented JSON together with the hex SHA-256 of that JSON.
//
// Missing or malformed import map files and unresolvable local targets are
// logged and skipped. Errors walking a Source are returned.
func Generate(opts Options) (content string, checksum string, err error) {
	log := opts.logger()
	m := Map{Imports: make(map[string]string)}

	for _, f := range opts.Files {
		if f.FS == nil {
			continue
		}
		name := strings.TrimPrefix(f.Path, "/")
		if name == "" {
			name = "importmap.json"
		}
		data, err := fs.ReadFile(f.FS, name)
		if err != nil {
			log.Warn("importmap: reading import map file", "path", name, "error", err)
			continue
		}
		var base Map
		if err := json.Unmarshal(data, &base); err != nil {
			log.Warn("importmap: parsing import map file", "path", name, "error", err)
			continue
		}
		m.merge(base)
	}

	sources := normalizeSources(opts.Sources)

	for _, source := range sources {
		err := fs.WalkDir(source.fs, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !isScript(name) {
				return nil
			}
			webPath := source.webPath(name)
			if owner, _, ok := sourceForWebPath(sources, webPath); ok && owner.prefix != source.prefix {
				return nil
			}
			sum, err := checksumFile(source.fs, name)
			if err != nil {
				log.Warn("importmap: checksumming file", "path", webPath, "error", err)
				return nil
			}
			m.Imports[webPath] = withChecksum(webPath, sum)
			return nil
		})
		if err != nil {
			return "", "", fmt.Errorf("importmap: walking source %q: %w", source.prefix+"/", err)
		}
	}

	for _, alias := range opts.Aliases {
		specifier := strings.TrimSpace(alias.Specifier)
		target := strings.TrimSpace(alias.Target)
		if specifier != "" && target != "" {
			m.Imports[specifier] = target
		}
	}

	checksumTargets(m.Imports, sources, log)
	for _, scope := range m.Scopes {
		checksumTargets(scope, sources, log)
	}

	result, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(result)
	return string(result), hex.EncodeToString(sum[:]), nil
}

// merge copies the entries of src into m, overriding existing keys.
func (m *Map) merge(src Map) {
	for k, v := range src.Imports {
		m.Imports[k] = v
	}
	for scope, imports := range src.Scopes {
		if m.Scopes == nil {
			m.Scopes = make(map[string]map[string]string)
		}
		if m.Scopes[scope] == nil {
			m.Scopes[scope] = make(map[string]string)
		}
		for k, v := range imports {
			m.Scopes[scope][k] = v
		}
	}
	for k, v := range src.Integrity {
		if m.Integrity == nil {
			m.Integrity = make(map[string]string)
		}
		m.Integrity[k] = v
	}
}

// checksumTargets adds a checksum to every local, non-prefix target in
// imports that doesn't already carry one.
func checksumTargets(imports map[string]string, sources []source, log *slog.Logger) {
	for k, v := range imports {
		if !isLocalTarget(v) || strings.Contains(v, "checksum=") {
			continue
		}
		src, name, ok := sourceForWebPath(sources, v)
		if !ok {
			log.Warn("importmap: no source for import target", "target", v)
			continue
		}
		sum, err := checksumFile(src.fs, name)
		if err != nil {
			log.Warn("importmap: checksumming import target", "target", v, "error", err)
			continue
		}
		imports[k] = withChecksum(v, sum)
	}
}

// isLocalTarget reports whether v is a root-relative URL to a single file
// (not a protocol-relative URL and not a "/prefix/" mapping).
func isLocalTarget(v string) bool {
	return strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.HasSuffix(v, "/")
}

func isScript(name string) bool {
	switch path.Ext(name) {
	case ".js", ".mjs":
		return true
	}
	return false
}

// withChecksum appends a shortened checksum query parameter to url.
func withChecksum(url, checksum string) string {
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "checksum=" + checksum[:16]
}

type source struct {
	fs     fs.FS
	prefix string // "" for the root, otherwise "/prefix" without a trailing slash.
}

func normalizeSources(sources []Source) []source {
	var normalized []source
	for _, s := range sources {
		if s.FS == nil {
			continue
		}
		prefix := "/" + strings.Trim(strings.TrimSpace(s.Prefix), "/")
		if prefix == "/" {
			prefix = ""
		}
		normalized = append(normalized, source{fs: s.FS, prefix: prefix})
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		return len(normalized[i].prefix) > len(normalized[j].prefix)
	})
	return normalized
}

func (s source) webPath(name string) string {
	return s.prefix + "/" + strings.TrimPrefix(name, "./")
}

func sourceForWebPath(sources []source, webPath string) (source, string, bool) {
	p, _, _ := strings.Cut(webPath, "#")
	p, _, _ = strings.Cut(p, "?")
	for _, s := range sources {
		if s.prefix == "" {
			if name := strings.TrimPrefix(p, "/"); name != "" {
				return s, name, true
			}
			continue
		}
		if p == s.prefix {
			return s, "", true
		}
		if strings.HasPrefix(p, s.prefix+"/") {
			return s, strings.TrimPrefix(p[len(s.prefix):], "/"), true
		}
	}
	return source{}, "", false
}

func checksumFile(fsys fs.FS, name string) (string, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
