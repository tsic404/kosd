package extension

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxArtifactBytes bounds one package file a directory source reads. The
// archive is untrusted input, so an oversized file is refused before it is
// loaded into memory.
const maxArtifactBytes int64 = 256 << 20

// DirectoryArtifacts serves extension packages from one directory: the
// reference a component names is the package's path below Root. The deployable
// control plane mounts that directory, so an operator delivers a package by
// dropping the file in it (C-17, KOSD-NG-7).
type DirectoryArtifacts struct {
	// Root is the directory package files are read from.
	Root string
}

// Fetch returns the bytes of the file the reference names. A missing or
// oversized artifact is an error: the component cannot install without it.
func (d DirectoryArtifacts) Fetch(_ context.Context, reference string) ([]byte, error) {
	if strings.TrimSpace(d.Root) == "" {
		return nil, errors.New("no package directory is configured")
	}
	file, err := d.file(reference)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("artifact %q is not available under %s", reference, d.Root)
		}
		return nil, fmt.Errorf("read artifact %q: %w", reference, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact %q is not a regular file", reference)
	}
	if info.Size() > maxArtifactBytes {
		return nil, fmt.Errorf("artifact %q exceeds the %d byte limit", reference, maxArtifactBytes)
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read artifact %q: %w", reference, err)
	}
	return content, nil
}

// file resolves one reference inside Root. A component names a clean relative
// path; an absolute path or one that climbs out of Root is refused, so a
// component can only read a package the directory holds.
func (d DirectoryArtifacts) file(reference string) (string, error) {
	name := path.Clean(strings.TrimSpace(reference))
	if name == "." || name == ".." || strings.HasPrefix(name, "../") ||
		path.IsAbs(name) || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("artifact reference %q is not a path inside the package directory", reference)
	}
	return filepath.Join(d.Root, filepath.FromSlash(name)), nil
}
