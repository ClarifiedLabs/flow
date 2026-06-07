package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/web/webassetbuild"
)

//go:embed assets/* src/app.module.css
var assetFS embed.FS

var assetVersion = computeAssetVersion()

func IndexHTML() ([]byte, error) {
	contents, err := assetFS.ReadFile("assets/index.html")
	if err != nil {
		return nil, fmt.Errorf("read index html: %w", err)
	}
	rendered := strings.ReplaceAll(string(contents), "{{VERSION}}", assetVersion)

	return []byte(rendered), nil
}

func Asset(name string) ([]byte, string, bool) {
	name = strings.Trim(strings.TrimSpace(name), "/")
	if name == "" || strings.Contains(name, "..") {
		return nil, "", false
	}
	if name == "app.css" {
		contents, err := generatedCSS()
		return contents, contentType(name), err == nil
	}
	contents, err := assetFS.ReadFile("assets/" + name)
	if err != nil {
		return nil, "", false
	}

	return contents, contentType(name), true
}

func computeAssetVersion() string {
	hash := sha256.New()
	if css, err := generatedCSS(); err == nil {
		_, _ = hash.Write(css)
	}
	// Hash every served JS module (not just app.js) so the cache-busting version
	// reflects a change to any module the entry imports. Sorted for determinism.
	names, _ := fs.Glob(assetFS, "assets/*.js")
	sort.Strings(names)
	for _, name := range names {
		if js, err := assetFS.ReadFile(name); err == nil {
			_, _ = hash.Write([]byte(name))
			_, _ = hash.Write(js)
		}
	}

	return hex.EncodeToString(hash.Sum(nil))[:12]
}

// AssetETag returns a strong ETag (a quoted content hash) for the given asset
// bytes. Unversioned requests — notably the browser's native ES module imports
// like `import "./markdown.js"`, which carry no ?v= cache key — are served with
// this ETag plus Cache-Control: no-cache so an edited module is revalidated and
// never served stale from an immutable cache.
func AssetETag(contents []byte) string {
	sum := sha256.Sum256(contents)
	return `"` + hex.EncodeToString(sum[:])[:16] + `"`
}

func generatedCSS() ([]byte, error) {
	source, err := assetFS.ReadFile("src/app.module.css")
	if err != nil {
		return nil, fmt.Errorf("read css module source: %w", err)
	}
	return webassetbuild.BuildCSS(source), nil
}

func contentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
