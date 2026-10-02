package transcode

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestACutStreamIsTheOutputSidesUnlessTheVolumeSaysOtherwise pins
// classifyCut on what a volume can answer the probe that follows a stream
// cut short, on every platform: nothing (it takes the bytes: the fault went
// as the tool exited), a permission (fs.ErrPermission), or a cause that is
// no fault of the host, which keeps the job's strike. The errno causes are
// in rendition_complete_unix_test.go.
func TestACutStreamIsTheOutputSidesUnlessTheVolumeSaysOtherwise(t *testing.T) {
	cut := errors.New("cut short")
	denied := &os.PathError{Op: "write", Path: "x.tmp", Err: os.ErrPermission}
	for _, tc := range []struct {
		name   string
		probe  error
		kind   outputFaultKind // 0: not the output side's
		reason string
	}{
		{name: "the volume takes the bytes", probe: nil, kind: outputFull, reason: reasonWriteFailedThenRoom},
		{name: "a permission", probe: denied, kind: outputDenied},
		{name: "a cause no table names", probe: errors.New("an answer no table names")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyCut(outputVariants, "/variants/Album", cut, tc.probe)
			if !errors.Is(err, cut) {
				t.Errorf("classifyCut = %v, want the cut wrapped", err)
			}
			f, marked := unwritableOutput(err)
			if marked != (tc.kind != 0) || (marked && (f.kind != tc.kind || f.where != outputVariants || f.dir != "/variants/Album")) {
				t.Errorf("marked %v as %+v, want kind %v", marked, f, tc.kind)
			}
			if tc.reason != "" && f.reason != tc.reason {
				t.Errorf("reason %q, want %q", f.reason, tc.reason)
			}
			if tc.probe != nil && !strings.Contains(err.Error(), tc.probe.Error()) {
				t.Errorf("%q does not say what the volume answered (%v)", err, tc.probe)
			}
		})
	}
}

// TestAShortScratchIsTheOutputSidesOnlyWhenItsVolumeRefuses pins
// classifyScratch: a scratch volume that takes the bytes says the source
// decoded short, which strikes as it always did; one that refuses for a
// cause the host's table names is the scratch's outage.
func TestAShortScratchIsTheOutputSidesOnlyWhenItsVolumeRefuses(t *testing.T) {
	short := errors.New("decoded short")
	if err := classifyScratch("/tmp/1-bit-bridge-render", short, nil); err != short {
		t.Errorf("room: classifyScratch = %v, want the error as it was", err)
	}
	if err := classifyScratch("/tmp/1-bit-bridge-render", short, errors.New("an answer no table names")); err != short {
		t.Errorf("an unnamed refusal: classifyScratch = %v, want the error as it was", err)
	}
	err := classifyScratch("/tmp/1-bit-bridge-render", short, &os.PathError{Op: "write", Path: "x", Err: os.ErrPermission})
	if f, ok := unwritableOutput(err); !ok || f.where != outputScratch || f.kind != outputDenied || !errors.Is(err, short) {
		t.Errorf("a permission: classifyScratch = %v (%+v, %v), want the scratch's, the error wrapped", err, f, ok)
	}
}
