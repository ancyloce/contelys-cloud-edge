package infrastructure

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// readContracts reads the paths of the OpenAPI contract of each deployable that ships a fragment.
// The contract of a domain service is the directory <dir>/<slug>/ (layout standard,
// contracts/openapi/<slug>/), and every YAML or JSON document in it that has an openapi key adds
// its paths. A deployable without such a document has no entry, so the application refuses its
// fragment unless it is Authoring's.
func readContracts(dir string, fragments []application.Fragment) (map[application.Slug][]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("the contracts directory: %w", err)
	}
	contracts := map[application.Slug][]string{}
	for _, fragment := range fragments {
		contractDir := filepath.Join(dir, string(fragment.Slug))
		entries, err := os.ReadDir(contractDir) // sorted by name
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		paths := map[string]bool{}
		found := false
		for _, entry := range entries {
			decode := yaml.Unmarshal
			switch filepath.Ext(entry.Name()) {
			case ".yaml", ".yml":
			case ".json":
				decode = json.Unmarshal
			default:
				continue
			}
			name := filepath.Join(contractDir, entry.Name())
			content, err := os.ReadFile(name)
			if err != nil {
				return nil, err
			}
			var document struct {
				OpenAPI string         `yaml:"openapi" json:"openapi"`
				Paths   map[string]any `yaml:"paths" json:"paths"`
			}
			if err := decode(content, &document); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if document.OpenAPI == "" {
				continue // not an OpenAPI document, such as a file of shared schemas
			}
			found = true
			for path := range document.Paths {
				paths[path] = true
			}
		}
		if found {
			contracts[fragment.Slug] = slices.Sorted(maps.Keys(paths))
		}
	}
	return contracts, nil
}
