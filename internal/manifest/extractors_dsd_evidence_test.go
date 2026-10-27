package manifest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDFFCompressionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		nested, extra []byte
		want          string
	}{
		{"raw", nil, nil, "raw"},
		{"duplicate", dffChunk("CMPR", []byte("DSD ")), nil, "unsupported"},
		{"conflict", dffChunk("CMPR", []byte("DST ")), nil, "unsupported"},
		{"short", dffChunk("CMPR", []byte("DS")), nil, "unsupported"},
		{"overflow", []byte("ABSS\xff\xff\xff\xff\xff\xff\xff\xff"), nil, "unsupported"},
		{"trailing", []byte{1, 2}, nil, "unsupported"},
		{"two_props", nil, dffChunk("PROP", []byte("SND ")), "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := dffWithPropAndChunks(t, 2822400, tc.nested, tc.extra)
			if tc.name == "conflict" {
				b = conflictingDFFEvidence(t)
			}
			path := writeTempDFF(t, b)
			tr := &Track{}
			if err := extractDFFWithContext(path, tr, nil); err != nil {
				t.Fatal(err)
			}
			if tr.DFFCompressionEvidence != tc.want {
				t.Fatalf("evidence=%q want %q", tr.DFFCompressionEvidence, tc.want)
			}
			wire, _ := json.Marshal(tr)
			var fields map[string]any
			json.Unmarshal(wire, &fields)
			if fields["dffCompressionEvidence"] != tc.want {
				t.Fatal("wire evidence lost")
			}
		})
	}
}
func TestDSFValidatedChannels(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind, count uint32
		want        int
	}{{"stereo", 2, 2, 2}, {"mono", 1, 1, 1}, {"mismatch", 2, 1, 0}, {"unknown", 99, 2, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeMinimalDSFWithBits(t, 2822400, 1)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			binary.LittleEndian.PutUint32(b[48:52], tc.kind)
			binary.LittleEndian.PutUint32(b[52:56], tc.count)
			os.WriteFile(path, b, 0600)
			tr := &Track{}
			if err := extractDSF(path, tr); err != nil {
				t.Fatal(err)
			}
			if tc.want == 0 {
				if tr.Channels != nil {
					t.Fatal("invalid fmt claims channels")
				}
			} else if tr.Channels == nil || *tr.Channels != tc.want {
				t.Fatal("actual channels missing")
			}
		})
	}
}

func TestDSDEvidenceBackfillFullDelta(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	samples := map[string][]byte{
		"raw.dff":       dffWithPropAndChunks(t, 2822400, nil),
		"duplicate.dff": dffWithPropAndChunks(t, 2822400, dffChunk("CMPR", []byte("DSD "))),
		"conflict.dff":  conflictingDFFEvidence(t),
		"short.dff":     dffWithPropAndChunks(t, 2822400, dffChunk("CMPR", []byte("DS"))),
		"overflow.dff":  dffWithPropAndChunks(t, 2822400, []byte("ABSS\xff\xff\xff\xff\xff\xff\xff\xff")),
		"props.dff":     dffWithPropAndChunks(t, 2822400, nil, dffChunk("PROP", []byte("SND "))),
	}
	dsf, e := os.ReadFile(writeMinimalDSFWithBits(t, 2822400, 1))
	if e != nil {
		t.Fatal(e)
	}
	samples["stereo.dsf"] = dsf
	for name, b := range samples {
		if e := os.WriteFile(filepath.Join(root, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	scanOnce(t, sc, "initial")
	if _, e := store.db.Exec(`UPDATE tracks SET extractor_version=27,tags_json=json_remove(tags_json,'$.dffCompressionEvidence','$.channels')`); e != nil {
		t.Fatal(e)
	}
	var before int64
	if e := store.db.QueryRow(`SELECT max(indexed_at) FROM tracks`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	scanOnce(t, sc, "DSD evidence backfill")
	for _, since := range []time.Time{{}, time.Unix(0, before)} {
		m, e := BuildManifest(context.Background(), store, []string{root}, since)
		if e != nil {
			t.Fatal(e)
		}
		if len(m.Tracks) != len(samples) {
			t.Fatalf("full/delta tracks=%d want %d", len(m.Tracks), len(samples))
		}
		for _, tr := range m.Tracks {
			if tr.Codec == "DSF" {
				if tr.Channels == nil || *tr.Channels != 2 {
					t.Fatal("DSF channels missing from backfill")
				}
			} else {
				want := "unsupported"
				if tr.Path == "raw.dff" {
					want = "raw"
				}
				if tr.DFFCompressionEvidence != want {
					t.Fatalf("%s evidence=%q want %q", tr.Path, tr.DFFCompressionEvidence, want)
				}
			}
		}
		var wire bytes.Buffer
		if e := WriteManifest(context.Background(), &wire, store, []string{root}, since); e != nil {
			t.Fatal(e)
		}
		var decoded Manifest
		if e := json.Unmarshal(wire.Bytes(), &decoded); e != nil {
			t.Fatal(e)
		}
		if len(m.Tracks) != len(decoded.Tracks) {
			t.Fatal("streamed track count")
		}
		wantByPath := map[string]Track{}
		for _, tr := range m.Tracks {
			wantByPath[tr.Path] = tr
		}
		for _, tr := range decoded.Tracks {
			want := wantByPath[tr.Path]
			if tr.DFFCompressionEvidence != want.DFFCompressionEvidence || (tr.Channels == nil) != (want.Channels == nil) {
				t.Fatal("streamed full/delta loses evidence")
			}
		}
	}
	var after int64
	if e := store.db.QueryRow(`SELECT max(indexed_at) FROM tracks`).Scan(&after); e != nil {
		t.Fatal(e)
	}
	scanOnce(t, sc, "unchanged")
	m, e := BuildManifest(context.Background(), store, []string{root}, time.Unix(0, after))
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Tracks) != 0 {
		t.Fatal("unchanged scan emits a delta")
	}
	if ExtractorVersion <= 27 {
		t.Fatal("extractor version not bumped")
	}
}

func conflictingDFFEvidence(t *testing.T) []byte {
	t.Helper()
	b := dffWithPropAndChunks(t, 2822400, dffChunk("CMPR", []byte("DSD ")))
	at := bytes.Index(b, []byte("CMPR"))
	copy(b[at+12:], "DST ")
	return b
}

func TestMalformedDFFKeepsKnownFormat(t *testing.T) {
	path := writeTempDFF(t, dffWithPropAndChunks(t, 2822400, []byte{1, 2}))
	tr := &Track{}
	if e := extractDFFWithContext(path, tr, nil); e != nil {
		t.Fatal(e)
	}
	if tr.DFFCompressionEvidence != "unsupported" || tr.Codec != "DFF" || tr.IsDSD == nil || !*tr.IsDSD || tr.SampleRate == nil || *tr.SampleRate != 2822400 || tr.BitsPerSample == nil || *tr.BitsPerSample != 1 {
		t.Fatalf("known DFF format lost: %+v", tr)
	}
}
