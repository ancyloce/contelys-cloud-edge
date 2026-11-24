package infrastructure

import (
	"bytes"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// Write implements application.Sink. Each file is written beside the current one and renamed into
// place, so that APISIX never reads a half-written file. apisix.yaml holds keys, so only its owner
// may read it.
func (Files) Write(dir string, out application.Output) error {
	for _, instance := range out.Instances {
		content, err := encode(instance.File)
		if err != nil {
			return err
		}
		// APISIX reads a file only if it ends with this marker.
		content = append(content, "#END\n"...)
		instanceDir := filepath.Join(dir, instance.Address)
		if err := os.MkdirAll(instanceDir, 0o755); err != nil {
			return err
		}
		if err := writeAtomically(filepath.Join(instanceDir, "apisix.yaml"), content, 0o600); err != nil {
			return err
		}
	}
	content, err := encode(out.Config)
	if err != nil {
		return err
	}
	return writeAtomically(filepath.Join(dir, "config.yaml"), content, 0o644)
}

func encode(v any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeAtomically(path string, content []byte, mode os.FileMode) (err error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(temporary.Name())
		}
	}()
	if _, err = temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
