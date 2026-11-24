package infrastructure

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// Write implements application.Sink. It encodes and checks every file before it writes any, so a
// file that breaks a rule leaves the current ones in place. Each file is written beside the current
// one, and only when all of them are written are they renamed into place, so that APISIX never reads
// a half-written file and a failure while writing replaces none. apisix.yaml holds keys, so only its
// owner may read it.
func (Files) Write(dir string, out application.Output) (err error) {
	type file struct {
		path    string
		content []byte
		mode    os.FileMode
	}
	var files []file
	for _, instance := range out.Instances {
		content, err := encodeRouteFile(routeFileName(instance.Address), instance.File)
		if err != nil {
			return err
		}
		files = append(files, file{filepath.Join(dir, instance.Address, "apisix.yaml"), content, 0o600})
	}
	config, err := encodeConfigFile(out.Config)
	if err != nil {
		return err
	}
	files = append(files, file{filepath.Join(dir, "config.yaml"), config, 0o644})

	var temporaries []string
	defer func() {
		for _, temporary := range temporaries {
			os.Remove(temporary) // a renamed one is gone already
		}
	}()
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return err
		}
		temporary, err := writeBeside(f.path, f.content, f.mode)
		if err != nil {
			return err
		}
		temporaries = append(temporaries, temporary)
	}
	for i, f := range files {
		if err := os.Rename(temporaries[i], f.path); err != nil {
			return err
		}
	}
	return nil
}

// endMarker closes every apisix.yaml: APISIX reads a file only if it ends with it.
const endMarker = "#END\n"

// routeFileName names the apisix.yaml of the Edge instance at address in a failure.
func routeFileName(address string) string {
	return "the apisix.yaml of " + address
}

// encodeRouteFile encodes an apisix.yaml, which name describes in a failure, and checks its bytes.
func encodeRouteFile(name string, file application.ApisixFile) ([]byte, error) {
	content, err := encode(file)
	if err != nil {
		return nil, err
	}
	content = append(content, endMarker...)
	return content, checkRouteFile(name, content)
}

// encodeConfigFile encodes config.yaml and checks its bytes.
func encodeConfigFile(config application.ConfigFile) ([]byte, error) {
	content, err := encode(config)
	if err != nil {
		return nil, err
	}
	return content, checkReferences("config.yaml", content)
}

// endMarkerAtTheEnd is how APISIX finds the marker: in the last 10 bytes of the file, followed by
// nothing but white space.
var endMarkerAtTheEnd = regexp.MustCompile(`#END\s*$`)

// checkRouteFile applies the rules of section 03 to the bytes of an apisix.yaml: it holds no
// reference to an environment variable, and it ends with the #END marker, without which APISIX
// never applies it.
func checkRouteFile(name string, content []byte) error {
	if err := checkReferences(name, content); err != nil {
		return err
	}
	if !endMarkerAtTheEnd.Match(content[max(0, len(content)-10):]) {
		return fmt.Errorf("%s does not end with the #END marker, without which APISIX never applies it", name)
	}
	return nil
}

// checkReferences refuses the sequence ${{ anywhere in a file. APISIX reads it as a reference to an
// environment variable, in apisix.yaml and config.yaml alike, and refuses the whole file when the
// variable is not set.
func checkReferences(name string, content []byte) error {
	if bytes.Contains(content, []byte("${{")) {
		return fmt.Errorf("%s contains ${{, which APISIX reads as a reference to an environment variable", name)
	}
	return nil
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

// writeBeside writes content into a new file in the directory of path, with the given mode and
// synced to disk, and returns the new file's path for the rename into place.
func writeBeside(path string, content []byte, mode os.FileMode) (_ string, err error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.Remove(temporary.Name())
		}
	}()
	if _, err = temporary.Write(content); err != nil {
		temporary.Close()
		return "", err
	}
	if err = temporary.Chmod(mode); err != nil {
		temporary.Close()
		return "", err
	}
	if err = temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err = temporary.Close(); err != nil {
		return "", err
	}
	return temporary.Name(), nil
}
