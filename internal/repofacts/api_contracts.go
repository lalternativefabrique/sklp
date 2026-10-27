package repofacts

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// detectAPIContracts walks the pre-computed file list and bins every
// API description into the right APIContracts bucket. The detection
// is purely filename / extension based — we do NOT validate that a
// swagger.json is actually a swagger document, for example. False
// positives are acceptable here because:
//
//   - The cost of a wrong "you have an OpenAPI contract" line in the
//     LLM prompt is one wasted CI step the operator can delete.
//   - The cost of a wrong "you have NO OpenAPI" omission is a missed
//     breaking-change check, which is a real regression.
//
// We err on the side of inclusion. Tests pin the exact patterns so a
// future refactor can't quietly drop one.
func detectAPIContracts(files []string, root string) APIContracts {
	var c APIContracts
	for _, p := range files {
		base := filepath.Base(p)
		baseLower := strings.ToLower(base)
		r := rel(root, p)

		switch {
		// ── OpenAPI ─────────────────────────────────────────
		case isOpenAPIFile(base, baseLower, r):
			c.OpenAPI = append(c.OpenAPI, r)

		// ── AsyncAPI ────────────────────────────────────────
		case isAsyncAPIFile(baseLower):
			c.AsyncAPI = append(c.AsyncAPI, r)

		// ── GraphQL ─────────────────────────────────────────
		case isGraphQLFile(baseLower):
			c.GraphQL = append(c.GraphQL, r)

		// ── Protobuf ────────────────────────────────────────
		case strings.HasSuffix(baseLower, ".proto"):
			c.Protobuf = append(c.Protobuf, r)

		// ── Avro ────────────────────────────────────────────
		case strings.HasSuffix(baseLower, ".avsc"), strings.HasSuffix(baseLower, ".avro"):
			c.Avro = append(c.Avro, r)

		// ── Tools markers (boolean flags, not paths) ────────
		case base == "buf.yaml" || base == "buf.gen.yaml":
			c.Tools.Buf = true
		case base == "codegen.yml" || base == "codegen.yaml" ||
			base == "codegen.ts" || base == "codegen.js":
			c.Tools.GraphQLCodegen = true
		}
	}

	// Swag detection is content-based, not filename-based: any docs.go
	// importing github.com/swaggo/swag flags the tool. Run it as a
	// second pass over the OpenAPI hits so we don't grep every file in
	// the repo — swag projects always have a docs.go sibling to their
	// swagger.json.
	if len(c.OpenAPI) > 0 {
		c.Tools.Swag = detectSwagToolMarker(files, root, c.OpenAPI)
	}

	// Deterministic ordering for cache-friendly JSON output.
	sort.Strings(c.OpenAPI)
	sort.Strings(c.AsyncAPI)
	sort.Strings(c.GraphQL)
	sort.Strings(c.Protobuf)
	sort.Strings(c.Avro)
	return c
}

// isOpenAPIFile matches the conventional file shapes for an OpenAPI
// / Swagger document. We accept both the modern "openapi.*" naming
// and the legacy "swagger.*" so we don't have to know whether the
// repo migrated to OpenAPI 3.
//
// Special case: the Kubernetes convention places the spec under
// `api/openapi-spec/`. We accept any *.json / *.yaml / *.yml inside
// such a directory.
func isOpenAPIFile(base, baseLower, relPath string) bool {
	switch baseLower {
	case "openapi.json", "openapi.yaml", "openapi.yml",
		"swagger.json", "swagger.yaml", "swagger.yml":
		return true
	}
	// api/openapi-spec/<file>.<json|yaml|yml>
	if strings.Contains(filepath.ToSlash(relPath), "/openapi-spec/") {
		switch filepath.Ext(baseLower) {
		case ".json", ".yaml", ".yml":
			return true
		}
	}
	// Defensive: a literal "openapi" with no extension is unusual but
	// has been seen. We don't accept it without an extension — too
	// many false positives on directories named "openapi".
	_ = base
	return false
}

// isAsyncAPIFile matches the canonical asyncapi.{ext} naming. The
// spec is much younger than OpenAPI's so we don't have a legacy alias
// to worry about.
func isAsyncAPIFile(baseLower string) bool {
	switch baseLower {
	case "asyncapi.json", "asyncapi.yaml", "asyncapi.yml":
		return true
	}
	return false
}

// isGraphQLFile matches *.graphql and *.graphqls files (the schema
// extensions), plus the conventional schema.graphql / schema.gql.
// We do NOT match plain *.gql because that extension is widely used
// for query files (operations), not schema definitions.
func isGraphQLFile(baseLower string) bool {
	if strings.HasSuffix(baseLower, ".graphql") || strings.HasSuffix(baseLower, ".graphqls") {
		return true
	}
	switch baseLower {
	case "schema.gql":
		return true
	}
	return false
}

// detectSwagToolMarker returns true when there is a docs.go file
// sitting in any directory that already produced an OpenAPI hit AND
// that docs.go imports github.com/swaggo/swag. The check is content-
// based because the filename alone (docs.go) is too generic — the
// swag toolchain is identified by its package import.
func detectSwagToolMarker(files []string, root string, openAPIRelPaths []string) bool {
	// Map openAPI dirs for O(1) lookup.
	openAPIDirs := make(map[string]bool, len(openAPIRelPaths))
	for _, p := range openAPIRelPaths {
		openAPIDirs[filepath.Dir(p)] = true
	}
	for _, p := range files {
		if filepath.Base(p) != "docs.go" {
			continue
		}
		dir := rel(root, filepath.Dir(p))
		if !openAPIDirs[dir] {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), `"github.com/swaggo/swag"`) {
			return true
		}
	}
	return false
}
