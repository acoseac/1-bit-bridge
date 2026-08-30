package config

import (
	"fmt"
	"os"
	"reflect"
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

// envBinding is one settable leaf: where it lives and what it is called.
type envBinding struct {
	YAMLPath string
	EnvVar   string
	// index is the reflect field-index path from the Config root, which
	// is what lets a binding be resolved against any *Config rather
	// than only the one it was discovered on.
	index []int
	kind  reflect.Kind
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
				// Pointer-to-bool is the codebase's "unset means default
				// on" idiom (SmartPlaylists.Enabled). An env override
				// allocates, which is exactly the explicit choice the
				// pointer exists to represent.
				if ft.Elem().Kind() == reflect.Bool {
					out = append(out, envBinding{
						YAMLPath: path, EnvVar: envName(path), index: here, kind: ft.Kind(),
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
// notice than one that ignored an override and said so.
func (c *Config) applyEnvOverrides() {
	byPath := make(map[string]envBinding, 128)
	for _, b := range envBindings() {
		byPath[b.YAMLPath] = b
		if v, ok := os.LookupEnv(b.EnvVar); ok && v != "" {
			c.applyEnvValue(b, b.EnvVar, v)
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
		if v, ok := os.LookupEnv(name); ok && v != "" {
			c.applyEnvValue(b, name, v)
		}
	}
}

func (c *Config) applyEnvValue(b envBinding, name, raw string) {
	fv := reflect.ValueOf(c).Elem()
	for _, i := range b.index {
		fv = fv.Field(i)
	}
	if !fv.CanSet() {
		return
	}
	switch b.kind {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			validateLogger.Warn("ignoring unparseable boolean env var", "var", name, "value", raw, "err", err)
			return
		}
		fv.SetBool(v)
	case reflect.Pointer:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			validateLogger.Warn("ignoring unparseable boolean env var", "var", name, "value", raw, "err", err)
			return
		}
		fv.Set(reflect.ValueOf(&v))
	case reflect.Int, reflect.Int64:
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			validateLogger.Warn("ignoring unparseable integer env var", "var", name, "value", raw, "err", err)
			return
		}
		fv.SetInt(v)
	case reflect.Slice:
		// OS-native PATH separator: `:` on POSIX, `;` on Windows, so a
		// drive-letter path (C:\Music) is not split in half. Container
		// deployments are linux, where this is `:`, so existing
		// docker-compose and k8s manifests keep working unchanged.
		parts := strings.Split(raw, string(os.PathListSeparator))
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		fv.Set(reflect.ValueOf(out))
	}
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
