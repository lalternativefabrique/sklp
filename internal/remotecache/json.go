package remotecache

import "encoding/json"

// Tiny shims so the fake doesn't have to import encoding/json directly
// (keeps the wire format change in one place if we ever swap encoder).
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
