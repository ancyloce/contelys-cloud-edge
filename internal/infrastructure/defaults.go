// Package infrastructure reads the renderer's input files, checks a route fragment against its
// schema, and writes the rendered files as YAML.
package infrastructure

import (
	_ "embed"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// defaultsFile holds every value that the renderer needs (Edge ADR 0002). It is embedded, so that
// the values belong to one version of Edge.
//
//go:embed defaults.yaml
var defaultsFile []byte

// Defaults decodes the embedded defaults file.
func Defaults() (application.Defaults, error) {
	var defaults application.Defaults
	if err := yaml.Unmarshal(defaultsFile, &defaults); err != nil {
		return application.Defaults{}, fmt.Errorf("the embedded defaults file: %w", err)
	}
	return defaults, nil
}
