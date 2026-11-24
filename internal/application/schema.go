package application

import _ "embed"

// FragmentSchema is the JSON Schema of a route fragment. It refuses an unknown field or value.
//
//go:embed fragment.schema.json
var FragmentSchema []byte
