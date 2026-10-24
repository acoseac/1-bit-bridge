package config

import (
	"errors"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValueFreeYAMLError is err, an error gopkg.in/yaml.v3 returned for a config
// file, rebuilt from the parts of its message that hold none of the file's
// text: the line, the Go type a value was decoded into, a standard YAML tag
// and the decoder's own words. A config holds MPD's password and the
// player's serial, and what yaml.v3 refuses there is often one of them
// pasted onto the wrong line or written unquoted, while `bridge serve`,
// `bridge doctor` and init print the refusal and a service's journal keeps
// it (backlog B505, round 3). yaml.v3 v3.0.1 quoted a value it could not
// decode whole when it was ten bytes or fewer and its first seven bytes
// otherwise, wrote a value tagged !!map or !!seq whole and unquoted, named a
// key it does not take and a key written twice, printed whole a value its
// explicit tag does not fit, and named the anchor of an alias that names
// none, which is what a password starting with * is when it is not quoted.
// Keys go as values do: a scalar written alone in a flow block is a key, and
// output.player's reasons say "that key" for the same reason. A tag of the
// file's own goes too, since a value starting with ! is one.
//
// Each line stays, so the operator finds in the file what a refusal is
// about. The parser's own messages (a line, and a problem from the
// parser's fixed set) are kept as they are. An error that is not
// yaml.v3's own, from a type's own unmarshaler, is replaced, since its text
// is that type's. io.EOF, an empty file, and nil are returned as they are.
func ValueFreeYAMLError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	var te *yaml.TypeError
	if errors.As(err, &te) {
		lines := make([]string, len(te.Errors))
		for i, e := range te.Errors {
			lines[i] = valueFreeTypeError(e)
		}
		return &yaml.TypeError{Errors: lines}
	}
	return errors.New(valueFreeYAMLMessage(err.Error()))
}

var (
	// yamlLine is a message of yaml.v3's that names the line it is about.
	yamlLine = regexp.MustCompile(`(?s)^line ([0-9]+): (.*)$`)
	// yamlGoType is the type yaml.v3 says it decoded into: a Go type's name,
	// which the file never writes.
	yamlGoType = regexp.MustCompile(`^[A-Za-z0-9_.*\[\]{} ]+$`)
	// yamlLineNumber is a line number and nothing else.
	yamlLineNumber = regexp.MustCompile(`^[0-9]+$`)
)

// standardYAMLTags are the tags yaml.v3 resolves a value to without one
// written: the only tags a refusal names.
var standardYAMLTags = map[string]bool{
	"!!str": true, "!!int": true, "!!bool": true, "!!float": true,
	"!!null": true, "!!timestamp": true, "!!binary": true, "!!merge": true,
}

// valueFreeTypeError is one line of a *yaml.TypeError without the file's
// text: its line, and what yaml.v3 could not do there.
func valueFreeTypeError(e string) string {
	m := yamlLine.FindStringSubmatch(e)
	if m == nil {
		return "a value does not read"
	}
	at, rest := "line "+m[1]+": ", m[2]
	if body, ok := strings.CutPrefix(rest, "cannot unmarshal "); ok {
		// "cannot unmarshal <tag><value> into <type>": the type is
		// last, and holds no " into ", so the last one ends the value.
		if i := strings.LastIndex(body, " into "); i >= 0 && yamlGoType.MatchString(body[i+len(" into "):]) {
			return at + "cannot unmarshal " + valueFreeTagged(body[:i]) + " into " + body[i+len(" into "):]
		}
	}
	if strings.HasPrefix(rest, "field ") {
		for _, s := range []struct{ sep, says string }{
			{" not found in type ", "that key is not a field of type "},
			{" already set in type ", "that key names a field already set in type "},
		} {
			if i := strings.LastIndex(rest, s.sep); i >= 0 && yamlGoType.MatchString(rest[i+len(s.sep):]) {
				return at + s.says + rest[i+len(s.sep):]
			}
		}
	}
	if strings.HasPrefix(rest, "mapping key ") {
		const sep = " already defined at line "
		if i := strings.LastIndex(rest, sep); i >= 0 && yamlLineNumber.MatchString(rest[i+len(sep):]) {
			return at + "that key is written twice in one block, first at line " + rest[i+len(sep):]
		}
	}
	return at + "a value does not read"
}

// valueFreeTagged is what yaml.v3 says it could not decode, without the
// value: !!map and !!seq as they are (yaml.v3 quotes no value for a block
// or a list), a value by its standard tag, and anything else as "that
// value", a tag the file wrote included. What is said so already, "a
// !!float value" (a line CheckWholeNumbers wrote, or one this rebuilt), is
// kept as it is: yaml.v3's own always starts with the tag.
func valueFreeTagged(what string) string {
	switch what {
	case "!!map", "!!seq":
		return what
	}
	if tag, _, ok := strings.Cut(what, " `"); ok && standardYAMLTags[tag] && strings.HasSuffix(what, "`") {
		return "a " + tag + " value"
	}
	if tag, ok := strings.CutPrefix(what, "a "); ok {
		if tag, ok = strings.CutSuffix(tag, " value"); ok && standardYAMLTags[tag] {
			return what
		}
	}
	return "that value"
}

// valueFreeYAMLMessage is a message of an error yaml.v3 returned that is not
// a *yaml.TypeError, without the file's text.
func valueFreeYAMLMessage(msg string) string {
	rest, ok := strings.CutPrefix(msg, "yaml: ")
	if !ok {
		return "yaml: a value does not read as its setting"
	}
	switch {
	case strings.HasPrefix(rest, "cannot decode "):
		// "cannot decode <resolved tag> `<value>` as a <tag written>".
		resolved, _, _ := strings.Cut(strings.TrimPrefix(rest, "cannot decode "), " `")
		if i := strings.LastIndex(rest, "` as a "); i >= 0 {
			if written := rest[i+len("` as a "):]; standardYAMLTags[resolved] && standardYAMLTags[written] {
				return "yaml: cannot decode a " + resolved + " value as a " + written
			}
		}
		return "yaml: a value does not read as the tag written on it"
	case strings.HasPrefix(rest, "unknown anchor "):
		return "yaml: an alias names an anchor the file does not define"
	case strings.HasPrefix(rest, "anchor ") && strings.HasSuffix(rest, " value contains itself"):
		return "yaml: an alias names the block that holds it"
	case strings.HasPrefix(rest, "invalid map key"):
		return "yaml: a key is a block or a list"
	}
	return msg
}
