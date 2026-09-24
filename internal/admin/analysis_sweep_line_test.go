package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// analysisSweepLineRe splits describeAnalysisSweep's line into the total and
// its parts: "12 tracks — 3 enqueued · 9 up to date".
var analysisSweepLineRe = regexp.MustCompile(`^(\d+) tracks — (.+)$`)

// analysisSweepPartRe matches one part of the line, which opens with the count
// it contributes to the total.
var analysisSweepPartRe = regexp.MustCompile(`^(\d+) \S`)

// sweepBucket is one bucket of AnalysisSweepCounts: its JSON name and the
// value the fixture gave it.
type sweepBucket struct {
	name  string
	value int
}

// TestDescribeAnalysisSweepAccountsForEveryTrack runs the shipped
// describeAnalysisSweep under node and requires its line to account for every
// track: each bucket of AnalysisSweepCounts named, and the parts adding back
// up to Total.
//
// That is the line's own promise ("the numbers visibly account for total"),
// and nothing pinned it. TestEveryJobsFieldIsRenderedSomewhere cannot: its
// walk stops at the shared *AnalysisSweepState, so these leaves are outside
// it, and one read of `analysis.sweep` satisfies it. The bucket at risk is the
// next one added. `alreadyQueued` arrived with a change to the Go sweeper, and
// until this test nothing tied a new field to the line that has to name it.
//
// Every int field but Total is filled by reflection with a distinct power of
// two, so a bucket the line leaves out is named by its missing value, and a
// field is covered the day it is declared. The fixture travels through
// json.Marshal of the real type, so a Go tag and a JS read that disagree
// about a name fail here as well.
func TestDescribeAnalysisSweepAccountsForEveryTrack(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	counts, buckets := analysisSweepWithDistinctBuckets(t)
	// Vacuous-pass guard: a walk that found no buckets would check nothing.
	if len(buckets) < 7 {
		t.Fatalf("only %d buckets found on AnalysisSweepCounts: the reflection walk "+
			"is broken, so this test proves nothing", len(buckets))
	}
	payload, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	fn := extractJSFunction(t, readFile(t, "static/app.js"), "describeAnalysisSweep")
	script := fn + "\nconsole.log(describeAnalysisSweep(" + string(payload) + "));\n"
	path := filepath.Join(t.TempDir(), "sweep.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	line := strings.TrimSpace(string(out))

	m := analysisSweepLineRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("describeAnalysisSweep rendered %q, not \"<total> tracks — <parts>\"", line)
	}
	if m[1] != strconv.Itoa(counts.Total) {
		t.Errorf("the line opens with %s tracks, want %d: %q", m[1], counts.Total, line)
	}
	rendered := map[int]bool{}
	sum := 0
	for _, part := range strings.Split(m[2], " · ") {
		pm := analysisSweepPartRe.FindStringSubmatch(part)
		if pm == nil {
			t.Fatalf("part %q of %q does not open with a count", part, line)
		}
		n, err := strconv.Atoi(pm[1])
		if err != nil {
			t.Fatalf("part %q of %q: %v", part, line, err)
		}
		rendered[n] = true
		sum += n
	}
	for _, b := range buckets {
		if !rendered[b.value] {
			t.Errorf("%q (%d) is not in the line %q.\n"+
				"Every bucket is a part of total: one the line does not name is tracks "+
				"that silently stop adding up.", b.name, b.value, line)
		}
	}
	if sum != counts.Total {
		t.Errorf("the line's parts add up to %d of %d tracks: %q", sum, counts.Total, line)
	}
}

// analysisSweepWithDistinctBuckets returns an AnalysisSweepCounts whose int
// fields other than Total hold distinct powers of two, with Total their sum
// and the queue not saturated, and each bucket's JSON name and value.
//
// A field of any other kind fails the test. Whether it is a bucket of Total,
// and how the line renders it, is for whoever adds it to decide.
func analysisSweepWithDistinctBuckets(t *testing.T) (AnalysisSweepCounts, []sweepBucket) {
	t.Helper()
	var c AnalysisSweepCounts
	v := reflect.ValueOf(&c).Elem()
	var buckets []sweepBucket
	total := 0
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		switch {
		case f.Name == "Total", f.Name == "QueueSaturated":
			continue
		case f.Type.Kind() == reflect.Int:
			value := 1 << len(buckets)
			v.Field(i).SetInt(int64(value))
			buckets = append(buckets, sweepBucket{strings.Split(f.Tag.Get("json"), ",")[0], value})
			total += value
		default:
			t.Fatalf("AnalysisSweepCounts.%s is a %s, which this test cannot fill. Decide "+
				"whether it is a bucket of Total and how describeAnalysisSweep renders it, "+
				"then teach this test.", f.Name, f.Type)
		}
	}
	c.Total = total
	return c, buckets
}
