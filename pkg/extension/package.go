// Package extension implements the extension interface: package digests, the
// declaration, contribution loading, dependency activation and the visibility
// projection (C-17 to C-20, C-28).
//
// The kernel never identifies an extension by name: an install request names a
// package artifact, the artifact carries the declaration, and every visible
// contribution comes from that declaration (C-17). One artifact is a gzipped tar
// carrying the declaration under manifest.json, its UI assets under assets/, and
// the digest over both under "digest".
package extension

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// The package structure is fixed: the declaration, the UI assets it names and
// the digest over both, nothing else (C-17).
const (
	// ManifestEntry is the declaration's entry name.
	ManifestEntry = "manifest.json"
	// DigestEntry is the digest's entry name.
	DigestEntry = "digest"
	// AssetPrefix is the namespace UI assets live under.
	AssetPrefix = "assets/"
)

const (
	// maxEntries bounds how many entries one package may carry.
	maxEntries = 4096
	// maxUncompressedBytes bounds how much content one package may materialize.
	// A package is untrusted input, so the budget is checked against the header
	// before any content is read.
	maxUncompressedBytes int64 = 256 << 20
	// digestPrefix is the one digest form an install request names (C-17).
	digestPrefix = "sha256:"
)

// epoch pins archive timestamps so the same input always yields the same bytes.
var epoch = time.Unix(0, 0).UTC()

// Asset is one UI asset file. Path is the package-relative entry name and must
// be a clean path under AssetPrefix (KOSD-C-73).
type Asset struct {
	Path    string
	Content []byte
}

// Artifact is the verified content of one package: the declaration the kernel
// loads and the UI assets the digest covers.
type Artifact struct {
	// Declaration is the decoded manifest.json.
	Declaration Declaration
	// Assets are the UI asset files carried under AssetPrefix.
	Assets []Asset
	// Digest is the package digest in its "sha256:<hex>" wire form.
	Digest string
}

// Package is one extension package artifact and the digest the install request
// binds it to (C-17).
type Package struct {
	// Reference locates the artifact; it is the component spec's value.
	Reference string
	// Digest is the sha256 the artifact must match.
	Digest string
	// Content is the fetched artifact: the package archive.
	Content []byte
}

// Verify reads the package and reports whether its content matches the named
// digest. A missing or malformed digest is a mismatch: an install request has to
// name one (C-17).
func (p Package) Verify() (Artifact, error) {
	named, ok := parseDigest(p.Digest)
	if !ok {
		return Artifact{}, fmt.Errorf("package digest %q is not sha256:<hex>", p.Digest)
	}
	artifact, err := Read(p.Content)
	if err != nil {
		return Artifact{}, err
	}
	if named != artifact.Digest {
		return Artifact{}, fmt.Errorf("package artifact is %s, the request names %s", artifact.Digest, named)
	}
	return artifact, nil
}

// ErrDeclaration marks a package whose manifest is not a valid declaration.
var ErrDeclaration = errors.New("package declaration is invalid")

// Read decodes a package archive and verifies the digest entry covers the
// declaration and every UI asset exactly as carried. It does not judge the
// declaration's content: that is Load's job, class by class (C-28).
func Read(content []byte) (Artifact, error) {
	manifest, carried, err := readEntries(content)
	if err != nil {
		return Artifact{}, err
	}
	digest := DigestOf(manifest, carried)
	declaration, err := Parse(manifest)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrDeclaration, err)
	}
	return Artifact{Declaration: declaration, Assets: carried, Digest: digest}, nil
}

// DigestOf returns the package digest over the declaration and every asset.
// Each record is framed with its name and length, so a renamed entry or a
// shifted boundary changes the digest (KOSD-C-17).
func DigestOf(manifest []byte, assets []Asset) string {
	ordered := sortedAssets(assets)
	hasher := sha256.New()
	addRecord(hasher, ManifestEntry, manifest)
	for _, asset := range ordered {
		addRecord(hasher, asset.Path, asset.Content)
	}
	return digestPrefix + hex.EncodeToString(hasher.Sum(nil))
}

func addRecord(hasher io.Writer, name string, content []byte) {
	fmt.Fprintf(hasher, "%s %d\n", name, len(content))
	hasher.Write(content)
}

// Pack builds one package archive and returns its digest. The archive carries
// exactly the declaration and the assets the caller supplies (C-17).
func Pack(declaration Declaration, assets []Asset) ([]byte, string, error) {
	manifest, err := json.Marshal(declaration)
	if err != nil {
		return nil, "", fmt.Errorf("encode declaration: %w", err)
	}
	for index, asset := range assets {
		if !isAssetPath(asset.Path) {
			return nil, "", fmt.Errorf("asset path %q must be a clean path under %q", asset.Path, AssetPrefix)
		}
		if hasAsset(assets[:index], asset.Path) {
			return nil, "", fmt.Errorf("asset path %q is declared twice", asset.Path)
		}
	}
	ordered := sortedAssets(assets)
	digest := DigestOf(manifest, ordered)
	archive, err := writeArchive(manifest, ordered, digest)
	if err != nil {
		return nil, "", fmt.Errorf("write package archive: %w", err)
	}
	return archive, digest, nil
}

func hasAsset(assets []Asset, name string) bool {
	for _, asset := range assets {
		if asset.Path == name {
			return true
		}
	}
	return false
}

