# importmaps

[![Go Reference](https://pkg.go.dev/badge/bits2life.com/importmaps.svg)](https://pkg.go.dev/bits2life.com/importmaps)

Cache-busting [import maps](https://developer.mozilla.org/en-US/docs/Web/HTML/Element/script/type/importmap)
for Go web servers.

Every JavaScript file your server exposes is added to the import map with a
content checksum, so `/js/app.js` resolves to `/js/app.js?checksum=3f2a…`.
Scripts can then be cached aggressively (`immutable`, one year), and clients
still fetch exactly the files that changed as soon as the import map changes.

```sh
go get bits2life.com/importmaps
```

```go
import "bits2life.com/importmaps"
```

The package name is `importmap`.

## Usage

The simplest setup serves everything from one filesystem with an
`importmap.json` at its root:

```go
//go:embed public
var public embed.FS

publicFS, _ := fs.Sub(public, "public")
importmaps := importmap.NewCacheFromFS(publicFS, "importmap.json", development)
```

Then put the import map inline in your page template, before any module
scripts:

```go
tmpl.Execute(w, map[string]any{
	"Importmap": template.HTML(importmaps.GetInlineScriptTag()),
})
```

```html
<head>
  {{ .Importmap }}
  <script type="module">import "app";</script>
</head>
```

Set `DisableCache` (the last argument above) during development to rebuild
the map on every request, or call `Invalidate` after files change.

### Several filesystems and aliases

Reusable modules can contribute their own filesystems, mounted at URL
prefixes, plus extra import map files and aliases:

```go
importmaps := importmap.NewCache(importmap.CacheOptions{
	Importmaps: []importmap.ImportmapFile{
		{FS: publicFS, Path: "importmap.json"},
	},
	Sources: []importmap.FileSource{
		{FS: publicFS, Prefix: "/"},
		{FS: adminFS, Prefix: "/_admin/assets"},
	},
	Aliases: []importmap.Alias{
		{Specifier: "@example/admin", Target: "/_admin/assets/admin/index.js"},
	},
	DisableCache: development,
})
```

- Later import map files override earlier ones for the same specifier.
- Every `.js` and `.mjs` file in each source is added under its web path.
  When prefixes overlap, the longest prefix owns the file.
- Local targets (`/path/to/file.js`) in `imports`, `scopes` and aliases get a
  checksum. External URLs and prefix mappings (`"lib/": "/lib/"`) are left
  as they are.

Serving the static files themselves is up to you, for example with
`http.FileServerFS` and a long `Cache-Control` for requests carrying a
`checksum` query parameter.

### Generating without a cache

`GenerateImportMap` and `GenerateImportMapFromSources` return the import map
JSON and its SHA-256, if you want to write it to disk at build time or cache
it yourself.

### Serving importmap.json

`HTTPHandler` serves the generated map as `application/importmap+json` with
an ETag. Requests whose `checksum` query parameter matches the current map
are marked immutable; all others must revalidate.

Browsers do not currently load external import maps
(`<script type="importmap" src="…">` is ignored), so `GetScriptTag` and the
endpoint are mainly useful for tooling and debugging. Use
`GetInlineScriptTag` in pages.

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
   (`<script type="module">import "app";</script>`), or give its `src` a
   checksummed URL.

### Import map size

Listing every script makes the import map larger, and because it is inlined
it is sent with every page. For most applications this is a few kilobytes
of well-compressible JSON; in exchange, a change to one script only
invalidates that script and the page's import map.
