package manifest

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The evidence a render asks for before it writes into a variants directory
// that reads as away (backlog B727): VariantRecordedUnder, whether the catalog
// records a rendition below the directory. Its range holds exactly the
// recorded paths below the directory: never a sibling whose name only begins
// with the directory's (variants2, variants-old, variants.bak, and the
// character after the separator, '0' after '/' and ']' after '\'), never the
// directory's own path, with or without a separator at the end of the
// directory given; by the platform's separator; and compared as the platform's
// volumes compare names: without regard to case on Windows and macOS (NTFS and
// APFS by default), where a variants directory spelled in another case since
// its renditions were recorded is the same directory, and exactly on Linux.

// variantShape is one platform's spelling of a variants directory and of
// what is recorded around it.
type variantShape struct {
	name string
	sep  rune
	// dir is the variants directory as configured, and respelled the same
	// directory in another case.
	dir, respelled string
	// below is a rendition recorded below dir; siblings are recorded paths
	// that only begin as dir does, or name dir itself.
	below    string
	siblings []string
}

var variantShapes = []variantShape{
	{
		name: "slash", sep: '/', dir: "/mnt/usb/Variants", respelled: "/mnt/usb/vARIANTS",
		below: "/mnt/usb/Variants/Artist/Album/01.flac.upscaled-v2-176400-24.flac",
		siblings: []string{"/mnt/usb/Variants2/a.flac", "/mnt/usb/Variants-old/a.flac",
			"/mnt/usb/Variants.bak/a.flac", "/mnt/usb/Variants0/a.flac", "/mnt/usb/Variants"},
	},
	{
		name: "backslash", sep: '\\', dir: `D:\mnt\usb\Variants`, respelled: `d:\MNT\usb\variants`,
		below: `D:\mnt\usb\Variants\Artist\Album\01.flac.upscaled-v2-176400-24.flac`,
		siblings: []string{`D:\mnt\usb\Variants2\a.flac`, `D:\mnt\usb\Variants-old\a.flac`,
			`D:\mnt\usb\Variants.bak\a.flac`, `D:\mnt\usb\Variants]\a.flac`, `D:\mnt\usb\Variants`},
	},
	{
		// A fold that lowers ASCII alone (SQLite's own lower()) misses this.
		name: "non-ASCII", sep: '/', dir: "/Volumes/MÜSIK/Varianten", respelled: "/Volumes/müsik/varianten",
		below:    "/Volumes/MÜSIK/Varianten/Ärzte/01.flac.upscaled-v2-176400-24.flac",
		siblings: []string{"/Volumes/MÜSIK/Varianten2/a.flac", "/Volumes/MÜSIK/Varianten-alt/a.flac"},
	},
}

// recordedVariants is a store recording one rendition at each of sidecars,
// each of a track of its own.
func recordedVariants(t *testing.T, sidecars ...string) *Store {
	t.Helper()
	s := openTestStore(t)
	for i, p := range sidecars {
		src := fmt.Sprintf("Artist/Album/%02d.flac", i)
		seedTrack(t, s, src)
		mustUpsertVariant(t, s, VariantRow{SourcePath: src, VariantID: "upscaled-v2-176400-24",
			SidecarPath: p, SampleRate: 176400, BitsPerSample: 24})
	}
	return s
}

// withAndWithoutItsSeparator is dir as given and with one separator after it.
func withAndWithoutItsSeparator(dir string, sep rune) []string {
	return []string{dir, dir + string(sep)}
}

// TestTheVariantsGuardHoldsOnlyWhatIsRecordedBelowItsDirectory: in every
// shape, compared exactly or without regard to case, the siblings alone are
// no rendition below the directory, and a rendition below it is one.
func TestTheVariantsGuardHoldsOnlyWhatIsRecordedBelowItsDirectory(t *testing.T) {
	ctx := context.Background()
	for _, sh := range variantShapes {
		for _, fold := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fold=%v", sh.name, fold), func(t *testing.T) {
				s := recordedVariants(t, sh.siblings...)
				for _, dir := range withAndWithoutItsSeparator(sh.dir, sh.sep) {
					if held, err := s.variantRecordedUnder(ctx, dir, sh.sep, fold); err != nil || held {
						t.Errorf("with %q recorded, %q holds a rendition (%v, %v)", sh.siblings, dir, held, err)
					}
				}
				s = recordedVariants(t, append(append([]string(nil), sh.siblings...), sh.below)...)
				for _, dir := range withAndWithoutItsSeparator(sh.dir, sh.sep) {
					if held, err := s.variantRecordedUnder(ctx, dir, sh.sep, fold); err != nil || !held {
						t.Errorf("with %q recorded, %q holds no rendition (%v, %v)", sh.below, dir, held, err)
					}
				}
			})
		}
	}
}

// TestTheVariantsGuardFindsADirectoryRespelledWhereTheVolumeFoldsCase: the
// directory spelled in another case holds what is recorded below its first
// spelling where the volume folds case, and nothing where it compares names
// exactly; and its siblings, in any case, are still none of it.
func TestTheVariantsGuardFindsADirectoryRespelledWhereTheVolumeFoldsCase(t *testing.T) {
	ctx := context.Background()
	for _, sh := range variantShapes {
		t.Run(sh.name, func(t *testing.T) {
			s := recordedVariants(t, sh.below)
			for _, dir := range withAndWithoutItsSeparator(sh.respelled, sh.sep) {
				if held, err := s.variantRecordedUnder(ctx, dir, sh.sep, true); err != nil || !held {
					t.Errorf("where the volume folds case, %q holds no rendition while %q is recorded (%v, %v)",
						dir, sh.below, held, err)
				}
				if held, err := s.variantRecordedUnder(ctx, dir, sh.sep, false); err != nil || held {
					t.Errorf("where the volume compares names exactly, %q holds %q (%v, %v)", dir, sh.below, held, err)
				}
			}
			var upper []string
			for _, p := range sh.siblings {
				upper = append(upper, strings.ToUpper(p))
			}
			s = recordedVariants(t, upper...)
			for _, dir := range withAndWithoutItsSeparator(sh.respelled, sh.sep) {
				if held, err := s.variantRecordedUnder(ctx, dir, sh.sep, true); err != nil || held {
					t.Errorf("where the volume folds case, with %q recorded, %q holds a rendition (%v, %v)",
						upper, dir, held, err)
				}
			}
		})
	}
}

// TestTheVariantsGuardComparesAsThisPlatformsVolumesDo: the store's own
// answer, on the platform this runs on, for a directory spelled as its
// renditions were recorded and for the same directory in another case, which
// holds them on Windows and macOS alone.
func TestTheVariantsGuardComparesAsThisPlatformsVolumesDo(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "usb", "Variants")
	s := recordedVariants(t, filepath.Join(dir, "Artist", "01.flac.upscaled-v2-176400-24.flac"))
	if held, err := s.VariantRecordedUnder(ctx, dir); err != nil || !held {
		t.Fatalf("the directory as recorded holds no rendition (%v, %v)", held, err)
	}
	respelled := filepath.Join(filepath.Dir(dir), "vARIANTS")
	want := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if held, err := s.VariantRecordedUnder(ctx, respelled); err != nil || held != want {
		t.Errorf("on %s the directory in another case holds a rendition: %v (%v), want %v",
			runtime.GOOS, held, err, want)
	}
}
