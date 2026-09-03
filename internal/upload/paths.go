// Package upload stages files uploaded over HTTP and commits them into a
// library root.
//
// The write direction has no equivalent of internal/fs.Resolver, which is
// read-only, so this package owns its own validation. Two properties matter
// more than the rest:
//
//   - Staging happens INSIDE the target root, under a dot-directory the
//     scanner skips (manifest.shouldSkipDir returns true for any "."-prefixed
//     name and the walker returns SkipDir before it upserts a folder row). That
//     makes commit a same-filesystem rename. The obvious alternative — staging
//     under the data dir — is a cross-device copy wherever the library is a
//     separate mount, which is the norm (the reference deployment has its data
//     dir on the root disk and its library on a B2 FUSE mount).
//
//   - A staged file NEVER carries an audio extension. ".part" is not in
//     manifest.Ext, so even a staging path that somehow got walked could not be
//     enqueued for extraction. A truncated file reaching the library has bitten
//     this codebase before (internal/analyze/decode.go).
package upload

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

const (
	// StagingDirName is the per-root staging directory. The leading dot is
	// load-bearing, not cosmetic — see the package doc.
	StagingDirName = ".bridge-upload"

	// PartSuffix is appended to every staged file.
	PartSuffix = ".part"

	// maxSegmentBytes caps one path segment. It mirrors transcode's
	// fsBasenameCap reasoning: a committed file may later grow a sidecar
	// whose name extends this one, so the cap leaves room rather than
	// spending the full 255.
	maxSegmentBytes = 200

	// maxRelPathBytes caps the whole relative path. Windows resolves paths
	// against MAX_PATH (260) unless long paths are enabled, and the root
	// prefix consumes part of that budget.
	maxRelPathBytes = 1024

	// maxDepth bounds how deep a client may build. Real libraries are
	// Artist/Album/Disc/Track at their deepest.
	maxDepth = 16
)

// ErrInvalidPath is the sentinel every path rejection wraps.
var ErrInvalidPath = errors.New("invalid path")

// windowsReserved are the DOS device names. They are refused on every host,
// not just Windows: a library synced or restored onto Windows would otherwise
// carry a file that cannot be opened there, and the bridge is explicitly
// cross-platform.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// companionExts are the non-audio files accepted alongside audio.
//
// The principle: a file that sits BESIDE audio in a curated library is
// accepted; archives and executables are not. ".lrc", ".ttml" and ".txt" ARE
// consumed since the lyrics feature (the scanner reads <stem>.lrc / .ttml /
// .txt as the track's lyrics, served by /v1/lyrics); ".cue" is accepted but
// NOT consumed — the bridge has no cue-sheet handling — so it round-trips for
// other players without the UI implying it does something here.
//
// ".png" is deliberately absent. The local-artwork path is JPEG-only by design
// (internal/manifest/extractors.go validates both the MIME and the SOI magic),
// so accepting a PNG cover would silently produce a file that is never used.
var companionExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".pdf":  true,
	".lrc":  true,
	".ttml": true,
	".txt":  true,
	".cue":  true,
}

// AcceptedExt reports whether a filename's extension may be uploaded, and
// whether it is audio (as opposed to an inert companion).
//
// The audio set is manifest.Ext itself rather than a copy — there is no import
// cycle, and a mirrored map is a second thing to keep in step.
func AcceptedExt(name string) (accepted, isAudio bool) {
	ext := strings.ToLower(path.Ext(name))
	if manifest.Ext[ext] {
		return true, true
	}
	return companionExts[ext], false
}

