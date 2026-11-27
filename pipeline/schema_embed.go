package pipeline

import _ "embed"

// Schema is pipeline.schema.json, the published JSON Schema of a pipeline file.
//
//go:embed pipeline.schema.json
var Schema []byte
