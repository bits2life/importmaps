package importmap

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"sync"
)

// Cache generates an import map on first use and keeps it until Invalidate
// is called, or regenerates it every time if Options.DisableCache is set.
//
// A Cache is also an http.Handler serving the loader script that Script
// references. It is safe for concurrent use.
type Cache struct {
	mu    sync.RWMutex
	opts  Options // Scripts is guarded by mu; the rest is read-only.
	entry *entry
}

type entry struct {
	state    *state // Inputs for Refresh; never modified once cached.
	content  string // Import map JSON.
	checksum string // Hex SHA-256 of content.
	loader   string // JavaScript that writes the import map into the page.
}

// New returns a Cache for the import map described by opts.
func New(opts Options) *Cache {
	if opts.ScriptPath == "" {
		opts.ScriptPath = "/importmap.js"
	}
	opts.Scripts = append([]Script(nil), opts.Scripts...)
	return &Cache{opts: opts}
}

// Get returns the import map JSON and its hex SHA-256, generating it if
// needed.
func (c *Cache) Get() (content string, checksum string, err error) {
	e, err := c.get()
	if err != nil {
		return "", "", err
	}
	return e.content, e.checksum, nil
}

func (c *Cache) get() (*entry, error) {
	if c.opts.DisableCache {
		c.mu.RLock()
		opts := c.opts
		c.mu.RUnlock()
		return generate(opts)
	}

	c.mu.RLock()
	e := c.entry
	c.mu.RUnlock()
	if e != nil {
		return e, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry != nil {
		return c.entry, nil
	}
	e, err := generate(c.opts)
	if err != nil {
		return nil, err
	}
	c.entry = e
	return e, nil
}

func generate(opts Options) (*entry, error) {
	st, err := build(opts)
	if err != nil {
		return nil, err
	}
	return newEntry(st)
}

func newEntry(st *state) (*entry, error) {
	content, checksum, err := st.render()
	if err != nil {
		return nil, err
	}
	// content is JSON with <, > and & escaped, so it is safe inside a script
	// element, and encoding it again yields a valid JavaScript string literal.
	tag, err := json.Marshal(`<script type="importmap"NONCE>` + content + `</script>`)
	if err != nil {
		return nil, err
	}
	nonce := `" + (n ? ' nonce="' + n.replace(/"/g, "") + '"' : "") + "`
	loader := "(function () {\n" +
		"  var s = document.currentScript, n = s && s.nonce;\n" +
		"  document.write(" + strings.Replace(string(tag), "NONCE", nonce, 1) + ");\n" +
		"})();\n"
	return &entry{state: st, content: content, checksum: checksum, loader: loader}, nil
}

// Refresh recomputes the checksums of the scripts at the given URL paths,
// such as "/js/app.js", and updates the cached import map without
// rebuilding the rest of it. A path can be a file in a Source or a
// registered Script; new script files in a Source are added, and files that
// no longer exist are removed.
//
// Refresh returns an error for a path that is neither a Script nor inside a
// Source, or whose content can't be read; the other paths are still
// refreshed. It does nothing if the import map hasn't been generated yet or
// caching is disabled, since the next use computes every checksum anyway.
func (c *Cache) Refresh(paths ...string) error {
	if c.opts.DisableCache {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry == nil {
		return nil
	}

	st := c.entry.state.clone()
	var errs []error
	for _, p := range paths {
		p = scriptPath(urlPath(p))
		if err := st.refresh(p); err != nil {
			errs = append(errs, err)
			continue
		}
		_, registered := st.scripts[p]
		if _, ok := st.sums[p]; ok && (registered || isScript(p)) {
			st.listed[p] = true
		}
	}
	e, err := newEntry(st)
	if err != nil {
		return err
	}
	c.entry = e
	return errors.Join(errs...)
}

// SetScript registers s, replacing any Script with the same Path, and
// updates its checksum in the cached import map without rebuilding the rest
// of it. Use it to add scripts after the Cache is created, or to change a
// Script's Version or Content function.
func (c *Cache) SetScript(s Script) error {
	p := scriptPath(s.Path)
	c.mu.Lock()
	defer c.mu.Unlock()

	scripts := make([]Script, 0, len(c.opts.Scripts)+1)
	for _, existing := range c.opts.Scripts {
		if scriptPath(existing.Path) != p {
			scripts = append(scripts, existing)
		}
	}
	c.opts.Scripts = append(scripts, s)

	if c.opts.DisableCache || c.entry == nil {
		return nil
	}
	st := c.entry.state.clone()
	st.scripts = make(map[string]Script, len(c.entry.state.scripts)+1)
	for k, v := range c.entry.state.scripts {
		st.scripts[k] = v
	}
	st.scripts[p] = s
	st.listed[p] = true
	refreshErr := st.refresh(p)
	e, err := newEntry(st)
	if err != nil {
		return err
	}
	c.entry = e
	return refreshErr
}

// Invalidate discards the cached import map, so the next use regenerates it.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = nil
}

// InlineScript returns a <script type="importmap"> element containing the
// import map. If generation fails, the error is logged and an empty import
// map is returned.
func (c *Cache) InlineScript() template.HTML {
	e, err := c.get()
	if err != nil {
		c.opts.logger().Error("importmap: generating import map", "error", err)
		return `<script type="importmap">{"imports":{}}</script>`
	}
	return template.HTML(`<script type="importmap">` + e.content + `</script>`)
}

// ScriptURL returns the versioned URL of the loader script, for pages that
// build their own script tag (for example to add a CSP nonce).
func (c *Cache) ScriptURL() string {
	e, err := c.get()
	if err != nil {
		c.opts.logger().Error("importmap: generating import map", "error", err)
		return c.opts.ScriptPath
	}
	sep := "?"
	if strings.Contains(c.opts.ScriptPath, "?") {
		sep = "&"
	}
	return c.opts.ScriptPath + sep + "checksum=" + e.checksum
}

// Script returns a classic, synchronous <script> element that loads the
// import map from the Cache's handler. The loader writes the import map into
// the page with document.write, so it must appear in the document's markup
// before any module script. Unlike InlineScript, the import map is then
// cached by the browser instead of being sent with every page.
func (c *Cache) Script() template.HTML {
	return template.HTML(`<script src="` + template.HTMLEscapeString(c.ScriptURL()) + `"></script>`)
}

// ServeHTTP serves the loader script referenced by Script.
//
// Requests whose checksum query parameter matches the current import map are
// served as immutable. All other requests must revalidate, using the ETag.
func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e, err := c.get()
	if err != nil {
		c.opts.logger().Error("importmap: generating import map", "error", err)
		http.Error(w, "failed to generate import map", http.StatusInternalServerError)
		return
	}

	etag := `"sha256-` + e.checksum + `"`
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("ETag", etag)
	if r.URL.Query().Get("checksum") == e.checksum {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Unversioned or stale URL: don't let the current content be cached
		// under it for long, but allow cheap revalidation.
		w.Header().Set("Cache-Control", "no-cache")
	}

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write([]byte(e.loader))
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
