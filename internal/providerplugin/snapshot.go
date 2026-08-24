package providerplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type bundleFile struct {
	Path   string
	Size   int64
	Mode   os.FileMode
	SHA256 string
}

type snapshotResult struct {
	Digest         string
	Files          []bundleFile
	TotalBytes     int64
	FileCount      int
	DirectoryCount int
}

// canonicalBundleDigest commits to the normalized path, file mode, size, and
// already-verified content hash of every file. It deliberately does not
// reopen pathnames after snapshotting.
func canonicalBundleDigest(files []bundleFile) string {
	files = slices.Clone(files)
	slices.SortFunc(files, func(a, b bundleFile) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	for _, entry := range files {
		_, _ = h.Write([]byte("file\x00"))
		_, _ = h.Write([]byte(entry.Path))
		_, _ = h.Write([]byte("\x00"))
		_, _ = h.Write([]byte(strconv.FormatUint(uint64(entry.Mode.Perm()), 8)))
		_, _ = h.Write([]byte("\x00"))
		_, _ = h.Write([]byte(strconv.FormatInt(entry.Size, 10)))
		_, _ = h.Write([]byte("\x00"))
		_, _ = h.Write([]byte(entry.SHA256))
		_, _ = h.Write([]byte("\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func normalizedFileMode(os.FileMode) os.FileMode { return 0o600 }

func validEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) && len(name) <= MaxRelativePathBytes &&
		!strings.ContainsAny(name, "/\\:\x00")
}

func validRelativePath(path string, depth int) bool {
	return path != "" && len(path) <= MaxRelativePathBytes && depth <= MaxBundleDepth
}
