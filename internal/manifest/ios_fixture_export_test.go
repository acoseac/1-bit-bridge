package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestExportIOSManifestFixtures always exercises extraction, SQLite and both
// streaming legs. Only publication outside t.TempDir is environment-gated.
func TestExportIOSManifestFixtures(t *testing.T) {
	type sample struct {
		name, evidence string
		channels       int
		data           []byte
	}
	raw := dffWithPropAndChunks(t, 2822400, nil)
	samples := []sample{
		{"single_raw.dff", "raw", 2, raw},
		{"missing_cmpr.dff", "missing", 2, iosReplaceCMPR(t, raw, nil)},
		{"unknown_cmpr.dff", "unsupported", 0, iosReplaceCMPR(t, raw, dffChunk("CMPR", []byte("????\x00")))},
		{"short_cmpr.dff", "unsupported", 2, iosReplaceCMPR(t, raw, dffChunk("CMPR", []byte("DS")))},
		{"duplicate_cmpr.dff", "unsupported", 2, dffWithPropAndChunks(t, 2822400, dffChunk("CMPR", []byte("DSD ")))},
		{"conflicting_cmpr.dff", "unsupported", 2, conflictingDFFEvidence(t)},
		{"multiple_prop.dff", "unsupported", 2, dffWithPropAndChunks(t, 2822400, nil, dffChunk("PROP", []byte("SND ")))},
		{"malformed_tail.dff", "unsupported", 2, dffWithPropAndChunks(t, 2822400, []byte{1, 2})},
		{"dst.dff", "unsupported", 2, buildDFFWithOpts(t, dffFixtureOpts{sampleRate: 2822400, compression: "DST ", channels: 2, dstFrames: 75, dstFrameRate: 75, emitDSTChunk: true})},
		{"ordinary_pcm.wav", "", 0, wavWithChunks()},
	}
	dsf, err := os.ReadFile(writeMinimalDSFWithBits(t, 2822400, 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		name        string
		kind, count uint32
		want        int
	}{
		{"mono", 1, 1, 1}, {"stereo", 2, 2, 2}, {"multichannel", 7, 6, 6}, {"invalid_fmt", 2, 1, 0},
	} {
		data := bytes.Clone(dsf)
		binary.LittleEndian.PutUint32(data[48:52], v.kind)
		binary.LittleEndian.PutUint32(data[52:56], v.count)
		samples = append(samples, sample{v.name + ".dsf", "", v.want, data})
	}
	root := filepath.Join(t.TempDir(), "ios-dsd")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, scanner := newScanFixture(t, root)
	for _, s := range samples {
		if err := os.WriteFile(filepath.Join(root, s.name), s.data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	scanOnce(t, scanner, "iOS evidence fixtures")
	var earliest int64
	if err := store.db.QueryRow(`SELECT min(indexed_at) FROM tracks`).Scan(&earliest); err != nil {
		t.Fatal(err)
	}
	// A cursor immediately before the earliest insertion selects every new row.
	deltaSince := time.Unix(0, earliest-1)
	documents := map[string][]byte{}
	for _, leg := range []struct {
		name  string
		since time.Time
	}{{"dsd_full.json", time.Time{}}, {"dsd_delta.json", deltaSince}} {
		var out bytes.Buffer
		if err := WriteManifest(context.Background(), &out, store, []string{root}, leg.since); err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Tracks  []map[string]json.RawMessage `json:"tracks"`
			Folders json.RawMessage              `json:"folders"`
		}
		if err := json.Unmarshal(out.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire.Tracks) != len(samples) {
			t.Fatalf("%s: got %d tracks, want %d", leg.name, len(wire.Tracks), len(samples))
		}
		byPath := map[string]map[string]json.RawMessage{}
		for _, tr := range wire.Tracks {
			var p string
			if err := json.Unmarshal(tr["path"], &p); err != nil {
				t.Fatal(err)
			}
			if byPath[p] != nil {
				t.Fatalf("duplicate path %s", p)
			}
			byPath[p] = tr
		}
		for _, s := range samples {
			tr, ok := byPath[s.name]
			if !ok {
				t.Fatalf("missing %s", s.name)
			}
			wantEvidence, _ := json.Marshal(s.evidence)
			if s.evidence == "" {
				if _, ok := tr["dffCompressionEvidence"]; ok {
					t.Fatalf("%s: non-DFF evidence", s.name)
				}
			} else if !bytes.Equal(tr["dffCompressionEvidence"], wantEvidence) {
				t.Fatalf("%s: evidence %s, want %s", s.name, tr["dffCompressionEvidence"], wantEvidence)
			}
			if s.channels == 0 {
				if _, ok := tr["channels"]; ok {
					t.Fatalf("%s: unknown channels emitted", s.name)
				}
			} else if string(tr["channels"]) != fmt.Sprint(s.channels) {
				t.Fatalf("%s: channels %s", s.name, tr["channels"])
			}
			codec := "DFF"
			if strings.HasSuffix(s.name, ".dsf") {
				codec = "DSF"
			}
			if strings.HasSuffix(s.name, ".wav") {
				codec = "WAV"
			}
			if string(tr["codec"]) != strconvJSON(codec) {
				t.Fatalf("%s: codec %s", s.name, tr["codec"])
			}
			if s.name == "dst.dff" {
				if string(tr["compression"]) != `"DST"` {
					t.Fatal("DST compression missing")
				}
			} else if s.name != "conflicting_cmpr.dff" {
				if _, ok := tr["compression"]; ok {
					t.Fatalf("%s: unexpected compression", s.name)
				}
			}
		}
		if !leg.since.IsZero() && wire.Folders != nil {
			t.Fatal("delta includes folders")
		}
		documents[leg.name] = bytes.Clone(out.Bytes())
	}
	var full, delta map[string]json.RawMessage
	if err := json.Unmarshal(documents["dsd_full.json"], &full); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(documents["dsd_delta.json"], &delta); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full["tracks"], delta["tracks"]) {
		t.Fatal("full/delta track bytes differ")
	}
	commit := ""
	if os.Getenv("BRIDGE_EXPORT_IOS_FIXTURES") != "" {
		git, err := exec.LookPath("git")
		if err == nil {
			if b, e := exec.Command(git, "rev-parse", "HEAD").Output(); e == nil {
				commit = strings.TrimSpace(string(b))
			}
		}
		if commit == "" {
			commit = os.Getenv("BRIDGE_FIXTURE_COMMIT")
		}
		if commit == "" {
			t.Fatal("git unavailable: set BRIDGE_FIXTURE_COMMIT")
		}
	}
	protocol, err := os.ReadFile(filepath.Join("..", "..", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]any{}
	for _, s := range samples {
		records[s.name] = map[string]any{"case": strings.TrimSuffix(s.name, filepath.Ext(s.name)), "inputSHA256": fmt.Sprintf("%x", sha256.Sum256(s.data)), "inputBytes": len(s.data), "dffCompressionEvidence": s.evidence, "channels": s.channels, "documents": []string{"dsd_full.json", "dsd_delta.json"}}
	}
	info := map[string]any{"bridgeCommit": commit, "protocolSHA256": fmt.Sprintf("%x", sha256.Sum256(protocol)), "source": "working tree; commit identifies the base, protocolSHA256 identifies the mirrored contract", "manifestRecords": records, "deltaSince": deltaSince.UTC().Format(time.RFC3339Nano)}
	if dir := os.Getenv("BRIDGE_EXPORT_IOS_FIXTURES"); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		// Export packages sequentially (-p=1); preserve the other exporter's section.
		path := filepath.Join(dir, "fixtures-info.json")
		if b, e := os.ReadFile(path); e == nil {
			var old map[string]json.RawMessage
			if e = json.Unmarshal(b, &old); e != nil {
				t.Fatal(e)
			}
			if v, ok := old["outputRecords"]; ok {
				info["outputRecords"] = v
			}
		} else if !os.IsNotExist(e) {
			t.Fatal(e)
		}
		for name, b := range documents {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("BRIDGE_EXPORT_IOS_FIXTURES"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "fixtures-info.json"), append(b, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func strconvJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

// Replace the single CMPR in the shared raw fixture, updating both containing
// sizes. A short CMPR here is standalone, not merely a second conflicting one.
func iosReplaceCMPR(t *testing.T, raw, chunk []byte) []byte {
	t.Helper()
	at := bytes.Index(raw, []byte("CMPR"))
	if at < 0 {
		t.Fatal("CMPR missing from builder")
	}
	n := int(binary.BigEndian.Uint64(raw[at+4 : at+12]))
	n = 12 + n + n%2
	out := append(bytes.Clone(raw[:at]), chunk...)
	out = append(out, raw[at+n:]...)
	binary.BigEndian.PutUint64(out[4:12], uint64(len(out)-12))
	prop := bytes.Index(out, []byte("PROP"))
	size := int(binary.BigEndian.Uint64(raw[prop+4:prop+12])) + len(chunk) - n
	binary.BigEndian.PutUint64(out[prop+4:prop+12], uint64(size))
	return out
}

// Ordinary source-archive runs still exercise extraction and both serializers.
func TestIOSManifestFixturesWithoutProvenance(t *testing.T) {
	t.Setenv("BRIDGE_EXPORT_IOS_FIXTURES", "")
	t.Setenv("BRIDGE_FIXTURE_COMMIT", "")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing.git"))
	t.Run("no_head", TestExportIOSManifestFixtures)
	t.Setenv("PATH", t.TempDir())
	t.Run("no_git", TestExportIOSManifestFixtures)
}
