package infrastructure

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/ancyloce/contelys-cloud-edge/internal/application"
)

// ContractFile is the OpenAPI contract of one domain service, in its directory of the shared
// contracts: contracts/openapi/<slug>/openapi.yaml, in OpenAPI 3.0.3 (layout standard).
const ContractFile = "openapi.yaml"

// readContracts reads the paths of the OpenAPI contract of each deployable that ships a fragment,
// from <dir>/<slug>/openapi.yaml. A deployable without that file has no entry, so the application
// refuses its fragment unless it is Authoring's.
func readContracts(dir string, fragments []application.Fragment) (map[application.Slug][]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("the contracts directory: %w", err)
	}
	contracts := map[application.Slug][]string{}
	for _, fragment := range fragments {
		name := filepath.Join(dir, string(fragment.Slug), ContractFile)
		content, err := os.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var document struct {
			Paths map[string]any `yaml:"paths"`
		}
		if err := yaml.Unmarshal(content, &document); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		contracts[fragment.Slug] = slices.Sorted(maps.Keys(document.Paths))
	}
	return contracts, nil
}
