package importmap

import (
	"encoding/json"
	"net/http"
)

// URL returns the checksummed URL of the script at URL path p, such as
// "/js/app.js?checksum=…", and whether the import map knows that script.
func (c *Cache) URL(p string) (string, bool) {
	e, err := c.get()
	if err != nil {
		c.opts.logger().Error("importmap: generating import map", "error", err)
		return "", false
	}
	return e.url(scriptPath(urlPath(p)))
}

func (e *entry) url(p string) (string, bool) {
	sum, ok := e.state.sums[p]
	if !ok {
		return "", false
	}
	return withChecksum(p, sum), true
}

// Middleware wraps a handler serving JavaScript files, such as an
// http.FileServer, and manages caching for the scripts in the import map:
//
//   - Requests whose checksum query parameter matches the script's current
//     content are marked immutable for a year.
//   - Requests without a checksum, or with a stale one, must revalidate.
//   - Every response gets an ETag derived from the script's checksum, and
//     matching If-None-Match requests are answered with 304 Not Modified
//     without calling next.
//
// Requests for Options.ScriptPath are served the loader script. All other
// requests, including ones for files the import map doesn't know, are passed
// to next untouched. With Options.DisableCache, scripts are always marked
// for revalidation.
func (c *Cache) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == c.opts.ScriptPath {
			c.ServeHTTP(w, r)
			return
		}
		if c.opts.DisableCache {
			if isScript(r.URL.Path) {
				w.Header().Set("Cache-Control", "no-cache")
			}
			next.ServeHTTP(w, r)
			return
		}

		e, err := c.get()
		if err != nil {
			c.opts.logger().Error("importmap: generating import map", "error", err)
			next.ServeHTTP(w, r)
			return
		}
		sum, ok := e.state.sums[r.URL.Path]
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		etag := `"sha256-` + sum + `"`
		w.Header().Set("ETag", etag)
		if r.URL.Query().Get("checksum") == sum[:16] {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RefreshRequest is the JSON body RefreshHandler accepts.
type RefreshRequest struct {
	Paths []string `json:"paths"`
}

// RefreshResponse is the JSON body RefreshHandler responds with.
type RefreshResponse struct {
	// URLs maps each requested path to its new checksummed URL, or to ""
	// if the script no longer exists.
	URLs map[string]string `json:"urls"`

	// Checksum is the new hex SHA-256 of the import map.
	Checksum string `json:"checksum"`

	// Error describes paths that couldn't be refreshed, if any.
	Error string `json:"error,omitempty"`
}

// RefreshHandler returns a handler that refreshes the checksums of the
// scripts named in a POST request, for processes that change script files
// outside the one holding the Cache:
//
//	POST {"paths": ["/js/app.js"]}
//
// It responds with a RefreshResponse, with status 400 if the body is
// invalid and 422 if some paths couldn't be refreshed.
//
// Anyone who can reach the handler can make the server re-read files, so
// mount it where only trusted callers can reach it.
func (c *Cache) RefreshHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req RefreshRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}

		status := http.StatusOK
		var resp RefreshResponse
		if err := c.Refresh(req.Paths...); err != nil {
			status = http.StatusUnprocessableEntity
			resp.Error = err.Error()
		}
		e, err := c.get()
		if err != nil {
			c.opts.logger().Error("importmap: generating import map", "error", err)
			http.Error(w, "failed to generate import map", http.StatusInternalServerError)
			return
		}
		resp.Checksum = e.checksum
		resp.URLs = make(map[string]string, len(req.Paths))
		for _, p := range req.Paths {
			resp.URLs[p], _ = e.url(scriptPath(urlPath(p)))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	})
}
