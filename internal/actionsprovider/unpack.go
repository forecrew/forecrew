package actionsprovider

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/forecrew/forecrew/internal/domain"
)

// Limits of the artifact of a run. The zip is data the repo's workflow
// produced, so it is never trusted: a small zip can unpack to gigabytes.
const (
	// MaxFiles is the most files taken from one artifact; the rest are
	// ignored, like the worker's upload limit.
	MaxFiles = 200
	// MaxUnpacked is the most bytes one artifact may unpack to; more fails
	// the unpacking.
	MaxUnpacked = 128 << 20
)

// file is one regular file of an artifact.
type file struct {
	Name string
	Data []byte
}

// unpacked is what unpack found in an artifact.
type unpacked struct {
	Files []file
	// Skipped says, per refused entry, why: symlinks, names that are not
	// valid artifact names (which covers absolute paths, ".." and "\"),
	// duplicates and files over maxFile.
	Skipped []string
}

// errUnpack means the archive is unusable or too large.
var errUnpack = errors.New("unusable artifact")

// unpack reads the regular files of a zip: at most MaxFiles files of at
// most maxFile bytes each and budget bytes together. Directories are
// ignored; symlinks, entries whose name domain.ValidArtifactName refuses
// (path traversal included) and duplicates are skipped; a zip that unpacks
// to more than the budget fails with errUnpack. Nothing is written to disk.
func unpack(data []byte, maxFile, budget int64) (unpacked, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return unpacked{}, fmt.Errorf("%w: %w", errUnpack, err)
	}
	var out unpacked
	seen := map[string]bool{}
	total := budget
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode.IsDir():
			continue
		case mode&fs.ModeSymlink != 0 || !mode.IsRegular():
			out.Skipped = append(out.Skipped, f.Name+": not a regular file")
			continue
		case !domain.ValidArtifactName(f.Name):
			out.Skipped = append(out.Skipped, f.Name+": not a valid artifact name")
			continue
		case seen[f.Name]:
			out.Skipped = append(out.Skipped, f.Name+": duplicate")
			continue
		case f.UncompressedSize64 > uint64(maxFile): // #nosec G115 -- maxFile is a positive limit
			out.Skipped = append(out.Skipped, f.Name+": over the artifact size limit")
			continue
		case len(out.Files) >= MaxFiles:
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: more than %d files", f.Name, MaxFiles))
			continue
		}
		seen[f.Name] = true
		body, err := readEntry(f, min(maxFile, budget))
		switch {
		case errors.Is(err, errTooBig):
			if budget < maxFile {
				return unpacked{}, fmt.Errorf("%w: unpacks to more than %d bytes", errUnpack, total)
			}
			out.Skipped = append(out.Skipped, f.Name+": over the artifact size limit")
			continue
		case err != nil:
			return unpacked{}, fmt.Errorf("%w: %s: %w", errUnpack, f.Name, err)
		}
		budget -= int64(len(body))
		out.Files = append(out.Files, file{Name: f.Name, Data: body})
	}
	return out, nil
}

var errTooBig = errors.New("entry is larger than it says")

// readEntry reads a zip entry of at most limit bytes, whatever its header
// claims.
func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooBig
	}
	return data, nil
}
