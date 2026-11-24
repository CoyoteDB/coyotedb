// Package dashboard holds the web dashboard's built files (dist/, made by
// `npm run build` in this folder) embedded into the coyote binary.
//
// dist/ is git-ignored except for dist/.gitkeep, so a plain `go build`
// works without Node: the embedded tree is then empty, Built reports false,
// and the agent serves Fallback instead of the app.
//
// Serving rules are in API.md §16: files from FS, index.html for any other
// GET outside /v1/, long-lived caching only under assets/, and the security
// headers of API.md §2.5 on every response.
package dashboard

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built dashboard rooted at dist/ (index.html at its top).
// It holds only .gitkeep when the dashboard was not built.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// fs.Sub fails only for an invalid path, and "dist" is valid.
		panic(err)
	}
	return sub
}

// Built reports whether the dashboard was built into this binary.
func Built() bool {
	_, err := fs.Stat(FS(), "index.html")
	return err == nil
}

const fallbackStyle = `body{margin:0;background:#fff;color:#1b1b1b;font:16px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}` +
	`main{max-width:36rem;margin:12vh auto;padding:0 16px}` +
	`.k{font:500 12px/1.4 ui-monospace,Menlo,Consolas,monospace;letter-spacing:.07em;text-transform:uppercase;color:#7a1f2b;margin:0 0 16px}` +
	`h1{font-weight:300;font-size:2rem;line-height:1.1;margin:0 0 16px}` +
	`@media (prefers-color-scheme:dark){body{background:#121212;color:#f1f1f1}.k{color:#d76070}}`

// Fallback is served (as text/html) for every dashboard path when Built is
// false. It is self-contained: no scripts and no other files.
const Fallback = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>CoyoteDB</title>
<style>` + fallbackStyle + `</style>
</head>
<body>
<main>
<p class="k">CoyoteDB</p>
<h1>The dashboard is not included in this build</h1>
<p>This copy of <code>coyote</code> was built without its web dashboard. The database itself works as usual.</p>
<p>Use the command line instead, for example <code>coyote status</code>. To include the dashboard, run <code>npm ci &amp;&amp; npm run build</code> in the <code>dashboard</code> folder, then build <code>coyote</code> again.</p>
</main>
</body>
</html>
`

// FallbackCSP is the Content-Security-Policy to send with Fallback: it
// allows exactly its one inline style block and nothing else.
var FallbackCSP = func() string {
	sum := sha256.Sum256([]byte(fallbackStyle))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()
