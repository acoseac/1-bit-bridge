// Package formatgate is the one definition of the formats the bridge
// indexes and serves only to a client that says it plays them (backlog
// B489): WavPack (.wv) and Monkey's Audio (.ape).
//
// An app from before B489 decodes neither format, and a manifest row it is
// sent is a track it lists, so a library of these files would reach such an
// app as tracks that fail to play. So a catalog surface leaves these rows out
// unless the request declares the format in X-Bridge-Formats (Parse), and
// /v1/health advertises the header's support (the wavpackApe feature) so a
// new app knows to send it.
//
// A row is gated by its PATH's extension (Of), never by its codec: a file
// its extractor refused has no codec, a tombstone has no row left to read
// one from, and an upstream UPnP row carries the extension its URL gave it
// and no parsed header. Every reader of the gate asks this package, and the
// manifest package's SQL twin (the gated-format predicates beside the
// served-set readers) is held to Of by a test of the two over the same
// paths.
//
// The package imports nothing of the bridge's, so the manifest and the
// duplicates elector (internal/dupes, which internal/manifest imports) can
// both ask it.
package formatgate

import (
	"path"
	"sort"
	"strings"
)

// HeaderName is the request header a client declares the gated formats it
// plays in: a comma-separated list of tokens (TokenWavPack, TokenAPE),
// matched without regard to case, an unknown token ignored.
const HeaderName = "X-Bridge-Formats"

// The tokens of HeaderName.
const (
	TokenWavPack = "wavpack"
	TokenAPE     = "ape"
)

// Format is a format the gate holds back from a client that does not
// declare it, or None.
type Format uint8

const (
	// None is every path the gate serves to every client.
	None Format = iota
	// WavPack is a .wv file, served only to a client that declares
	// TokenWavPack. Its correction file (.wvc) is never a track.
	WavPack
	// APE is a Monkey's Audio .ape file, served only to a client that
	// declares TokenAPE.
	APE
)

// Of is the gated format a manifest path's extension names, compared
// without regard to case: ".wv" is WavPack, ".ape" APE, anything else
// None. The extension is path.Ext's, the part from the last dot of the
// path's last element, as the scanner's walk reads one.
func Of(p string) Format {
	switch strings.ToLower(path.Ext(p)) {
	case ".wv":
		return WavPack
	case ".ape":
		return APE
	}
	return None
}

// Gated reports whether Of(p) is a gated format: a WavPack or Monkey's Audio
// file, which the bridge serves as it is and decodes nothing of. The
// consumers that leave these files alone ask it (the duplicates elector, the
// rendition gates, analysis, the bridge's own player and the DLNA adapter),
// each for its own reason, given where it asks.
func Gated(p string) bool { return Of(p) != None }

// Token is the header token that declares f, or "" for None.
func (f Format) Token() string {
	switch f {
	case WavPack:
		return TokenWavPack
	case APE:
		return TokenAPE
	}
	return ""
}

// Set is the gated formats one request declared. The zero value declares
// none: the view of a client that sends no header, or one that names no
// token this bridge knows, which is every app before B489's.
type Set struct {
	wavpack bool
	ape     bool
}

// All declares every gated format: the operator's view (the console, the
// scanner's own bookkeeping), and the bridge's internal readers that serve
// no client.
func All() Set { return Set{wavpack: true, ape: true} }

// SetOf builds a set declaring the given formats.
func SetOf(formats ...Format) Set {
	var s Set
	for _, f := range formats {
		switch f {
		case WavPack:
			s.wavpack = true
		case APE:
			s.ape = true
		}
	}
	return s
}

// Parse reads the values of HeaderName: each a comma-separated list of
// tokens, each token trimmed of white space and matched without regard to
// case, a token this bridge does not know ignored. Several header lines
// add up. No value, or none naming a known token, is the zero Set.
func Parse(values []string) Set {
	var s Set
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			switch strings.ToLower(strings.TrimSpace(tok)) {
			case TokenWavPack:
				s.wavpack = true
			case TokenAPE:
				s.ape = true
			}
		}
	}
	return s
}

// Declares reports whether the set declares f. None is declared by every
// set.
func (s Set) Declares(f Format) bool {
	switch f {
	case WavPack:
		return s.wavpack
	case APE:
		return s.ape
	}
	return true
}

// Takes reports whether a client with this set is served the row or
// tombstone at the manifest path p.
func (s Set) Takes(p string) bool { return s.Declares(Of(p)) }

// WavPack reports whether the set declares WavPack: one of the SQL binds
// that stand for Takes in a query (one bind per gated format).
func (s Set) WavPack() bool { return s.wavpack }

// APE reports whether the set declares APE, the other bind.
func (s Set) APE() bool { return s.ape }

// IsZero reports whether the set declares no gated format.
func (s Set) IsZero() bool { return !s.wavpack && !s.ape }

// Key is the set as a stable string: its tokens, sorted and comma-joined, ""
// for the zero set. It names a view in a cache key, a feed's version and a
// page cursor, so two sets are the same view exactly when their keys are
// equal.
func (s Set) Key() string {
	var toks []string
	if s.wavpack {
		toks = append(toks, TokenWavPack)
	}
	if s.ape {
		toks = append(toks, TokenAPE)
	}
	sort.Strings(toks)
	return strings.Join(toks, ",")
}

// ParseKey is Key's inverse: the set a key names, and false for a string Key
// never returns.
func ParseKey(key string) (Set, bool) {
	if key == "" {
		return Set{}, true
	}
	var s Set
	for _, tok := range strings.Split(key, ",") {
		switch tok {
		case TokenWavPack:
			s.wavpack = true
		case TokenAPE:
			s.ape = true
		default:
			return Set{}, false
		}
	}
	if s.Key() != key {
		return Set{}, false
	}
	return s, true
}