// ValidateRelPath checks a client-declared, forward-slash relative path and
// returns its cleaned form.
//
// It is called TWICE: once at session create so a rejection costs nothing, and
// again at commit against the resolved root. The second call is the one that
// matters — a session manifest is on disk between them and must not be trusted
// to still say what it said.
func ValidateRelPath(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	if len(rel) > maxRelPathBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidPath, maxRelPathBytes)
	}
	if !utf8.ValidString(rel) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrInvalidPath)
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: contains NUL", ErrInvalidPath)
	}
	// A backslash is refused rather than interpreted. Treating it as a
	// separator is wrong on POSIX (where it is a legal filename character)
	// and treating it as a literal is dangerous on Windows (where
	// filepath.Join would then split on it) — so the same declared path
	// would mean different things on different hosts. Refusing is the only
	// answer that is portable.
	if strings.ContainsRune(rel, '\\') {
		return "", fmt.Errorf("%w: contains a backslash", ErrInvalidPath)
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("%w: absolute", ErrInvalidPath)
	}
	// Windows drive-relative and UNC forms.
	if len(rel) >= 2 && rel[1] == ':' {
		return "", fmt.Errorf("%w: drive-qualified", ErrInvalidPath)
	}

	segs := strings.Split(rel, "/")
	if len(segs) > maxDepth {
		return "", fmt.Errorf("%w: deeper than %d segments", ErrInvalidPath, maxDepth)
	}
	for _, seg := range segs {
		if err := validateSegment(seg); err != nil {
			return "", err
		}
	}

	// Raw traversal is rejected above by validateSegment, so Clean can only
	// be a no-op here; comparing anyway makes that an assertion rather than
	// an assumption.
	cleaned := path.Clean(rel)
	if cleaned != rel {
		return "", fmt.Errorf("%w: not in clean form", ErrInvalidPath)
	}

	if accepted, _ := AcceptedExt(cleaned); !accepted {
		return "", fmt.Errorf("%w: %q is not an accepted file type", ErrInvalidPath, path.Ext(cleaned))
	}
	return cleaned, nil
}

func validateSegment(seg string) error {
	switch seg {
	case "":
		return fmt.Errorf("%w: empty path segment", ErrInvalidPath)
	case ".", "..":
		return fmt.Errorf("%w: %q segment", ErrInvalidPath, seg)
	}
	if len(seg) > maxSegmentBytes {
		return fmt.Errorf("%w: segment longer than %d bytes", ErrInvalidPath, maxSegmentBytes)
	}
	// A leading dot would land the file inside a directory the scanner
	// skips — which is how the staging and trash directories hide — so a
	// client must not be able to write there.
	if strings.HasPrefix(seg, ".") {
		return fmt.Errorf("%w: segment starts with a dot", ErrInvalidPath)
	}
	for _, r := range seg {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character in segment", ErrInvalidPath)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control character in segment", ErrInvalidPath)
		}
		// Characters Windows refuses outright.
		if strings.ContainsRune(`<>:"|?*`, r) {
			return fmt.Errorf("%w: %q is not allowed in a filename", ErrInvalidPath, r)
		}
	}
	// Windows silently strips trailing dots and spaces, so a file created
	// with one resolves to a different name than the manifest records.
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return fmt.Errorf("%w: segment ends with a dot or space", ErrInvalidPath)
	}
	stem := seg
	if i := strings.Index(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	if windowsReserved[strings.ToUpper(stem)] {
		return fmt.Errorf("%w: %q is a reserved device name", ErrInvalidPath, stem)
	}
	return nil
}

// IsUnderStaging reports whether a library-relative path lies inside a staging
// or trash directory. Those are dot-directories, so ValidateRelPath already
// refuses to produce one; this is the belt-and-braces check for paths that
// arrive from somewhere other than a client declaration.
func IsUnderStaging(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// AssertRootContains is the final containment check, run at commit against the
// resolved absolute paths.
//
// On Windows this is the PRIMARY defense rather than a backstop: the raw
// traversal scan and path.Clean are both slash-based, so it is filepath.Join —
// cleaning with a backslash — that would collapse a backslash traversal into a
// real escape. ValidateRelPath refuses backslashes for that reason; this check
// is what makes the refusal unnecessary to be perfect.
func AssertRootContains(root, abs string) error {
	if fsutil.IsUnderAny(abs, []string{root}) == "" {
		return fmt.Errorf("%w: %q resolves outside the library root", ErrInvalidPath, abs)
	}
	return nil
}
