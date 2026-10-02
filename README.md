# importmaps

[![Go Reference](https://pkg.go.dev/badge/go.bits2life.com/importmaps.svg)](https://pkg.go.dev/go.bits2life.com/importmaps)

Cache-busting [import maps](https://developer.mozilla.org/en-US/docs/Web/HTML/Element/script/type/importmap)
for Go web servers.

Every JavaScript file your server exposes is added to the import map with a
content checksum, so `/js/app.js` resolves to `/js/app.js?checksum=3f2a…`.
Scripts can then be cached aggressively (`immutable`, one year), and clients
still fetch exactly the files that changed as soon as the import map changes.

```sh
go get go.bits2life.com/importmaps
```

```go
import "go.bits2life.com/importmaps" // package importmap
```

## Usage

The simplest setup serves everything from one filesystem with an
`importmap.json` at its root:

```go
//go:embed public
var public embed.FS

publicFS, _ := fs.Sub(public, "public")

importmaps := importmap.New(importmap.FromFS(publicFS, "importmap.json"))
http.Handle("/", importmaps.Middleware(http.FileServerFS(publicFS)))
```

The middleware serves the import map loader at `/importmap.js` and sets
caching headers on your scripts (see [Serving scripts](#serving-scripts)).

Then add the import map to your page template, before any module scripts:

```go
tmpl.Execute(w, map[string]any{"Importmap": importmaps.Script()})
```

```html
<head>
  {{ .Importmap }}
  <script type="module">import "app";</script>
</head>
```

### Loading the import map

Browsers don't load import maps from a `src` attribute, so there are two ways
to get the map into a page:

- **`Script()`** renders `<script src="/importmap.js?checksum=…"></script>`.
  The Cache serves that URL as a small, immutable script that writes the
  import map into the page with `document.write`. The map is downloaded once
  and then cached by the browser, which keeps pages small for applications
  with many JavaScript files. It costs one blocking request on the first
  visit. The tag must be in the page's markup (not inserted by script),
  before any module script.
- **`InlineScript()`** renders `<script type="importmap">{…}</script>`
  with the whole map. There's no extra request, but the map is sent with
  every page.

Both return `template.HTML`. If your Content Security Policy uses nonces,
build the tag yourself from `ScriptURL()` and add the nonce; the loader
copies it to the import map it writes.

### Options

Reusable modules can contribute their own filesystems, mounted at URL
prefixes, plus extra import map files and aliases:

```go
importmaps := importmap.New(importmap.Options{
	Files: []importmap.File{
		{FS: publicFS, Path: "importmap.json"},
	},
	Sources: []importmap.Source{
		{FS: publicFS, Prefix: "/"},
		{FS: adminFS, Prefix: "/_admin/assets"},
	},
	Aliases: []importmap.Alias{
		{Specifier: "@example/admin", Target: "/_admin/assets/admin/index.js"},
	},
	DisableCache: development,
})
```

- Later import map files override earlier ones for the same specifier, and
  aliases override both.
- Every `.js` and `.mjs` file in each source is added under its web path.
  When prefixes overlap, the longest prefix owns the file.
- Local targets (`/path/to/file.js`) in `imports`, `scopes` and aliases get a
  checksum. External URLs and prefix mappings (`"lib/": "/lib/"`) are left
  as they are.
- `Scripts` registers scripts that no filesystem can find, such as ones a
  package generates or serves from its own handler (see below).
- `DisableCache` rebuilds the map on every use, for development. Otherwise
  see [Updating scripts](#updating-scripts).
- `ScriptPath` is the loader URL used by `Script()` and the middleware
  (default `/importmap.js`).
- `Logger` receives warnings about missing files and unresolvable targets
  (default `slog.Default()`).

### Scripts outside any filesystem

A package that serves its own script resources can register them directly.
They are added to the import map and checksummed like files, and aliases
pointing at them are checksummed too:

```go
importmap.Options{
	Scripts: []importmap.Script{
		// Checksummed from its content.
		{Path: "/_widgets/widget.js", Content: widgets.Bundle},
		// Checksummed from a version string, for content that is
		// expensive to produce.
		{Path: "/_widgets/editor.js", Version: widgets.BuildID},
	},
	Aliases: []importmap.Alias{
		{Specifier: "@example/widget", Target: "/_widgets/widget.js"},
	},
}
```

Scripts can also be added or replaced later with `Cache.SetScript`.

### Updating scripts

A Cache computes every checksum once. When scripts change while the server
is running, update the cached map in one of these ways:

```go
// Recompute the checksums of specific files or registered scripts. New
// files in a source are added and deleted ones removed.
err := importmaps.Refresh("/js/app.js", "/_widgets/widget.js")

// Register a script, or replace one, for example with a new Version.
err = importmaps.SetScript(importmap.Script{Path: "/_widgets/editor.js", Version: newBuildID})

// Rebuild everything, re-reading import map files and walking sources.
importmaps.Invalidate()
```

`Refresh` and `SetScript` only touch the given scripts, and aliases and
scopes pointing at them pick up the new checksum. Pages rendered afterwards
get the new import map, and `Script()` points at a new loader URL.

#### From another process

When scripts are changed by a different process than the one holding the
Cache, mount `RefreshHandler` and have that process call it after each
write:

```http
POST /_importmap/refresh
Content-Type: application/json

{"paths": ["/js/app.js"]}
```

```json
{"urls": {"/js/app.js": "/js/app.js?checksum=9c1e…"}, "checksum": "…"}
```

The response carries each script's new URL (empty if it was deleted), for
example so an editor can `import()` the new version. Anyone who can reach
the endpoint can make the server re-read files, so mount it where only
trusted callers can reach it, such as an internal listener or behind your
own authentication.

### Serving scripts

`Cache.Middleware` wraps whatever serves your JavaScript files, such as
`http.FileServerFS`, and handles caching for every script in the import
map:

- A request whose `checksum` matches the script's current content is
  cached for a year as `immutable`.
- A request without a checksum, or with an outdated one, gets `no-cache`,
  so an old URL never caches new content for long.
- Responses carry an ETag based on the checksum, and matching
  `If-None-Match` requests get `304 Not Modified` without reaching your
  handler.
- Requests for `ScriptPath` get the loader script, and everything else
  passes through untouched.

With `DisableCache`, scripts always get `no-cache`. Wrap the middleware
outside any `http.StripPrefix`, since it matches the full URL path.

`Cache.URL("/js/app.js")` returns a script's current checksummed URL, for
example for a `<link rel="modulepreload">`.

### Generating without a cache

`importmap.Generate(opts)` returns the import map JSON and its SHA-256, if
you want to write it to disk at build time or cache it yourself.

## How it works

Browsers only apply an import map to module specifiers, but that includes
URL-like ones: a relative or absolute import such as `./util.js` or
`/js/util.js` is resolved to a URL first and then looked up in the map. By
listing every script in the map, all imports resolve to checksummed URLs,
whether they use a bare specifier (`import "app"`) or a path.

To keep that true:

1. Import scripts by bare specifier or by path under a known source. Avoid
   building import URLs at runtime or adding your own query strings.
2. Load the entry module through the import map as well
   (`<script type="module">import "app";</script>`). A `src` on a module
   script is fetched as written, without the import map.

## License

[MIT](LICENSE)
