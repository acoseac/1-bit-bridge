package config

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Environment overrides, derived from the Config struct rather than
// hand-listed.
//
// The hand-written form covered 11 of ~90 settable fields, which is the
// shape that blocks provisioning a bridge declaratively: a tenant could
// not be given an autocert domain, a deployment mode, or most feature
// toggles without someone writing a YAML file by hand. Worse, the list
// had no way to stay complete — every config field added since was
// silently absent, and nothing said so.
//
// Deriving the table from the struct makes coverage total by
// construction and keeps it total: a new field is env-settable the day
// it is added, and TestEveryConfigLeafIsEnvSettable fails if it somehow
// is not.
//
// Naming: the yaml path, upper-snaked, with a BRIDGE_ prefix.
// `upscale.enabled` → BRIDGE_UPSCALE_ENABLED. Two pre-existing names do
// not follow that rule and are kept as aliases (see legacyEnvAliases) —
// renaming them would break every deployment already setting them.
//
// Precedence is unchanged: env > yaml > defaults, and path-typed values
// still flow through resolvePaths afterwards so a relative path from
// env inherits the same relative-to-config-dir semantics as a YAML one.

// envPrefix is the namespace for every override.
const envPrefix = "BRIDGE_"

// legacyEnvAliases map an ALREADY-SHIPPED variable name to the yaml path
// it sets, for the two whose names predate the derivation rule.
//
// Derivation would call these BRIDGE_ENRICH_MUSICBRAINZ_BASE_URL and
// BRIDGE_ENRICH_COVERART_BASE_URL. Both spellings work: the derived one
// because it is derived, the legacy one because it is listed here.
// Dropping the legacy names would silently stop honouring an override a
// deployment is already setting — the failure mode being that a bridge
// quietly starts hammering public MusicBrainz at the self-hosted pace.
var legacyEnvAliases = map[string]string{
	"BRIDGE_MUSICBRAINZ_BASE_URL": "enrich.musicbrainzBaseURL",
	"BRIDGE_COVERART_BASE_URL":    "enrich.coverArtBaseURL",
}

// envExemptPaths are yaml paths deliberately NOT env-settable, each with
// the reason. The completeness test consults this map, so an exemption
// is a decision someone wrote down rather than an omission.
var envExemptPaths = map[string]string{
	// Set by `bridge init` and resolved relative to the config file.
	// An env override here would move the config out from under the
	// process that just read it.
	"tlsCertPath": "derived from dataDir at init; moving it at runtime orphans the cert",
	"tlsKeyPath":  "derived from dataDir at init; moving it at runtime orphans the key",
}

// envName derives the variable name for a yaml path.
//
//	"listenAddress"        -> BRIDGE_LISTEN_ADDRESS
//	"upscale.enabled"      -> BRIDGE_UPSCALE_ENABLED
//	"disableHttp3"         -> BRIDGE_DISABLE_HTTP3
func envName(yamlPath string) string {
	var b strings.Builder
	b.WriteString(envPrefix)
	for i, seg := range strings.Split(yamlPath, ".") {
		if i > 0 {
			b.WriteByte('_')
		}
		b.WriteString(upperSnake(seg))
	}
	return b.String()
}

