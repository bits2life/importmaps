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
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
)

// ImportMap represents the structure of an import map.
type ImportMap struct {
	Imports   map[string]string            `json:"imports"`
	Scopes    map[string]map[string]string `json:"scopes,omitempty"`
	Integrity map[string]string            `json:"integrity,omitempty"`
}

// ImportmapCache manages the cached state of an importmap.
type ImportmapCache struct {
	content      string
	checksum     string
	mu           sync.RWMutex
	generator    func() (string, string, error)
	DisableCache bool // If true, disables caching entirely
}

// ImportmapFile identifies an importmap JSON file to merge into the
// generated importmap. Later files override earlier files for the same key.
type ImportmapFile struct {
	FS   fs.FS
	Path string
}

// Alias maps a JavaScript import specifier to a target URL. Targets that point
// at a known FileSource are checksummed the same way importmap file entries are.
type Alias struct {
	Specifier string
	Target    string
}

// FileSource identifies a filesystem mounted at a web URL prefix. It is used
// both to discover JavaScript files and to checksum local import targets.
type FileSource struct {
	FS     fs.FS
	Prefix string
}

// CacheOptions configures a cache-backed importmap generator.
type CacheOptions struct {
	Importmaps   []ImportmapFile
	Sources      []FileSource
	Aliases      []Alias
	DisableCache bool
}

// NewImportmapCache creates a new importmap cache with the given generator function
// Accepts a disableCache boolean to control caching
func NewImportmapCache(generator func() (string, string, error), disableCache bool) *ImportmapCache {
	return &ImportmapCache{
		generator:    generator,
		DisableCache: disableCache,
	}
}

// NewCacheFromFS creates an importmap cache backed by a standard fs.FS.
func NewCacheFromFS(publicFS fs.FS, importmapPath string, disableCache bool) *ImportmapCache {
	return NewCache(CacheOptions{
		Importmaps:   []ImportmapFile{{FS: publicFS, Path: importmapPath}},
		Sources:      []FileSource{{FS: publicFS, Prefix: "/"}},
		DisableCache: disableCache,
	})
}

// NewCache creates an importmap cache from one or more importmap files and
// web-mounted file sources.
func NewCache(opts CacheOptions) *ImportmapCache {
	return NewImportmapCache(func() (string, string, error) {
		return GenerateImportMapFromSources(opts.Importmaps, opts.Sources, opts.Aliases...)
	}, opts.DisableCache)
}

// Get returns the cached content and checksum if available, generating it if
// needed. Generation errors are logged with slog and reported as ok == false.
func (c *ImportmapCache) Get() (content string, checksum string, ok bool) {
	if c.DisableCache {
		// Always generate fresh, never cache
		return c.generate()
	}
	// Try to get from cache first
	c.mu.RLock()
	if c.content != "" {
		defer c.mu.RUnlock()
		return c.content, c.checksum, true
	}
	c.mu.RUnlock()

	// Cache miss, generate new content
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check in case another goroutine generated while we were waiting
	if c.content != "" {
		return c.content, c.checksum, true
	}

	content, checksum, ok = c.generate()
	if !ok {
		return "", "", false
	}

	// Update cache
	c.content = content
	c.checksum = checksum
	return content, checksum, true
}

func (c *ImportmapCache) generate() (string, string, bool) {
	content, checksum, err := c.generator()
	if err != nil {
		slog.Error("importmap: generating import map", "error", err)
		return "", "", false
	}
	return content, checksum, true
}

// GetScriptTag returns an HTML script tag referencing /importmap.json with the
// current checksum.
//
// Note: browsers do not currently support external import maps (a
// <script type="importmap"> with a src attribute is ignored), so pages should
// use GetInlineScriptTag instead.
func (c *ImportmapCache) GetScriptTag() string {
	_, checksum, ok := c.Get()
	if !ok {
		return `<script type="importmap" src="/importmap.json"></script>`
	}
	return `<script type="importmap" src="/importmap.json?checksum=` + checksum + `"></script>`
}

// GetInlineScriptTag returns an HTML script tag with the importmap content
// inlined. The content is JSON with HTML-sensitive characters escaped, so it
// is safe to embed in a page.
func (c *ImportmapCache) GetInlineScriptTag() string {
	content, _, ok := c.Get()
	if !ok {
		return `<script type="importmap">{"imports":{}}</script>`
	}
	return `<script type="importmap">` + content + `</script>`
}

// Invalidate clears the cache
func (c *ImportmapCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content = ""
	c.checksum = ""
}

// GenerateImportMap creates a new import map with cache-busting checksums from
// a standard Go filesystem rooted at the public directory.
func GenerateImportMap(publicFS fs.FS, importmapPath string) (string, string, error) {
	return GenerateImportMapFromSources(
		[]ImportmapFile{{FS: publicFS, Path: importmapPath}},
		[]FileSource{{FS: publicFS, Prefix: "/"}},
	)
}

