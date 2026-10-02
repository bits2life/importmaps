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
	"errors"
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

// Script is a script served outside of any Source, such as one generated at
// runtime or served by another package's handler. It is added to the import
// map like files found in a Source, and aliases pointing at it are
// checksummed too.
type Script struct {
	Path string // URL path, such as "/_admin/app.js".

	// Content returns the script's current content, which is checksummed.
	// If Content is nil, Version is checksummed instead.
	Content func() ([]byte, error)

	// Version identifies the script's current content, such as a build
	// hash, for scripts whose content is expensive to read.
	Version string
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

	// Scripts that no Source can find. They override Source files with the
	// same path.
	Scripts []Script

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
	st, err := build(opts)
	if err != nil {
		return "", "", err
	}
	return st.render()
}

// state is a generated import map before checksums are applied, so that
// individual checksums can be recomputed without rebuilding everything.
type state struct {
	log     *slog.Logger
	base    Map               // Merged import map files.
	aliases []Alias           // Applied after listed scripts.
	sources []source          // Sorted by descending prefix length.
	scripts map[string]Script // Registered scripts by URL path.
	listed  map[string]bool   // URL paths added to the import map as keys.
	sums    map[string]string // Full hex checksums by URL path.
}

func build(opts Options) (*state, error) {
	st := &state{
		log:     opts.logger(),
		base:    Map{Imports: make(map[string]string)},
		aliases: opts.Aliases,
		sources: normalizeSources(opts.Sources),
		scripts: make(map[string]Script),
		listed:  make(map[string]bool),
		sums:    make(map[string]string),
	}

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
			st.log.Warn("importmap: reading import map file", "path", name, "error", err)
			continue
		}
		var base Map
		if err := json.Unmarshal(data, &base); err != nil {
			st.log.Warn("importmap: parsing import map file", "path", name, "error", err)
			continue
		}
		st.base.merge(base)
	}

	for _, source := range st.sources {
		err := fs.WalkDir(source.fs, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !isScript(name) {
				return nil
			}
			webPath := source.webPath(name)
			if owner, _, ok := sourceForWebPath(st.sources, webPath); ok && owner.prefix != source.prefix {
				return nil
			}
			st.listed[webPath] = true
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("importmap: walking source %q: %w", source.prefix+"/", err)
		}
	}

	for _, script := range opts.Scripts {
		p := scriptPath(script.Path)
		st.scripts[p] = script
		st.listed[p] = true
	}

	for p := range st.listed {
		st.refresh(p)
	}
	for _, imports := range st.targets() {
		for _, v := range imports {
			if p, ok := st.unresolved(v); ok {
				st.refresh(p)
			}
		}
	}
	return st, nil
}

// targets returns the import tables whose local targets get checksums.
func (st *state) targets() []map[string]string {
	tables := []map[string]string{st.base.Imports, {}}
	for _, a := range st.aliases {
		tables[1][a.Specifier] = strings.TrimSpace(a.Target)
	}
	for _, scope := range st.base.Scopes {
		tables = append(tables, scope)
	}
	return tables
}

// unresolved reports whether target v needs a checksum, and its URL path.
func (st *state) unresolved(v string) (string, bool) {
	if !isLocalTarget(v) || strings.Contains(v, "checksum=") {
		return "", false
	}
	return urlPath(v), true
}

// refresh recomputes the checksum of the script at URL path p, removing it
// from the import map if it no longer exists. It returns an error if p is
// neither a registered script nor inside a source.
func (st *state) refresh(p string) error {
	sum, err := st.checksum(p)
	switch {
	case err == nil:
		st.sums[p] = sum
		return nil
	case errors.Is(err, fs.ErrNotExist):
		delete(st.sums, p)
		delete(st.listed, p)
		st.log.Warn("importmap: script not found", "path", p)
		return nil
	default:
		delete(st.sums, p)
		st.log.Warn("importmap: checksumming script", "path", p, "error", err)
		return err
	}
}

func (st *state) checksum(p string) (string, error) {
	if script, ok := st.scripts[p]; ok {
		return script.checksum()
	}
	src, name, ok := sourceForWebPath(st.sources, p)
	if !ok {
		return "", fmt.Errorf("importmap: no script or source for %q", p)
	}
	return checksumFile(src.fs, name)
}

// render applies the current checksums and returns the import map JSON and
// its hex SHA-256.
func (st *state) render() (content string, checksum string, err error) {
	m := Map{
		Imports:   make(map[string]string, len(st.base.Imports)+len(st.listed)),
		Integrity: st.base.Integrity,
	}
	for k, v := range st.base.Imports {
		m.Imports[k] = v
	}
	for p := range st.listed {
		if sum, ok := st.sums[p]; ok {
			m.Imports[p] = withChecksum(p, sum)
		}
	}
	for _, alias := range st.aliases {
		specifier := strings.TrimSpace(alias.Specifier)
		target := strings.TrimSpace(alias.Target)
		if specifier != "" && target != "" {
			m.Imports[specifier] = target
		}
	}
	st.checksumTargets(m.Imports)
	for scope, imports := range st.base.Scopes {
		if m.Scopes == nil {
			m.Scopes = make(map[string]map[string]string)
		}
		m.Scopes[scope] = make(map[string]string, len(imports))
		for k, v := range imports {
			m.Scopes[scope][k] = v
		}
		st.checksumTargets(m.Scopes[scope])
	}

	result, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(result)
	return string(result), hex.EncodeToString(sum[:]), nil
}

// checksumTargets adds a checksum to every local, non-prefix target in
// imports that doesn't already carry one.
func (st *state) checksumTargets(imports map[string]string) {
	for k, v := range imports {
		p, ok := st.unresolved(v)
		if !ok {
			continue
		}
		if sum, ok := st.sums[p]; ok {
			imports[k] = withChecksum(v, sum)
		}
	}
}

func scriptPath(p string) string {
	return "/" + strings.TrimPrefix(strings.TrimSpace(p), "/")
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

// urlPath returns url without its query and fragment.
func urlPath(url string) string {
	p, _, _ := strings.Cut(url, "#")
	p, _, _ = strings.Cut(p, "?")
	return p
}

func sourceForWebPath(sources []source, webPath string) (source, string, bool) {
	p := urlPath(webPath)
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

func (s Script) checksum() (string, error) {
	data := []byte(s.Version)
	if s.Content != nil {
		var err error
		if data, err = s.Content(); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func checksumFile(fsys fs.FS, name string) (string, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// clone returns a copy of st whose checksums can be changed independently.
func (st *state) clone() *state {
	c := *st
	c.listed = make(map[string]bool, len(st.listed))
	for k, v := range st.listed {
		c.listed[k] = v
	}
	c.sums = make(map[string]string, len(st.sums))
	for k, v := range st.sums {
		c.sums[k] = v
	}
	return &c
}
