package repofacts

import (
	"testing"
)

func TestDetectAPIContracts_OpenAPI(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			"swagger.json + swagger.yaml",
			map[string]string{
				"services/api/docs/swagger.json": "{}",
				"services/api/docs/swagger.yaml": "openapi: 3.0\n",
			},
			[]string{"services/api/docs/swagger.json", "services/api/docs/swagger.yaml"},
		},
		{
			"openapi.yaml at root",
			map[string]string{"openapi.yaml": "openapi: 3.0\n"},
			[]string{"openapi.yaml"},
		},
		{
			"kubernetes-style openapi-spec layout",
			map[string]string{
				"api/openapi-spec/v1.json": "{}",
				"api/openapi-spec/v2.yaml": "openapi: 3.0\n",
			},
			[]string{"api/openapi-spec/v1.json", "api/openapi-spec/v2.yaml"},
		},
		{
			"directory named openapi is NOT a hit",
			map[string]string{
				"openapi/notes.md": "# notes\n",
			},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := Scan(mkRepo(t, c.files))
			if err != nil {
				t.Fatal(err)
			}
			got := f.APIContracts.OpenAPI
			if !stringSliceEq(got, c.want) {
				t.Errorf("OpenAPI = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDetectAPIContracts_AsyncAPI(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"asyncapi.yaml":                 "asyncapi: 2.6.0\n",
		"services/events/asyncapi.json": "{}",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"asyncapi.yaml", "services/events/asyncapi.json"}
	if !stringSliceEq(f.APIContracts.AsyncAPI, want) {
		t.Errorf("AsyncAPI = %v, want %v", f.APIContracts.AsyncAPI, want)
	}
}

func TestDetectAPIContracts_GraphQL(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"schema.graphql":               "type Query { x: Int }\n",
		"apps/web/src/schema.graphqls": "type Foo { id: ID! }\n",
		// Plain *.gql is NOT matched (could be a query, not a schema).
		"queries/get-user.gql": "{ user { id } }\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apps/web/src/schema.graphqls", "schema.graphql"}
	if !stringSliceEq(f.APIContracts.GraphQL, want) {
		t.Errorf("GraphQL = %v, want %v", f.APIContracts.GraphQL, want)
	}
}

func TestDetectAPIContracts_Protobuf(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"proto/user.proto":    `syntax = "proto3";`,
		"proto/billing.proto": `syntax = "proto3";`,
		"buf.yaml":            "version: v1\n",
		"unrelated.txt":       "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"proto/billing.proto", "proto/user.proto"}
	if !stringSliceEq(f.APIContracts.Protobuf, want) {
		t.Errorf("Protobuf = %v, want %v", f.APIContracts.Protobuf, want)
	}
	if !f.APIContracts.Tools.Buf {
		t.Errorf("expected Tools.Buf = true")
	}
}

func TestDetectAPIContracts_Avro(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"schemas/user.avsc":  `{"type":"record"}`,
		"schemas/event.avro": `{"type":"record"}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"schemas/event.avro", "schemas/user.avsc"}
	if !stringSliceEq(f.APIContracts.Avro, want) {
		t.Errorf("Avro = %v, want %v", f.APIContracts.Avro, want)
	}
}

func TestDetectAPIContracts_SwagToolMarker(t *testing.T) {
	// A docs.go importing swaggo/swag, sibling to swagger.json,
	// flips Tools.Swag. A docs.go that doesn't import swag must NOT.
	cases := []struct {
		name     string
		docsGo   string
		wantSwag bool
	}{
		{
			"docs.go imports swaggo/swag",
			"package docs\n\nimport \"github.com/swaggo/swag\"\n\nvar _ = swag.Spec{}\n",
			true,
		},
		{
			"docs.go without swag import",
			"package docs\n\n// Just documentation, no swag.\n",
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := Scan(mkRepo(t, map[string]string{
				"services/api/docs/swagger.json": "{}",
				"services/api/docs/docs.go":      c.docsGo,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if f.APIContracts.Tools.Swag != c.wantSwag {
				t.Errorf("Tools.Swag = %v, want %v", f.APIContracts.Tools.Swag, c.wantSwag)
			}
		})
	}
}

func TestDetectAPIContracts_GraphQLCodegenMarker(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"codegen.yml":    "schema: schema.graphql\n",
		"schema.graphql": "type Query { x: Int }\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !f.APIContracts.Tools.GraphQLCodegen {
		t.Errorf("codegen.yml should flag Tools.GraphQLCodegen")
	}
}

func TestDetectAPIContracts_EmptyRepo(t *testing.T) {
	f, err := Scan(mkRepo(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	// All slices nil + all tool markers false.
	if len(f.APIContracts.OpenAPI) != 0 || len(f.APIContracts.Protobuf) != 0 {
		t.Errorf("empty repo leaked api contracts: %+v", f.APIContracts)
	}
	if f.APIContracts.Tools.Buf || f.APIContracts.Tools.Swag {
		t.Errorf("empty repo leaked tool markers")
	}
}

// stringSliceEq is a tiny order-sensitive comparator. We rely on
// detectAPIContracts sorting its outputs, so the test compares
// against sorted want values too.
func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
