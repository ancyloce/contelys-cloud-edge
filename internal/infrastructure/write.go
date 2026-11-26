package infrastructure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ancyloce/contelys-cloud-edge/internal/application"
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
		// polled marks an apisix.yaml, whose modification time APISIX checks.
		polled bool
	}
	var files []file
	var polled []string
	for _, instance := range out.Instances {
		content, err := encodeRouteFile(routeFileName(instance.Address), instance.File)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, instance.Address, "apisix.yaml")
		files = append(files, file{path, content, 0o600, true})
		polled = append(polled, path)
	}
	config, err := encodeConfigFile(out.Config)
	if err != nil {
		return err
	}
	files = append(files, file{filepath.Join(dir, "config.yaml"), config, 0o644, false})
	stamp, err := laterSecond(polled)
	if err != nil {
		return err
	}

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
		if f.polled {
			if err := os.Chtimes(temporary, stamp, stamp); err != nil {
				return err
			}
		}
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

// laterSecond waits until the clock has passed the second of the latest modification time of the
// current files at paths, and returns the time that the new files take. APISIX compares the
// modification time of apisix.yaml at a resolution of one second and never applies a file whose
// second it has already loaded, so a replacement within that second waits for the next one instead
// of being lost (section 03). A file system may stamp a new file a little behind the clock, so the
// new files take this time explicitly.
func laterSecond(paths []string) (time.Time, error) {
	var latest time.Time
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	if wait := time.Until(latest.Truncate(time.Second).Add(time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	return time.Now(), nil
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

// encodeRouteFile encodes an apisix.yaml, which name describes in a failure, puts the digest of the
// file into the answer of its fixed-response route, and checks its bytes.
func encodeRouteFile(name string, file application.ApisixFile) ([]byte, error) {
	content, err := encode(file)
	if err != nil {
		return nil, err
	}
	content, err = digested(name, append(content, endMarker...))
	if err != nil {
		return nil, err
	}
	return content, checkRouteFile(name, content)
}

// digested replaces the placeholder of the digest with the SHA-256 of the file as it reads with the
// placeholder, which is how section 03 defines the digest of a file that carries its own digest.
func digested(name string, content []byte) ([]byte, error) {
	placeholder := []byte(application.DigestPlaceholder)
	if n := bytes.Count(content, placeholder); n != 1 {
		return nil, fmt.Errorf("%s holds the placeholder of its digest %d times, and the fixed-response route needs it once", name, n)
	}
	return bytes.Replace(content, placeholder, []byte(digestOf(content)), 1), nil
}

// digestOf is the digest of a file that holds the placeholder in the digest's place.
func digestOf(withPlaceholder []byte) string {
	sum := sha256.Sum256(withPlaceholder)
	return application.DigestPrefix + hex.EncodeToString(sum[:])
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