// upperSnake converts a camelCase yaml key to UPPER_SNAKE.
//
// A run of capitals is kept together so an initialism does not explode
// into single letters: `coverArtBaseURL` becomes COVER_ART_BASE_URL, not
// COVER_ART_BASE_U_R_L. A digit is treated as part of the word it
// follows, so `disableHttp3` becomes DISABLE_HTTP3 — which is what the
// already-shipped variable is called, and the reason this rule is not
// simply "split before every capital".
func upperSnake(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && i > 0 {
			prev := runes[i-1]
			prevLower := prev >= 'a' && prev <= 'z'
			prevDigit := prev >= '0' && prev <= '9'
			// Boundary before a capital that follows a lowercase or a
			// digit, or that starts a new word after an initialism
			// (the last capital of a run, when a lowercase follows).
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			prevUpper := prev >= 'A' && prev <= 'Z'
			if prevLower || prevDigit || (prevUpper && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(toUpperRune(r))
	}
	return b.String()
}

func toUpperRune(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}

// pathListFields are the slice fields whose entries are FILESYSTEM
// PATHS, and so use the OS-native PATH separator.
//
// Everything else uses a comma, and the distinction is not cosmetic. A
// colon separator is correct for paths (and `;` on Windows, so a
// drive-letter path like C:\Music is not split in half) and CATASTROPHIC
// for anything else in this config: `customEndpoints` holds URLs, which
// contain a colon before the port, and `metrics.allowCidrs` holds CIDRs,
// which contain several in any IPv6 range. Splitting
// "https://bridge.example:7788" on ":" yields three fragments, every one
// of which then fails URL validation and is silently dropped.
//
// The hand-written form this replaced applied the PATH separator to
// libraryRoots ONLY, which was right; generalising it to every string
// slice is what introduced the bug. (Gemini HIGH on PR #802.)
var pathListFields = map[string]bool{
	"libraryRoots": true,
}

func listSeparator(yamlPath string) string {
	if pathListFields[yamlPath] {
		return string(os.PathListSeparator)
	}
	return ","
}

// envBinding is one settable leaf: where it lives and what it is called.
type envBinding struct {
	YAMLPath string
	EnvVar   string
	// index is the reflect field-index path from the Config root, which
	// is what lets a binding be resolved against any *Config rather
	// than only the one it was discovered on.
	index []int
	kind  reflect.Kind
	// elemKind is the pointed-to kind for a pointer leaf.
	elemKind reflect.Kind
	// elemIsString marks a []string leaf.
	elemIsString bool
}

// envBindings walks the Config type once and returns every settable
// leaf. Pure function of the type, so the result is stable for a build.
func envBindings() []envBinding {
	var out []envBinding
	var walk func(t reflect.Type, prefix string, idx []int)
	walk = func(t reflect.Type, prefix string, idx []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("yaml")
			if tag == "" || tag == "-" {
				continue
			}
			key := strings.Split(tag, ",")[0]
			if key == "" {
				continue
			}
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			here := append(append([]int{}, idx...), i)

			ft := f.Type
			switch ft.Kind() {
			case reflect.Struct:
				// A nested config section. time.Time and friends have no
				// yaml-tagged fields, so they fall through to no bindings
				// rather than being descended into meaninglessly.
				walk(ft, path, here)
			case reflect.String, reflect.Bool, reflect.Int, reflect.Int64:
				out = append(out, envBinding{
					YAMLPath: path, EnvVar: envName(path), index: here, kind: ft.Kind(),
				})
			case reflect.Slice:
				if ft.Elem().Kind() == reflect.String {
					out = append(out, envBinding{
						YAMLPath: path, EnvVar: envName(path), index: here,
						kind: ft.Kind(), elemIsString: true,
					})
				}
			case reflect.Pointer:
				// Pointer scalars are the codebase's "unset means the
				// default" idiom — *bool for SmartPlaylists.Enabled,
				// *int for Backup.IntervalHours and the manifest rate
				// limits. An env override allocates, which is exactly
				// the explicit choice the pointer exists to represent.
				switch ft.Elem().Kind() {
				case reflect.Bool, reflect.Int, reflect.Int64:
					out = append(out, envBinding{
						YAMLPath: path, EnvVar: envName(path), index: here,
						kind: ft.Kind(), elemKind: ft.Elem().Kind(),
					})
				}
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "", nil)
	return out
}

// applyEnvOverrides sets fields from the environment. Env wins over
// yaml; an unset or empty variable is a no-op.
//
// A present-but-unparseable value is logged at Warn and IGNORED, so the
// yaml/default value stands. That direction is deliberate: coercing a
// typo like `yes` or `on` to false would silently turn a feature off,
// and a bridge that quietly stopped doing something is much harder to
// notice than one that ignored an override and said so. The warning names
// the variable alone (unparseableBoolEnv, unparseableIntEnv). The opt-outs
// (switchEnvPaths) read the other way: off is what they are for, so they
// take the file's spellings of on and off, and anything else as off.
//
// It records in c the yaml path of each leaf a variable set (envSet): the
// live holder's save writes the file's own value for each one an update
// left as it was, so a save does not write a variable's value into the
// file (filedEnvValues, backlog B761).
func (c *Config) applyEnvOverrides() {
	byPath := make(map[string]envBinding, 128)
	set := map[string]bool{}
	for _, b := range envBindings() {
		byPath[b.YAMLPath] = b
		if v, ok := os.LookupEnv(b.EnvVar); ok && v != "" && c.applyEnvValue(b, b.EnvVar, v) {
			set[b.YAMLPath] = true
		}
	}
	// Legacy names last so an explicitly-set legacy variable wins over
	// the derived one — a deployment that sets the old name is stating
	// an intention, and the derived name is unlikely to be set too.
	for name, path := range legacyEnvAliases {
		b, ok := byPath[path]
		if !ok {
			// The field was renamed or removed out from under the alias.
			// Loud, because the alias is now silently doing nothing.
			validateLogger.Warn("legacy env alias points at a config field that no longer exists",
				"var", name, "path", path)
			continue
		}
		if v, ok := os.LookupEnv(name); ok && v != "" && c.applyEnvValue(b, name, v) {
			set[path] = true
		}
	}
	c.envSet = nil
	for path := range set {
		c.envSet = append(c.envSet, path)
	}
	slices.Sort(c.envSet)
}

// The warnings for a BRIDGE_* variable that does not read as its setting's
// boolean or whole number. They name the variable and never its value, nor
// strconv's error, which repeats the value: a secret set in the wrong
// variable reached the journal with them until B505's round 3.
const (
	unparseableBoolEnv = "ignoring unparseable boolean env var"
	unparseableIntEnv  = "ignoring unparseable integer env var"
	// unreadableSwitchEnv is an opt-out's (switchEnvPaths): its value is
	// taken as off, never ignored.
	unreadableSwitchEnv = "an opt-out env var that reads as neither on nor off is taken as off"
)

// switchEnvPaths are the opt-outs whose variables read as the file reads
// their keys, and as off where they read as neither (backlog B617):
// enrich.enabled and enrich.deezerEnabled stop requests to services the
// operator chose not to ask. Read as every other boolean variable is, by
// strconv.ParseBool with a refusal ignored, `off` and `no` left them on, and
// the bridge went on sending what the operator had asked it to stop.
var switchEnvPaths = map[string]bool{
	"enrich.enabled":       true,
	"enrich.deezerEnabled": true,
}

// parseSwitchEnv reads an opt-out's variable as yaml.v3 reads a bool key,
// without regard to case, and as strconv.ParseBool reads one: on for y, yes,
// on, true, t and 1; off for n, no, off, false, f and 0; white space around
// the word ignored. ok is false for anything else.
func parseSwitchEnv(raw string) (on, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "y", "yes", "on", "true", "t", "1":
		return true, true
	case "n", "no", "off", "false", "f", "0":
		return false, true
	}
	return false, false
}

// readSwitchEnv is an opt-out's variable as the overrides take it: unset for
// a value of white space alone, which says nothing, as an empty one does; on
// or off as parseSwitchEnv reads it; and off, with unreadableSwitchEnv
// naming the variable and never its value, for anything else.
func readSwitchEnv(name, raw string) (on, set bool) {
	if strings.TrimSpace(raw) == "" {
		return false, false
	}
	if on, ok := parseSwitchEnv(raw); ok {
		return on, true
	}
	validateLogger.Warn(unreadableSwitchEnv, "var", name)
	return false, true
}

// EnrichEnabledFromEnv is enrich.enabled as BRIDGE_ENRICH_ENABLED alone sets
// it, read as Load reads it (readSwitchEnv); set is false where the variable
// is absent or says nothing. For a command that reads the switch where there
// may be no config file to load (bridge fingerprint).
func EnrichEnabledFromEnv() (on, set bool) {
	name := envName("enrich.enabled")
	v, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	return readSwitchEnv(name, v)
}

// applyEnvValue sets the leaf b names from raw, the value of the variable
// name, and reports whether it set it: false for a value it ignores (one
// that does not read as the leaf's boolean or whole number, an opt-out's
// white space) and for a leaf it cannot set.
func (c *Config) applyEnvValue(b envBinding, name, raw string) bool {
	fv := envLeaf(c, b)
	if !fv.CanSet() {
		return false
	}
	switch b.kind {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			validateLogger.Warn(unparseableBoolEnv, "var", name)
			return false
		}
		fv.SetBool(v)
	case reflect.Pointer:
		switch b.elemKind {
		case reflect.Bool:
			if switchEnvPaths[b.YAMLPath] {
				on, set := readSwitchEnv(name, raw)
				if set {
					fv.Set(reflect.ValueOf(&on))
				}
				return set
			}
			v, err := strconv.ParseBool(raw)
			if err != nil {
				validateLogger.Warn(unparseableBoolEnv, "var", name)
				return false
			}
			fv.Set(reflect.ValueOf(&v))
		case reflect.Int, reflect.Int64:
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				validateLogger.Warn(unparseableIntEnv, "var", name)
				return false
			}
			// Allocate through the field's own element type so an
			// *int and an *int64 field each get the right pointee.
			ptr := reflect.New(fv.Type().Elem())
			ptr.Elem().SetInt(n)
			fv.Set(ptr)
		default:
			return false
		}
	case reflect.Int, reflect.Int64:
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			validateLogger.Warn(unparseableIntEnv, "var", name)
			return false
		}
		fv.SetInt(v)
	case reflect.Slice:
		parts := strings.Split(raw, listSeparator(b.YAMLPath))
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		fv.Set(reflect.ValueOf(out))
	default:
		return false
	}
	return true
}

// envLeaf is the field b names in c, settable through c: every step on its
// way is a struct field held by value (envBindings walks no pointer to a
// struct), so setting it changes c alone.
func envLeaf(c *Config, b envBinding) reflect.Value {
	fv := reflect.ValueOf(c).Elem()
	for _, i := range b.index {
		fv = fv.Field(i)
	}
	return fv
}

// EnvOverrideDocs renders the recognised variables, one per line, for
// `bridge doctor` and the docs. Exported so the list an operator reads
// is generated from the same table the loader uses and cannot drift
// from it.
func EnvOverrideDocs() []string {
	bindings := envBindings()
	out := make([]string, 0, len(bindings)+len(legacyEnvAliases))
	for _, b := range bindings {
		out = append(out, fmt.Sprintf("%s\t%s", b.EnvVar, b.YAMLPath))
	}
	for name, path := range legacyEnvAliases {
		out = append(out, fmt.Sprintf("%s\t%s (legacy alias)", name, path))
	}
	return out
}