func sortedAssets(assets []Asset) []Asset {
	ordered := append([]Asset(nil), assets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	return ordered
}

// isAssetPath reports whether name is a clean path inside the asset namespace.
// A clean path keeps the digest framing unambiguous: the name carries no
// newline, and no "." or ".." segment can escape the namespace.
func isAssetPath(name string) bool {
	return strings.HasPrefix(name, AssetPrefix) &&
		len(name) > len(AssetPrefix) &&
		path.Clean(name) == name &&
		!strings.ContainsAny(name, "\x00\n\r")
}

func writeArchive(manifest []byte, assets []Asset, digest string) ([]byte, error) {
	var buffer bytes.Buffer
	compressed, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	compressed.Header.ModTime = epoch
	compressed.Header.OS = 255

	archive := tar.NewWriter(compressed)
	if err := writeEntry(archive, ManifestEntry, manifest); err != nil {
		return nil, err
	}
	for _, asset := range assets {
		if err := writeEntry(archive, asset.Path, asset.Content); err != nil {
			return nil, err
		}
	}
	if err := writeEntry(archive, DigestEntry, []byte(digest+"\n")); err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeEntry(archive *tar.Writer, name string, content []byte) error {
	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(content)),
		ModTime:  epoch,
	}
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	_, err := archive.Write(content)
	return err
}

// readEntries walks one archive and returns the declaration it carries and its
// assets. Entries outside the fixed structure are refused rather than ignored:
// an artifact the kernel does not understand is not installed (C-17).
func readEntries(content []byte) ([]byte, []Asset, error) {
	decompressed, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, nil, fmt.Errorf("package is not a gzip archive: %w", err)
	}
	defer decompressed.Close()

	var (
		manifest    []byte
		digestText  string
		hasManifest bool
		hasDigest   bool
		assets      []Asset
		entries     int
		remaining   = maxUncompressedBytes
	)
	seen := map[string]bool{}
	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("package archive is malformed: %w", err)
		}
		entries++
		if entries > maxEntries {
			return nil, nil, fmt.Errorf("package carries more than %d entries", maxEntries)
		}
		if seen[header.Name] {
			return nil, nil, fmt.Errorf("package carries entry %q twice", header.Name)
		}
		seen[header.Name] = true
		if header.Typeflag != tar.TypeReg {
			return nil, nil, fmt.Errorf("package entry %q is not a regular file", header.Name)
		}
		// The header states the size, so an entry that would overspend the
		// budget is refused before any content is materialized.
		if header.Size > remaining {
			return nil, nil, fmt.Errorf("package content exceeds the %d byte budget", maxUncompressedBytes)
		}
		carried, err := io.ReadAll(reader)
		if err != nil {
			return nil, nil, fmt.Errorf("read package entry %q: %w", header.Name, err)
		}
		remaining -= int64(len(carried))

		switch {
		case header.Name == ManifestEntry:
			manifest, hasManifest = carried, true
		case header.Name == DigestEntry:
			digestText, hasDigest = string(carried), true
		case isAssetPath(header.Name):
			assets = append(assets, Asset{Path: header.Name, Content: carried})
		default:
			return nil, nil, fmt.Errorf("package entry %q is outside the package structure", header.Name)
		}
	}
	if !hasManifest || !hasDigest {
		return nil, nil, errors.New("package carries no manifest.json or digest entry")
	}
	carried := DigestOf(manifest, assets)
	named, ok := parseDigest(digestText)
	if !ok {
		return nil, nil, fmt.Errorf("package digest entry %q is not sha256:<hex>", strings.TrimSpace(digestText))
	}
	if named != carried {
		return nil, nil, fmt.Errorf("package content is %s, its digest entry names %s", carried, named)
	}
	return manifest, assets, nil
}

// parseDigest accepts the wire form in any case and returns its canonical
// lowercase spelling, so a promised digest and a carried digest compare equal.
func parseDigest(text string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(text))
	sum, found := strings.CutPrefix(value, digestPrefix)
	if !found || len(sum) != 2*sha256.Size {
		return "", false
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", false
	}
	return digestPrefix + sum, true
}

// carriedApps keeps the applications whose icons point at an origin or at an
// asset this package carries, and reports the in-package icons the artifact does
// not carry (KOSD-C-73).
func carriedApps(apps []App, assets []Asset) ([]App, []string) {
	carried := make(map[string]bool, len(assets))
	for _, asset := range assets {
		carried[asset.Path] = true
	}
	kept := make([]App, 0, len(apps))
	problems := []string{}
	for _, app := range apps {
		if strings.HasPrefix(app.Icon, AssetPrefix) && !carried[app.Icon] {
			problems = append(problems, fmt.Sprintf("app %s: icon %s is not carried by the package", app.ID, app.Icon))
			continue
		}
		kept = append(kept, app)
	}
	return kept, problems
}

// ArtifactSource resolves the reference a component names to the artifact's
// bytes (C-17).
type ArtifactSource interface {
	// Fetch returns the artifact a reference names. A missing artifact is an
	// error: the component cannot install without it.
	Fetch(ctx context.Context, reference string) ([]byte, error)
}

// MemoryArtifacts is an in-memory artifact source: it serves the artifacts a
// process already holds, which is how an install request is resolved before a
// registry client exists.
type MemoryArtifacts map[string][]byte

// Fetch returns the artifact stored under reference.
func (m MemoryArtifacts) Fetch(_ context.Context, reference string) ([]byte, error) {
	content, ok := m[reference]
	if !ok {
		return nil, fmt.Errorf("artifact %q is not available", reference)
	}
	return content, nil
}