// GenerateImportMapFromSources creates a new import map with cache-busting
// checksums from one or more importmap files and web-mounted filesystems.
//
// It returns the import map as indented JSON and the hex SHA-256 of that JSON.
// Missing or malformed importmap files and unresolvable local targets are
// logged with slog and skipped.
func GenerateImportMapFromSources(importmapFiles []ImportmapFile, sources []FileSource, aliases ...Alias) (string, string, error) {
	importMap := ImportMap{
		Imports: make(map[string]string),
	}

	for _, importmapFile := range importmapFiles {
		if importmapFile.FS == nil {
			continue
		}
		name := strings.TrimPrefix(importmapFile.Path, "/")
		if name == "" {
			name = "importmap.json"
		}
		baseMap, err := fs.ReadFile(importmapFile.FS, name)
		if err != nil {
			slog.Warn("importmap: reading import map file", "path", name, "error", err)
			continue
		}
		var base ImportMap
		if err := json.Unmarshal(baseMap, &base); err != nil {
			slog.Warn("importmap: parsing import map file", "path", name, "error", err)
			continue
		}
		mergeImportMap(&importMap, base)
	}

	normalizedSources := normalizeSources(sources)

	for _, source := range normalizedSources {
		if err := fs.WalkDir(source.FS, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !isScript(name) {
				return nil
			}
			webPath := source.webPath(name)
			owner, _, ok := sourceForWebPath(normalizedSources, webPath)
			if ok && owner.prefix != source.prefix {
				return nil
			}
			checksum, err := checksumFile(source.FS, name)
			if err != nil {
				slog.Warn("importmap: checksumming file", "path", webPath, "error", err)
				return nil
			}
			importMap.Imports[webPath] = withChecksum(webPath, checksum)
			return nil
		}); err != nil {
			return "", "", fmt.Errorf("importmap: walking source %q: %w", source.prefix+"/", err)
		}
	}

	applyAliases(importMap.Imports, aliases)

	checksumTargets(importMap.Imports, normalizedSources)
	for _, scope := range importMap.Scopes {
		checksumTargets(scope, normalizedSources)
	}

	result, err := json.MarshalIndent(importMap, "", "  ")
	if err != nil {
		return "", "", err
	}

	sum := sha256.Sum256(result)
	return string(result), hex.EncodeToString(sum[:]), nil
}

// mergeImportMap copies the entries of src into dst, overriding existing keys.
func mergeImportMap(dst *ImportMap, src ImportMap) {
	for k, v := range src.Imports {
		dst.Imports[k] = v
	}
	for scope, imports := range src.Scopes {
		if dst.Scopes == nil {
			dst.Scopes = make(map[string]map[string]string)
		}
		if dst.Scopes[scope] == nil {
			dst.Scopes[scope] = make(map[string]string)
		}
		for k, v := range imports {
			dst.Scopes[scope][k] = v
		}
	}
	for k, v := range src.Integrity {
		if dst.Integrity == nil {
			dst.Integrity = make(map[string]string)
		}
		dst.Integrity[k] = v
	}
}

// checksumTargets adds a checksum to every local, non-prefix target in
// imports that doesn't already carry one.
func checksumTargets(imports map[string]string, sources []normalizedSource) {
	for k, v := range imports {
		if !isLocalTarget(v) || strings.Contains(v, "checksum=") {
			continue
		}
		source, name, ok := sourceForWebPath(sources, v)
		if !ok {
			slog.Warn("importmap: no source for import target", "target", v)
			continue
		}
		checksum, err := checksumFile(source.FS, name)
		if err != nil {
			slog.Warn("importmap: checksumming import target", "target", v, "error", err)
			continue
		}
		imports[k] = withChecksum(v, checksum)
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

func applyAliases(imports map[string]string, aliases []Alias) {
	for _, alias := range aliases {
		specifier := strings.TrimSpace(alias.Specifier)
		target := strings.TrimSpace(alias.Target)
		if specifier == "" || target == "" {
			continue
		}
		imports[specifier] = target
	}
}

type normalizedSource struct {
	FS     fs.FS
	prefix string
}

func normalizeSources(sources []FileSource) []normalizedSource {
	var normalized []normalizedSource
	for _, source := range sources {
		if source.FS == nil {
			continue
		}
		prefix := "/" + strings.Trim(strings.TrimSpace(source.Prefix), "/")
		if prefix == "/" {
			prefix = ""
		}
		normalized = append(normalized, normalizedSource{FS: source.FS, prefix: prefix})
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		return len(normalized[i].prefix) > len(normalized[j].prefix)
	})
	return normalized
}

func (s normalizedSource) webPath(name string) string {
	name = strings.TrimPrefix(name, "./")
	if s.prefix == "" {
		return "/" + name
	}
	return s.prefix + "/" + name
}

func sourceForWebPath(sources []normalizedSource, webPath string) (normalizedSource, string, bool) {
	p, _, _ := strings.Cut(webPath, "#")
	p, _, _ = strings.Cut(p, "?")
	for _, source := range sources {
		if source.prefix == "" {
			name := strings.TrimPrefix(p, "/")
			if name != "" {
				return source, name, true
			}
			continue
		}
		if p == source.prefix {
			return source, "", true
		}
		if strings.HasPrefix(p, source.prefix+"/") {
			return source, strings.TrimPrefix(p[len(source.prefix):], "/"), true
		}
	}
	return normalizedSource{}, "", false
}

func checksumFile(fsys fs.FS, name string) (string, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// HTTPHandler returns a standard http.Handler for serving the importmap.json endpoint.
//
// Requests whose checksum query parameter matches the current import map are
// served as immutable. All other requests must revalidate, using the ETag.
func HTTPHandler(cache *ImportmapCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		importMap, checksum, ok := cache.Get()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": "Failed to generate import map"}`))
			return
		}

		etag := `"sha256-` + checksum + `"`
		w.Header().Set("Content-Type", "application/importmap+json")
		w.Header().Set("ETag", etag)
		if r.URL.Query().Get("checksum") == checksum {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Unversioned or stale URL: never cache the current content under it
			// for long, but allow cheap revalidation.
			w.Header().Set("Cache-Control", "no-cache")
		}

		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			w.Write([]byte(importMap))
		}
	}
}

// etagMatches reports whether an If-None-Match header value matches etag,
// using weak comparison as required for If-None-Match.
func etagMatches(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
