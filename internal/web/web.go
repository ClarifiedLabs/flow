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

//go:embed assets/* assets/elements/* src/*.module.css
var assetFS embed.FS

// cssModules is the ordered stylesheet manifest. Each entry names the element
// its selectors are scoped to; tag names are unique, so a component's rules
// cannot leak. The token sheet is unscoped because :root and the theme media
// queries have to reach the whole document. Order is cascade order: tokens,
// then shared chrome, then components.
var cssModules = []struct{ Name, Scope string }{
	{"tokens.module.css", ""},
	{"base.module.css", "flow-app"},
	{"board.module.css", "flow-board"},
	{"board-sort.module.css", "flow-board-sort"},
	{"attention-strip.module.css", "flow-attention-strip"},
	{"throughput-strip.module.css", "flow-throughput-strip"},
	{"lane.module.css", "flow-lane"},
	{"task-card.module.css", "flow-task-card"},
	{"step-rail.module.css", "flow-step-rail"},
	{"board-table.module.css", "flow-board-table"},
	{"task-detail.module.css", "flow-task-detail"},
	{"task-rail.module.css", "flow-task-rail"},
	{"task-relations.module.css", "flow-task-relations"},
	{"run-spine.module.css", "flow-run-spine"},
	{"now-card.module.css", "flow-now-card"},
	{"held-panel.module.css", "flow-held-panel"},
	{"tab-strip.module.css", "flow-tab-strip"},
	{"run-list.module.css", "flow-run-list"},
	{"workflow-graph.module.css", "flow-workflow-graph"},
	{"check-list.module.css", "flow-check-list"},
	{"findings-list.module.css", "flow-findings-list"},
	{"activity-feed.module.css", "flow-activity-feed"},
	{"change.module.css", "flow-change"},
	{"diff.module.css", "flow-diff"},
	{"inline-thread.module.css", "flow-inline-thread"},
	{"review-bar.module.css", "flow-review-bar"},
	{"review-panel.module.css", "flow-review-panel"},
	{"epic.module.css", "flow-epic"},
	{"features.module.css", "flow-features"},
	{"feature.module.css", "flow-feature"},
}

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
	modules := make([]webassetbuild.Module, 0, len(cssModules))
	for _, module := range cssModules {
		source, err := assetFS.ReadFile("src/" + module.Name)
		if err != nil {
			return nil, fmt.Errorf("read css module %s: %w", module.Name, err)
		}
		modules = append(modules, webassetbuild.Module{Name: module.Name, Scope: module.Scope, Source: source})
	}
	return webassetbuild.BuildModules(modules), nil
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
