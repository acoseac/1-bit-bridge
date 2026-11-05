package dsdstate

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// schemaOneState is a state holding one of every field schema 1 has.
func schemaOneState(t *testing.T) State {
	h := hugo(t)
	return State{
		DefaultMode: ModeAuto,
		DACs: []DAC{
			{Key: hugo(t), DoPConfirmed: &DoPConfirmation{BCDDevice: "0100", ConfirmedAt: t0, SetBy: ByConsole}},
			{Key: goBar(t),
				Choice:   &Choice{Mode: ModeAuto, MaxBitRate: 0, Revision: 4, SetAt: t0, SetBy: ByApp},
				FirstUse: []FirstUse{{Transport: TransportNative, BCDDevice: "0123", BitRates: []int{DSD64}, AcknowledgedAt: t0, ObservedAt: t0.Add(time.Minute)}},
				Listener: []Answer{{Transport: TransportNative, BitRate: DSD64, BCDDevice: "0123", Answer: AnswerClean, On: "2026-11-05"}},
				Evidence: []Evidence{{Transport: TransportNative, Format: "DSD_U32_BE", BitRate: DSD64, BCDDevice: "0123", DriverAcceptedAt: t0, Kernel: "6.12.47-v8-16k+", StreamingObservedAt: t0.Add(time.Minute)}},
			},
		},
		MigratedFrom: &Marker{From: FromKeys, DSDMode: ModeAuto, AllowDoP: ptr(false), DSDQualification: "approved.json", RecordSHA256: strings.Repeat("ab", 32)},
		Warnings:     []Warning{{Code: WarningMigrationRecordRefused, Reason: ReasonIdentityMismatch, DAC: &h, At: t0}},
	}
}

// schemaOne is schemaOneState as its file holds it: schema 1, frozen.
const schemaOne = `{
  "schemaVersion": 1,
  "defaultMode": "auto",
  "dacs": [
    {
      "key": {
        "vendorId": "20b1",
        "productId": "3009",
        "interface": 1,
        "product": "GO bar"
      },
      "choice": {
        "mode": "auto",
        "maxBitRate": 0,
        "revision": 4,
        "setAt": "2026-11-05T10:00:00Z",
        "setBy": "app"
      },
      "dopConfirmed": null,
      "firstUse": [
        {
          "transport": "native",
          "bcdDevice": "0123",
          "bitRates": [
            2822400
          ],
          "acknowledgedAt": "2026-11-05T10:00:00Z",
          "observedAt": "2026-11-05T10:01:00Z"
        }
      ],
      "listener": [
        {
          "transport": "native",
          "bitRate": 2822400,
          "bcdDevice": "0123",
          "answer": "clean",
          "on": "2026-11-05"
        }
      ],
      "evidence": [
        {
          "transport": "native",
          "format": "DSD_U32_BE",
          "bitRate": 2822400,
          "bcdDevice": "0123",
          "driverAcceptedAt": "2026-11-05T10:00:00Z",
          "kernel": "6.12.47-v8-16k+",
          "streamingObservedAt": "2026-11-05T10:01:00Z"
        }
      ]
    },
    {
      "key": {
        "vendorId": "262a",
        "productId": "9302",
        "interface": null,
        "product": null
      },
      "choice": null,
      "dopConfirmed": {
        "bcdDevice": "0100",
        "confirmedAt": "2026-11-05T10:00:00Z",
        "setBy": "console"
      },
      "firstUse": [],
      "listener": [],
      "evidence": []
    }
  ],
  "migratedFrom": {
    "from": "keys",
    "dsdMode": "auto",
    "allowDoP": false,
    "dsdQualification": "approved.json",
    "recordSHA256": "abababababababababababababababababababababababababababababababab"
  },
  "warnings": [
    {
      "code": "migration_record_refused",
      "reason": "identity_mismatch",
      "dac": {
        "vendorId": "262a",
        "productId": "9302",
        "interface": null,
        "product": null
      },
      "at": "2026-11-05T10:00:00Z"
    }
  ]
}
`

