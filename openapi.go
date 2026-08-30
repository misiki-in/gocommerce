package gocommerce

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// specSource is the maintained OpenAPI contract, embedded in the binary so a
// running store always serves the contract it was built with.
//
//go:embed openapi.json
var specSource []byte

// OpenAPIContributor is an optional module capability: a module that mounts
// routes returns a JSON fragment with "paths" and, optionally, "components"
// so its endpoints appear in the served contract alongside core's.
//
//	func (m *Module) OpenAPI() []byte { return fragmentJSON }
type OpenAPIContributor interface {
	OpenAPI() []byte
}

// buildSpec merges module fragments into the embedded contract once, at
// startup, so serving /doc is a byte copy rather than a marshal per request.
func (a *App) buildSpec() error {
	var spec map[string]any
	if err := json.Unmarshal(specSource, &spec); err != nil {
		return fmt.Errorf("parse embedded openapi.json: %w", err)
	}

	for _, m := range a.modules {
		c, ok := m.(OpenAPIContributor)
		if !ok {
			continue
		}
		raw := c.OpenAPI()
		if len(raw) == 0 {
			continue
		}
		var frag map[string]any
		if err := json.Unmarshal(raw, &frag); err != nil {
			return fmt.Errorf("module %q: parse OpenAPI fragment: %w", m.Name(), err)
		}
		if err := mergeSpec(spec, frag, m.Name()); err != nil {
			return err
		}
	}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode merged openapi: %w", err)
	}
	a.spec = out
	return nil
}

// mergeSpec merges a fragment's "paths" and "components" into the spec. A
// duplicate path is an error rather than a silent overwrite: two modules
// claiming one path means the contract is lying about at least one of them.
func mergeSpec(spec, frag map[string]any, module string) error {
	if fragPaths, ok := frag["paths"].(map[string]any); ok {
		paths, _ := spec["paths"].(map[string]any)
		if paths == nil {
			paths = map[string]any{}
			spec["paths"] = paths
		}
		for p, v := range fragPaths {
			if _, exists := paths[p]; exists {
				return fmt.Errorf("module %q: OpenAPI path %q is already documented", module, p)
			}
			paths[p] = v
		}
	}

	fragComponents, ok := frag["components"].(map[string]any)
	if !ok {
		return nil
	}
	components, _ := spec["components"].(map[string]any)
	if components == nil {
		components = map[string]any{}
		spec["components"] = components
	}
	for section, v := range fragComponents {
		items, ok := v.(map[string]any)
		if !ok {
			continue
		}
		target, _ := components[section].(map[string]any)
		if target == nil {
			target = map[string]any{}
			components[section] = target
		}
		for name, def := range items {
			if _, exists := target[name]; exists {
				return fmt.Errorf("module %q: OpenAPI components.%s.%s is already defined", module, section, name)
			}
			target[name] = def
		}
	}
	return nil
}

// Spec returns the served OpenAPI document.
func (a *App) Spec() []byte {
	out := make([]byte, len(a.spec))
	copy(out, a.spec)
	return out
}

// SpecPaths returns the path templates the served contract documents. The
// coverage test compares it against [App.Routes] so the spec cannot silently
// drift from the code.
func (a *App) SpecPaths() ([]string, error) {
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(a.spec, &doc); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func (a *App) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(a.spec)
}

// docsPage renders the contract with Scalar, loaded from a CDN. This is the
// one place the engine references an external asset; /doc itself is fully
// self-contained, so an air-gapped deployment loses the rendered page but
// never the contract.
const docsPage = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>gocommerce API</title>
</head>
<body>
  <script id="api-reference" data-url="/doc"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>
`

func (a *App) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(docsPage))
}
