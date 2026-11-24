package infrastructure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// FragmentFile is the name of a route fragment in its deployable's directory (ADR 0058). The
// renderer reads <fragments>/<slug>/route-fragment.yaml for every deployable that has one.
const FragmentFile = "route-fragment.yaml"

func readFragments(dir string) ([]application.Fragment, error) {
	schema, err := fragmentSchema()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir) // sorted by name, so the fragments are sorted by slug
	if err != nil {
		return nil, err
	}
	var fragments []application.Fragment
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name(), FragmentFile))
		if errors.Is(err, fs.ErrNotExist) {
			continue // a deployable without external routes has no fragment
		}
		if err != nil {
			return nil, err
		}
		fragment, err := decodeFragment(schema, application.Slug(entry.Name()), content)
		if err != nil {
			return nil, err
		}
		fragments = append(fragments, fragment)
	}
	return fragments, nil
}

// decodeFragment checks a fragment against the fragment schema and decodes it. Every failure is a
// refusal that names the fragment.
func decodeFragment(schema *jsonschema.Schema, slug application.Slug, content []byte) (application.Fragment, error) {
	refuse := func(rule string) error { return &application.Refusal{Fragment: slug, Rule: rule} }
	var document any
	if err := yaml.Unmarshal(content, &document); err != nil {
		return application.Fragment{}, refuse("not YAML: " + err.Error())
	}
	// The schema is checked on the JSON form of the document, which is what JSON Schema describes.
	asJSON, err := json.Marshal(document)
	if err != nil {
		return application.Fragment{}, refuse("not a JSON-compatible document: " + err.Error())
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return application.Fragment{}, refuse(err.Error())
	}
	if err := schema.Validate(instance); err != nil {
		return application.Fragment{}, refuse("breaks the fragment schema: " + schemaFailure(err))
	}
	var decoded struct {
		Routes []application.FragmentRoute `yaml:"routes"`
	}
	if err := yaml.Unmarshal(content, &decoded); err != nil {
		return application.Fragment{}, refuse(err.Error())
	}
	return application.Fragment{Slug: slug, Routes: decoded.Routes}, nil
}

func fragmentSchema() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(application.FragmentSchema))
	if err != nil {
		return nil, fmt.Errorf("the embedded fragment schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("fragment.schema.json", document); err != nil {
		return nil, err
	}
	return compiler.Compile("fragment.schema.json")
}

// schemaFailure names the first place in the fragment that breaks the schema, and why: the
// description of the definition under $defs that names forbidden content, or else the library's own
// English wording.
func schemaFailure(err error) string {
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return err.Error()
	}
	output := validation.BasicOutput()
	for _, unit := range output.Errors {
		if unit.Error != nil {
			location := unit.InstanceLocation
			if location == "" {
				location = "/"
			}
			why := unit.Error.String()
			if forbidden, found := forbiddenContent(unit.AbsoluteKeywordLocation); found {
				why = forbidden
			}
			return fmt.Sprintf("at %s: %s", location, why)
		}
	}
	return err.Error()
}

// forbiddenContent is the description of the definition under the fragment schema's $defs that a
// keyword location points to, such as ...#/$defs/upstreamAddress.
func forbiddenContent(absoluteKeywordLocation string) (string, bool) {
	_, pointer, _ := strings.Cut(absoluteKeywordLocation, "#")
	name, isDefinition := strings.CutPrefix(pointer, "/$defs/")
	if !isDefinition {
		return "", false
	}
	var schema struct {
		Defs map[string]struct {
			Description string `json:"description"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(application.FragmentSchema, &schema); err != nil {
		return "", false
	}
	definition, found := schema.Defs[name]
	return definition.Description, found && definition.Description != ""
}
