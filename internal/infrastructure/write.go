package infrastructure

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// Write implements application.Sink. It encodes and checks every file before it writes any, so a
// file that breaks a rule leaves the current ones in place. Each file is written beside the current
// one, and only when all of them are written are they renamed into place, so that APISIX never reads
// a half-written file and a failure while writing replaces none. Until every new file is in place,
// each current file keeps a second name, so that a failed rename puts back the files that it already
// replaced. apisix.yaml holds keys, so only its owner may read it.
func (Files) Write(dir string, out application.Output) error {
	return write(dir, out, os.Rename)
}

// replacement is one file that write puts into place: its path, the new file beside it, and the
// current file's second name, which is empty when there is no current file.
type replacement struct {
	path, temporary, previous string
}

// write is Write with the rename that puts each new file into place.
func write(dir string, out application.Output, rename func(from, to string) error) error {
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

	var replacements []replacement
	defer func() {
		for _, r := range replacements {
			os.Remove(r.temporary) // a renamed one is gone already
			if r.previous != "" {
				os.Remove(r.previous) // a restored one is gone already
			}
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
		replacements = append(replacements, replacement{path: f.path, temporary: temporary})
		if replacements[len(replacements)-1].previous, err = keepCurrent(f.path); err != nil {
			return err
		}
	}
	for i, r := range replacements {
		if err := rename(r.temporary, r.path); err != nil {
			return errors.Join(err, restore(replacements[:i], rename))
		}
	}
	return nil
}

// keepCurrent gives the current file at path a second name beside it, a hard link, so that the file
// is never missing, and returns that name, or "" when there is no current file.
func keepCurrent(path string) (string, error) {
	previous := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".previous")
	if err := os.Remove(previous); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err // one that a killed renderer left behind
	}
	if err := os.Link(path, previous); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("keep the current %s: %w", path, err)
	}
	return previous, nil
}

// restore undoes the renames of replaced: each current file returns under its own name, with its
// own modification time, and a file that had no current one is removed.
func restore(replaced []replacement, rename func(from, to string) error) error {
	var errs []error
	for _, r := range replaced {
		if r.previous == "" {
			errs = append(errs, os.Remove(r.path))
		} else {
			errs = append(errs, rename(r.previous, r.path))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("put back the files already replaced: %w", err)
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