// TestTheStateIsSchemaOneAsWritten freezes schema 1 (the design's section
// 2.5): a state with one of every field is written as these bytes, every key
// present, every list [], an absent interface and product null, the DACs in
// their keys' order and each list in its own; and the bytes read back as the
// same state, and write again as themselves.
func TestTheStateIsSchemaOneAsWritten(t *testing.T) {
	b, err := schemaOneState(t).encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != schemaOne {
		t.Fatalf("schema 1 is written as\n%s\nwant\n%s", b, schemaOne)
	}
	s, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.normalized(), schemaOneState(t).normalized()) {
		t.Fatalf("schema 1 reads back as %+v", s)
	}
	again, err := s.encode()
	if err != nil || string(again) != schemaOne {
		t.Fatalf("schema 1 read and written again is\n%s\n%v", again, err)
	}
}

// TestTheStateRefusesWhatNoWriterWrites: Decode refuses a file no writer of
// schema 1 writes, each with a refusal that names the field and no value:
// data after the state, an unknown field, another schema, and every value a
// field does not take, including a list present and empty where empty would
// read as every rate, and the owner each warning code must have or not.
func TestTheStateRefusesWhatNoWriterWrites(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, want string
	}{
		{"data after the state", "  ]\n}\n", "  ]\n}\n{}", "data after the state"},
		{"an unknown field", `"defaultMode": "auto",`, `"defaultMode": "auto", "volume": 11,`, "does not parse"},
		{"another schema", `"schemaVersion": 1`, `"schemaVersion": 2`, "schema 2, not 1"},
		{"defaultMode native", `"defaultMode": "auto"`, `"defaultMode": "native"`, "defaultMode is neither auto nor off"},
		{"an uppercase vendor ID", `"vendorId": "20b1"`, `"vendorId": "20B1"`, "vendorId is not four lowercase"},
		{"a product with space around it", `"product": "GO bar"`, `"product": "GO bar "`, "white space around it"},
		{"an interface past 255", `"interface": 1,`, `"interface": 256,`, "0 to 255"},
		{"two entries of one model", `"vendorId": "262a",
        "productId": "9302",
        "interface": null,
        "product": null
      },
      "choice": null,`, `"vendorId": "20b1",
        "productId": "3009",
        "interface": 1,
        "product": "GO bar"
      },
      "choice": null,`, "a model another entry holds"},
		{"an entry holding nothing", `"dopConfirmed": {
        "bcdDevice": "0100",
        "confirmedAt": "2026-11-05T10:00:00Z",
        "setBy": "console"
      },`, `"dopConfirmed": null,`, "holds nothing"},
		{"a mode", `"mode": "auto"`, `"mode": "loud"`, "choice.mode is not a mode"},
		{"a cap", `"maxBitRate": 0`, `"maxBitRate": 1000`, "choice.maxBitRate"},
		{"revision 0", `"revision": 4`, `"revision": 0`, "choice.revision"},
		{"a writer", `"setBy": "app"`, `"setBy": "user"`, "choice.setBy"},
		{"a confirmation's firmware", `"bcdDevice": "0100"`, `"bcdDevice": "100"`, "dopConfirmed.bcdDevice"},
		{"a first use's transport", `"transport": "native",
          "bcdDevice": "0123",
          "bitRates"`, `"transport": "pcm",
          "bcdDevice": "0123",
          "bitRates"`, "firstUse[0].transport"},
		{"an empty rate list", `"bitRates": [
            2822400
          ],`, `"bitRates": [],`, "bitRates is empty"},
		{"rates out of order", `"bitRates": [
            2822400
          ],`, `"bitRates": [5644800, 2822400],`, "each once, ascending"},
		{"an answer", `"answer": "clean"`, `"answer": "loud"`, "listener[0].answer"},
		{"an answer's date", `"on": "2026-11-05"`, `"on": "2026-02-30"`, "listener[0].on is not a date"},
		{"a second answer for one tuple", `"answer": "clean",
          "on": "2026-11-05"
        }`, `"answer": "clean",
          "on": "2026-11-05"
        },
        {"transport": "native", "bitRate": 2822400, "bcdDevice": "0123", "answer": "noise"}`, "second answer"},
		{"evidence of nothing", `"driverAcceptedAt": "2026-11-05T10:00:00Z",
          "kernel": "6.12.47-v8-16k+",
          "streamingObservedAt": "2026-11-05T10:01:00Z"`, `"driverAcceptedAt": "0001-01-01T00:00:00Z"`, "holds no observation"},
		{"a kernel with no driver acceptance", `"driverAcceptedAt": "2026-11-05T10:00:00Z",
          "kernel"`, `"kernel"`, "kernel is given without driverAcceptedAt"},
		{"a format its transport does not play", `"format": "DSD_U32_BE"`, `"format": "S32_LE"`, "evidence[0] is not a transport in a format it plays"},
		{"a marker from elsewhere", `"from": "keys"`, `"from": "elsewhere"`, "from is neither keys nor state_unreadable"},
		{"a marker of keys with none", `"dsdMode": "auto",
    "allowDoP": false,
    "dsdQualification": "approved.json",
    "recordSHA256": "abababababababababababababababababababababababababababababababab"`, `"waitingFor": ""`, "names none"},
		{"waiting for keys that give a DAC nothing", `"dsdMode": "auto",`, `"dsdMode": "off", "waitingFor": "bound_dac",`, "keys that give a DAC nothing"},
		{"a warning code", `"code": "migration_record_refused"`, `"code": "migration_refused"`, "code is not a warning code"},
		{"a refusal without its reason", `"reason": "identity_mismatch",`, ``, "reason is not a reason word"},
		{"a migration's warning of the bridge's", `"dac": {
        "vendorId": "262a",
        "productId": "9302",
        "interface": null,
        "product": null
      },`, ``, "names no DAC"},
		{"the bridge's warning of a DAC's", `"code": "migration_record_refused",
      "reason": "identity_mismatch",`, `"code": "migrated_off",`, "names a DAC"},
		{"a warning twice", "    }\n  ]\n}\n", `    },
    {"code": "migration_record_refused", "reason": "stale_version", "dac": {"vendorId": "262a", "productId": "9302", "interface": null, "product": null}, "at": "2026-11-05T10:00:00Z"}
  ]
}
`, "repeats a code its owner has"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Count(schemaOne, tc.old) != 1 {
				t.Fatalf("the case's text is in schema 1's %d times, want once", strings.Count(schemaOne, tc.old))
			}
			_, err := Decode([]byte(strings.Replace(schemaOne, tc.old, tc.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Decode = %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}

// TestTheChoiceThatApplies: a DAC's own choice applies when it has one, and
// defaultMode, with no cap, to a DAC with none (the design's section 2.5):
// auto on a fresh install, off after a migrated off or a state that did not
// read.
func TestTheChoiceThatApplies(t *testing.T) {
	s := schemaOneState(t)
	if mode, max, own := s.ChoiceFor(goBar(t)); mode != ModeAuto || max != 0 || !own {
		t.Errorf("the GO bar's choice = %q %d %v, want its own", mode, max, own)
	}
	if mode, _, own := s.ChoiceFor(hugo(t)); mode != ModeAuto || own {
		t.Errorf("a DAC with no choice of its own takes %q (own %v), want defaultMode", mode, own)
	}
	s.DefaultMode = ModeOff
	if mode, _, own := s.ChoiceFor(brooklyn(t)); mode != ModeOff || own {
		t.Errorf("a DAC the state does not hold takes %q (own %v) under defaultMode off", mode, own)
	}
	if !Refusing(t0).Unreadable() || Defaults().Unreadable() || s.Unreadable() {
		t.Error("Unreadable is not the state_unreadable warning's")
	}
}
