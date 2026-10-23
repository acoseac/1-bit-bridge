// Package conf reads an MPD configuration file: the settings the bridge
// needs in order to find the daemon and to describe how it plays
// (where it listens, the outputs, replay gain, the resampler, the
// filters), and nothing else.
//
// The grammar is MPD's, from src/config/File.cxx and src/util/Tokenizer.cxx
// (v0.24.4): a line is a word, then a double-quoted value or a '{' that
// opens a block; '#' starts a comment, unless it is inside quotes; a
// backslash in a quote escapes the next byte and nothing more, so "\n"
// is the letter n. include and include_optional name a file relative to
// the file that names them.
//
// Two deliberate differences from MPD itself. MPD rejects a key it does
// not know, which would make a newer mpd.conf unreadable; this reader
// ignores a key it does not extract, and reports a value it cannot parse.
// MPD does not bound include recursion (MusicPlayerDaemon/MPD#2505); this
// reader refuses a cycle and a chain longer than maxIncludeDepth.
//
// A non-repeatable parameter keeps the last value: ReadConfigParam
// clears that option's list before adding the new one. bind_to_address
// and password are appended. A key repeated inside a block is a problem
// and the first value stands. A block keyword, or a '{', while a block
// is open closes that block with a problem; the next block's fields are
// not merged into it.
//
// A password value is never printed, serialised or put in an error. The
// field is unexported. What is removed is the whole value and, when the
// value is secret@permissions, the part before '@'. A secret shorter
// than four bytes is removed only where it is a whole token: a letter
// or a digit on either side keeps it, and '_', '/', '.' and '-' do not.
// String and JSON say only that one is set, and a problem about a
// password line does not quote the line.
package conf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxProblems bounds the problems one file reports. Past a handful the
// rest say nothing new; the last entry then counts the ones left out.
const maxProblems = 8

// maxQuoted bounds how much of a path a problem quotes. A password line
// is never quoted.
const maxQuoted = 64

// MaxConfBytes bounds one configuration file. A conf is a page of text;
// an include chain cannot make a read hold more than this per file.
const MaxConfBytes = 1 << 20

// maxLineBytes bounds one line. MPD's reader has no cap; a file that is
// one unterminated quote must still end.
const maxLineBytes = 64 << 10

// maxIncludeDepth is the longest include chain accepted below the file
// Read was given. MPD's own parser recurses with no limit.
const maxIncludeDepth = 16

// maxIncludeFiles bounds how many files one Read opens, so a wide tree
// of includes cannot walk the disk.
const maxIncludeFiles = 32

// DefaultPort is the port MPD listens on when mpd.conf sets none
// (Listen.cxx, DEFAULT_PORT).
const DefaultPort = 6600

// SystemPath is the last file MPD's Linux build searches: the
// SYSTEM_CONFIG_FILE_LOCATION a default build compiles in, /etc/mpd.conf.
const SystemPath = "/etc/mpd.conf"

// File is the settings this reader extracts from one mpd.conf, including
// the files it includes. A key the reader does not know is not here. A
// list is never nil.
type File struct {
	// Path is the file Read opened, as a logical path when it was given
	// one ("/etc/mpd.conf"). Parse leaves it empty.
	Path string `json:"path"`
	// MusicDirectory is the music_directory value, as written.
	MusicDirectory string `json:"musicDirectory"`
	// BindAddresses is every bind_to_address, in order. A path (a value
	// whose first character is '/', '~', '@' or '$') is a unix socket;
	// anything else is a TCP address. Endpoint picks which one to dial.
	BindAddresses []string `json:"bindAddresses"`
	// Port is the port setting. Nil means the file set none, and a TCP
	// dial uses DefaultPort.
	Port *int `json:"port"`
	// PasswordSet reports whether a top-level password line was read.
	// The value is not in this struct's exported fields.
	PasswordSet bool `json:"passwordSet"`
	// PasswordCount is how many top-level password lines were read. A
	// password inside an audio_output block is that output's own, and
	// is not counted: the line this reports is the one a client must
	// send to MPD.
	PasswordCount int `json:"passwordCount"`
	// Outputs is every audio_output block, in order.
	Outputs []AudioOutput `json:"outputs"`
	// Resampler is the resampler block. MPD allows one; a second is
	// reported and the first stands.
	Resampler Resampler `json:"resampler"`
	// SamplerateConverter is the older top-level setting of the same job.
	SamplerateConverter string `json:"samplerateConverter"`
	// ReplayGain is the replaygain setting, as written ("off", "album",
	// "track", "auto"). The live mode is a protocol reply, not this.
	ReplayGain string `json:"replayGain"`
	// VolumeNormalization is the volume_normalization setting.
	VolumeNormalization string `json:"volumeNormalization"`
	// AudioOutputFormat is the audio_output_format setting.
	AudioOutputFormat string `json:"audioOutputFormat"`
	// MixerType is the top-level mixer_type setting, deprecated in MPD and
	// still read: an output that sets neither mixer_type nor mixer_enabled
	// takes it, and "hardware" when it is unset too (src/output/Init.cxx,
	// audio_output_mixer_type).
	MixerType string `json:"mixerType"`
	// Filters is every filter block, in order. A block defines a filter;
	// an output applies it only when its filters setting names it.
	Filters []Filter `json:"filters"`
	// ZeroconfEnabled is the zeroconf_enabled setting.
	ZeroconfEnabled string   `json:"zeroconfEnabled"`
	Problems        []string `json:"problems"`

	passwords    []string
	hadResampler bool
}

// AudioOutput is one audio_output block. An omitted setting is an empty
// string, which for enabled means the file did not say: MPD then enables
// the output. The report layer applies that default; this struct does not,
// so a reader can tell an explicit "yes" from a line that was not there.
type AudioOutput struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	Device       string `json:"device"`
	Path         string `json:"path"`
	DOP          string `json:"dop"`
	MixerType    string `json:"mixerType"`
	MixerControl string `json:"mixerControl"`
	MixerDevice  string `json:"mixerDevice"`
	// MixerEnabled is the deprecated mixer_enabled setting, which MPD still
	// reads after mixer_type: "no" there is no mixer.
	MixerEnabled   string `json:"mixerEnabled"`
	Enabled        string `json:"enabled"`
	Format         string `json:"format"`
	AllowedFormats string `json:"allowedFormats"`
	// Filters is the output's filters setting as written: the names of the
	// filter blocks this output applies, separated by commas
	// (FilteredAudioOutput::Configure, filter_chain_parse).
	Filters string `json:"filters"`
}

// Resampler is the resampler block: the plugin (soxr, libsamplerate) and
// its quality. threads and the other knobs are not what the verdict asks.
type Resampler struct {
	Plugin  string `json:"plugin"`
	Quality string `json:"quality"`
}

// Filter is one filter block, its plugin and the name the file gave it.
type Filter struct {
	Name   string `json:"name"`
	Plugin string `json:"plugin"`
}

// passwordValues returns the top-level password values. It exists so the
// tests can prove those values never surface; nothing else calls it.
func (f File) passwordValues() []string {
	return append([]string(nil), f.passwords...)
}

// String is a summary a log can print. It says how many passwords are
// set and never a value.
func (f File) String() string {
	f = f.display()
	var b strings.Builder
	fmt.Fprintf(&b, "mpd conf %s\n", orUnset(f.Path))
	fmt.Fprintf(&b, "  music directory: %s\n", orUnset(f.MusicDirectory))
	if len(f.BindAddresses) == 0 {
		fmt.Fprintf(&b, "  bind: unset\n")
	} else {
		fmt.Fprintf(&b, "  bind: %s\n", strings.Join(f.BindAddresses, ", "))
	}
	if f.Port == nil {
		fmt.Fprintf(&b, "  port: unset\n")
	} else {
		fmt.Fprintf(&b, "  port: %d\n", *f.Port)
	}
	fmt.Fprintf(&b, "  password: %s\n", passwordPhrase(f.PasswordSet, f.PasswordCount))
	for _, o := range f.Outputs {
		fmt.Fprintf(&b, "  output %s: type %s, device %s, path %s, dop %s, mixer %s, enabled %s, filters %s\n",
			orUnset(o.Name), orUnset(o.Type), orUnset(o.Device), orUnset(o.Path),
			orUnset(o.DOP), mixerPhrase(o), orUnset(o.Enabled), orUnset(o.Filters))
	}
	fmt.Fprintf(&b, "  mixer type: %s\n", orUnset(f.MixerType))
	fmt.Fprintf(&b, "  replay gain: %s\n", orUnset(f.ReplayGain))
	fmt.Fprintf(&b, "  volume normalization: %s\n", orUnset(f.VolumeNormalization))
	if f.Resampler.Plugin == "" && f.Resampler.Quality == "" {
		fmt.Fprintf(&b, "  resampler: unset\n")
	} else {
		fmt.Fprintf(&b, "  resampler: %s (quality %s)\n", orUnset(f.Resampler.Plugin), orUnset(f.Resampler.Quality))
	}
	fmt.Fprintf(&b, "  samplerate converter: %s\n", orUnset(f.SamplerateConverter))
	fmt.Fprintf(&b, "  audio output format: %s\n", orUnset(f.AudioOutputFormat))
	if len(f.Filters) == 0 {
		fmt.Fprintf(&b, "  filters: none\n")
	} else {
		for _, fl := range f.Filters {
			fmt.Fprintf(&b, "  filter %s (%s)\n", orUnset(fl.Name), orUnset(fl.Plugin))
		}
	}
	fmt.Fprintf(&b, "  zeroconf: %s\n", orUnset(f.ZeroconfEnabled))
	for _, p := range f.Problems {
		fmt.Fprintf(&b, "  problem: %s\n", p)
	}
	return b.String()
}

// Format sends every verb through String, so %+v and %#v redact too.
func (f File) Format(s fmt.State, verb rune) {
	_, _ = io.WriteString(s, f.String())
}

// MarshalJSON is the same shape as the struct tags, with nil lists as []
// and no password value.
func (f File) MarshalJSON() ([]byte, error) {
	f.finish()
	shown := f.display()
	type out struct {
		Path                string        `json:"path"`
		MusicDirectory      string        `json:"musicDirectory"`
		BindAddresses       []string      `json:"bindAddresses"`
		Port                *int          `json:"port"`
		PasswordSet         bool          `json:"passwordSet"`
		PasswordCount       int           `json:"passwordCount"`
		Outputs             []AudioOutput `json:"outputs"`
		Resampler           Resampler     `json:"resampler"`
		SamplerateConverter string        `json:"samplerateConverter"`
		ReplayGain          string        `json:"replayGain"`
		VolumeNormalization string        `json:"volumeNormalization"`
		AudioOutputFormat   string        `json:"audioOutputFormat"`
		MixerType           string        `json:"mixerType"`
		Filters             []Filter      `json:"filters"`
		ZeroconfEnabled     string        `json:"zeroconfEnabled"`
		Problems            []string      `json:"problems"`
	}
	body, err := json.Marshal(out{
		Path: shown.Path, MusicDirectory: shown.MusicDirectory, BindAddresses: shown.BindAddresses,
		Port: shown.Port, PasswordSet: shown.PasswordSet, PasswordCount: shown.PasswordCount,
		Outputs: shown.Outputs, Resampler: shown.Resampler, SamplerateConverter: shown.SamplerateConverter,
		ReplayGain: shown.ReplayGain, VolumeNormalization: shown.VolumeNormalization,
		AudioOutputFormat: shown.AudioOutputFormat, MixerType: shown.MixerType, Filters: shown.Filters,
		ZeroconfEnabled: shown.ZeroconfEnabled, Problems: shown.Problems,
	})
	if err != nil {
		return nil, err
	}
	if leak := leakedSecret(body, f.redactionKeys()); leak != "" {
		return nil, errors.New("mpd conf: refusing to serialise a password")
	}
	return body, nil
}

// leakedSecret reports a password the JSON holds as a string value.
// The raw bytes are not enough: the word "password" is inside the keys
// passwordSet and passwordCount, and a password that is that word
// would refuse a document that does not contain the value. The check
// is the JSON encoding of the value, quotes included, so a short
// password does not match a longer string that merely contains it.
func leakedSecret(body []byte, passwords []string) string {
	for _, p := range passwords {
		if p == "" {
			continue
		}
		enc, err := json.Marshal(p)
		if err != nil || bytes.Contains(body, enc) {
			return p
		}
	}
	return ""
}

// Display is a copy safe to print. A password value is replaced in
// every string, including a setting that happens to be the password,
// and the copy does not carry the values.
func (f File) Display() File { return f.display() }

func (f File) display() File {
	pw := f.redactionKeys()
	out := f
	out.passwords = nil
	out.BindAddresses = append([]string(nil), f.BindAddresses...)
	out.Outputs = append([]AudioOutput(nil), f.Outputs...)
	out.Filters = append([]Filter(nil), f.Filters...)
	out.Problems = append([]string(nil), f.Problems...)
	out.Path = redactText(out.Path, pw)
	out.MusicDirectory = redactText(out.MusicDirectory, pw)
	out.SamplerateConverter = redactText(out.SamplerateConverter, pw)
	out.ReplayGain = redactText(out.ReplayGain, pw)
	out.VolumeNormalization = redactText(out.VolumeNormalization, pw)
	out.AudioOutputFormat = redactText(out.AudioOutputFormat, pw)
	out.MixerType = redactText(out.MixerType, pw)
	out.ZeroconfEnabled = redactText(out.ZeroconfEnabled, pw)
	out.Resampler.Plugin = redactText(out.Resampler.Plugin, pw)
	out.Resampler.Quality = redactText(out.Resampler.Quality, pw)
	for i, b := range out.BindAddresses {
		out.BindAddresses[i] = redactText(b, pw)
	}
	for i := range out.Outputs {
		o := &out.Outputs[i]
		o.Type = redactText(o.Type, pw)
		o.Name = redactText(o.Name, pw)
		o.Device = redactText(o.Device, pw)
		o.Path = redactText(o.Path, pw)
		o.DOP = redactText(o.DOP, pw)
		o.MixerType = redactText(o.MixerType, pw)
		o.MixerControl = redactText(o.MixerControl, pw)
		o.MixerDevice = redactText(o.MixerDevice, pw)
		o.MixerEnabled = redactText(o.MixerEnabled, pw)
		o.Enabled = redactText(o.Enabled, pw)
		o.Format = redactText(o.Format, pw)
		o.AllowedFormats = redactText(o.AllowedFormats, pw)
		o.Filters = redactText(o.Filters, pw)
	}
	for i := range out.Filters {
		out.Filters[i].Name = redactText(out.Filters[i].Name, pw)
		out.Filters[i].Plugin = redactText(out.Filters[i].Plugin, pw)
	}
	for i := range out.Problems {
		out.Problems[i] = redactText(out.Problems[i], pw)
	}
	if out.Port != nil {
		s := strconv.Itoa(*out.Port)
		if redactText(s, pw) != s {
			out.Port = nil
		}
	}
	return out
}

// shortSecret is the longest password that is redacted only as a whole
// token. A one-byte password is a real password (MPD accepts it), and
// it is also a letter that occurs in ordinary words: replacing every
// occurrence would turn "admin" into a lie. Four bytes is the point at
// which a substring replacement no longer collides with ordinary words
// in this report. A shorter secret is replaced only when the rune on
// either side is not a letter or a digit. '_', '/', '.' and '-' are
// boundaries, so a secret beside a socket name is removed and a secret
// inside a word such as "admin" is not.
const shortSecret = 4

// redactionKeys is every string that must not be printed: the whole
// password value, and, when the value is MPD's "secret@permissions"
// form (Permission.cxx splits on the first '@'), the secret on its own.
// Longer keys come first so "secret@read" is replaced whole rather than
// leaving the "@read".
func (f File) redactionKeys() []string {
	seen := map[string]struct{}{}
	var keys []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		keys = append(keys, s)
	}
	for _, p := range f.passwords {
		add(p)
		if i := strings.IndexByte(p, '@'); i > 0 {
			add(p[:i])
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return keys
}

// Redact returns s with every password value removed. Callers that
// print a path or an error from outside this file use it so a path
// that happens to contain the secret is not shown.
func (f File) Redact(s string) string {
	return redactText(s, f.redactionKeys())
}

// RedactSecrets returns s with each of passwords removed as a File removes
// its own: the whole value and, for MPD's secret@permissions, the secret
// before the '@', a value of fewer than four bytes only where it is a
// whole token. It is for a password that came from somewhere other than
// the file, such as the bridge's own output.password.
func RedactSecrets(s string, passwords ...string) string {
	return File{passwords: passwords}.Redact(s)
}

func redactText(s string, passwords []string) string {
	for _, p := range passwords {
		if p == "" || !strings.Contains(s, p) {
			continue
		}
		if len(p) < shortSecret {
			s = redactTokens(s, p)
			continue
		}
		s = strings.ReplaceAll(s, p, "[redacted]")
	}
	return s
}

// redactTokens replaces secret only where it is a whole token. The
// runes on either side must not be a letter or a digit.
func redactTokens(s, secret string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], secret)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		j += i
		if tokenBoundaryBefore(s, j) && tokenBoundaryAfter(s, j+len(secret)) {
			b.WriteString(s[i:j])
			b.WriteString("[redacted]")
			i = j + len(secret)
			continue
		}
		// The match sits inside a longer token. Keep the byte it
		// starts on and look again just after it, so the text before
		// the match is not dropped.
		b.WriteString(s[i : j+1])
		i = j + 1
	}
	return b.String()
}

func tokenBoundaryBefore(s string, i int) bool {
	if i <= 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return !isTokenRune(r)
}

func tokenBoundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return !isTokenRune(r)
}

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func (f *File) finish() {
	if f.BindAddresses == nil {
		f.BindAddresses = []string{}
	}
	if f.Outputs == nil {
		f.Outputs = []AudioOutput{}
	}
	if f.Filters == nil {
		f.Filters = []Filter{}
	}
	if f.Problems == nil {
		f.Problems = []string{}
	}
	f.PasswordSet = len(f.passwords) > 0
	f.PasswordCount = len(f.passwords)
	f.scrub()
}

// scrub removes a password value from the problem list. The messages are
// written without one; this is the backstop for a path that happens to
// be the password.
func (f *File) scrub() {
	keys := f.redactionKeys()
	for i := range f.Problems {
		f.Problems[i] = redactText(f.Problems[i], keys)
	}
}

func passwordPhrase(set bool, n int) string {
	if !set {
		return "not set"
	}
	if n > 1 {
		return fmt.Sprintf("set (%d)", n)
	}
	return "set"
}

func mixerPhrase(o AudioOutput) string {
	if o.MixerType == "" && o.MixerControl == "" && o.MixerDevice == "" && o.MixerEnabled == "" {
		return "unset"
	}
	parts := make([]string, 0, 4)
	if o.MixerType != "" {
		parts = append(parts, o.MixerType)
	}
	if o.MixerControl != "" {
		parts = append(parts, "control "+o.MixerControl)
	}
	if o.MixerDevice != "" {
		parts = append(parts, "device "+o.MixerDevice)
	}
	if o.MixerEnabled != "" {
		parts = append(parts, "enabled "+o.MixerEnabled)
	}
	return strings.Join(parts, ", ")
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

// Endpoint is where a client on this box should dial. A bind_to_address
// that names a socket (an absolute path, a '~' path, a Linux abstract
// name starting with '@', or a '$' expansion) wins, the first such in
// the file; otherwise the dial is 127.0.0.1 and the port setting, or
// DefaultPort when the file sets none. home expands a leading '~'.
//
// MPD's own rule for the socket is in the user manual's Listeners
// section: an absolute path or a path starting with '~', and '@' for an
// abstract socket. The manual also lets one TCP bind carry its own port
// ("localhost:6602"). This dials the port setting anyway: the bridge is
// a client on the same box, and the port setting is the one MPD applies
// when the address does not name one.
func (f File) Endpoint(home string) (addr, network string) {
	for _, b := range f.BindAddresses {
		expanded := expandBind(b, home)
		if isLocalSocket(b) || isLocalSocket(expanded) {
			if expanded == "" {
				expanded = b
			}
			return expanded, "unix"
		}
	}
	port := DefaultPort
	if f.Port != nil {
		port = *f.Port
	}
	return fmt.Sprintf("127.0.0.1:%d", port), "tcp"
}

// isLocalSocket reports whether a bind_to_address value is a unix socket
// rather than a TCP address.
func isLocalSocket(addr string) bool {
	if addr == "" {
		return false
	}
	switch addr[0] {
	case '/', '~', '@', '$':
		return true
	default:
		return false
	}
}

// expandBind applies the expansions MPD applies to a path setting that
// this reader needs in order to dial: a leading '~' and $HOME. The other
// path variables are left in place, so a value that still starts with '$'
// stays a socket path the caller can expand.
func expandBind(addr, home string) string {
	if home != "" {
		switch {
		case addr == "~":
			return home
		case strings.HasPrefix(addr, "~/"):
			return home + "/" + addr[2:]
		case strings.Contains(addr, "$HOME"):
			return strings.ReplaceAll(addr, "$HOME", home)
		}
	}
	return addr
}

// Parse reads one configuration file's text. include has nowhere to go
// and is reported; include_optional is skipped. Read is the call that
// follows includes.
func Parse(text string) (File, error) {
	if len(text) > MaxConfBytes {
		return File{}, fmt.Errorf("mpd conf longer than %d bytes", MaxConfBytes)
	}
	f := parseFile(text, "", "", nil)
	f.finish()
	return f, nil
}

type prober struct {
	file    *File
	name    string
	dir     string
	dropped int
	load    *loader
}

type loader struct {
	root  string
	depth int
	stack map[string]struct{}
	files int
}

func parseFile(text, name, dir string, load *loader) File {
	var f File
	p := &prober{file: &f, name: name, dir: dir, load: load}
	if load != nil {
		load.files++
	}
	lines := splitLines(text)
	inBlock := false
	var block *blockState
	for i, line := range lines {
		ln := i + 1
		if len(line) > maxLineBytes {
			p.add(ln, "line longer than %d bytes", maxLineBytes)
			continue
		}
		trim := trimLeft(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if inBlock {
			if strings.HasPrefix(trim, "}") {
				rest := trimLeft(trim[1:])
				if rest != "" && !strings.HasPrefix(rest, "#") {
					p.add(ln, "trailing tokens after '}'")
				}
				p.closeBlock(block)
				inBlock = false
				block = nil
				continue
			}
			// MPD's config_read_block accepts only a name and a quoted
			// value until '}'. A block keyword here means the previous
			// block was not closed (File.cxx throws, so the file is not
			// used). Close what we have and start the new block, so the
			// new block's fields cannot land on the previous one.
			if word, rest, wok := nextWord(trim); wok && strings.HasPrefix(trimLeft(rest), "{") {
				p.add(ln, "block %s is not closed", block.kind)
				p.closeBlock(block)
				after := trimLeft(trimLeft(rest)[1:])
				if after != "" && !strings.HasPrefix(after, "#") {
					p.add(ln, "trailing tokens after '{'")
					inBlock = false
					block = nil
					continue
				}
				block = newBlock(word)
				continue
			}
			if strings.HasPrefix(trim, "{") {
				p.add(ln, "block %s is not closed", block.kind)
				p.closeBlock(block)
				inBlock = false
				block = nil
				continue
			}
			key, value, ok := p.assignment(ln, trim, true)
			if !ok {
				continue
			}
			block.set(ln, key, value, p)
			continue
		}
		word, rest, ok := nextWord(trim)
		if !ok {
			p.add(ln, "a setting name was expected")
			continue
		}
		rest = trimLeft(rest)
		if strings.HasPrefix(rest, "{") {
			after := trimLeft(rest[1:])
			if after != "" && !strings.HasPrefix(after, "#") {
				p.add(ln, "trailing tokens after '{'")
				continue
			}
			inBlock = true
			block = newBlock(word)
			continue
		}
		if word == "include" || word == "include_optional" {
			value, _, ok := p.quoted(ln, rest, false)
			if !ok {
				continue
			}
			p.include(ln, value, word == "include_optional")
			continue
		}
		value, _, ok := p.quoted(ln, rest, word == "password")
		if !ok {
			continue
		}
		p.setScalar(ln, word, value)
	}
	if inBlock {
		p.add(0, "block %s is not closed", block.kind)
	}
	if p.dropped > 0 {
		p.file.Problems = append(p.file.Problems, fmt.Sprintf("%d more not shown", p.dropped))
	}
	return f
}

func splitLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func (p *prober) add(line int, format string, args ...any) {
	if len(p.file.Problems) >= maxProblems {
		p.dropped++
		return
	}
	msg := fmt.Sprintf(format, args...)
	where := ""
	if p.name != "" {
		where = p.name + ": "
	}
	if line > 0 {
		msg = fmt.Sprintf("%sline %d: %s", where, line, msg)
	} else if where != "" {
		msg = where + msg
	}
	p.file.Problems = append(p.file.Problems, msg)
}

func (p *prober) assignment(line int, trim string, inBlock bool) (key, value string, ok bool) {
	word, rest, wok := nextWord(trim)
	if !wok {
		p.add(line, "a setting name was expected")
		return "", "", false
	}
	// A password key is the sensitive one at the top of the file. Inside
	// a block the same word belongs to the output (a shout stream's
	// password) and is discarded with the other keys we do not keep, but
	// it is still not quoted in a problem.
	value, _, qok := p.quoted(line, rest, word == "password")
	if !qok {
		return "", "", false
	}
	_ = inBlock
	return word, value, true
}

// quoted parses one double-quoted value. When secret is set, a failure
// names the key and not the bytes: the line is a password line.
func (p *prober) quoted(line int, rest string, secret bool) (value, leftover string, ok bool) {
	value, leftover, err := nextQuoted(trimLeft(rest))
	if err != nil {
		if secret {
			p.add(line, "password: %s", err.Error())
		} else {
			p.add(line, "%s", err.Error())
		}
		return "", "", false
	}
	return value, leftover, true
}

// setScalar records one top-level parameter. A non-repeatable
// parameter keeps the last value: MPD 0.24.4 ReadConfigParam
// (src/config/File.cxx) clears that option's list before adding the
// new value, and GetParam (src/config/Data.hxx) returns the only
// element left. bind_to_address and password are the repeatable
// parameters (Templates.cxx) and are appended. A second resampler
// block is the other shape: ReadConfigBlock throws, and closeBlock
// keeps the first.
func (p *prober) setScalar(line int, key, value string) {
	switch key {
	case "music_directory":
		p.file.MusicDirectory = value
	case "bind_to_address":
		p.file.BindAddresses = append(p.file.BindAddresses, value)
	case "port":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 65535 {
			p.add(line, "port %q is not a TCP port", value)
			return
		}
		p.file.Port = &n
	case "password":
		p.file.passwords = append(p.file.passwords, value)
	case "replaygain":
		p.file.ReplayGain = value
	case "volume_normalization":
		p.file.VolumeNormalization = value
	case "audio_output_format":
		p.file.AudioOutputFormat = value
	case "mixer_type":
		p.file.MixerType = value
	case "samplerate_converter":
		p.file.SamplerateConverter = value
	case "zeroconf_enabled":
		p.file.ZeroconfEnabled = value
	default:
		// A key this reader does not extract. MPD would reject it; a
		// newer file would then yield nothing. Ignored on purpose.
	}
}

type blockState struct {
	kind string
	seen map[string]int
	out  AudioOutput
	res  Resampler
	flt  Filter
}

func newBlock(kind string) *blockState {
	return &blockState{kind: kind, seen: map[string]int{}}
}

// set records one line of a block. File.cxx config_read_name_value
// throws when GetBlockParam already has the name, for every name,
// including ones this reader does not extract. The first value is the
// one GetBlockParam would have returned; the line is a problem either
// way, and the message does not quote the value.
func (b *blockState) set(line int, key, value string, p *prober) {
	if prev, ok := b.seen[key]; ok {
		p.add(line, "%s repeats %s, first set on line %d", b.kind, key, prev)
		return
	}
	b.seen[key] = line
	switch b.kind {
	case "audio_output":
		switch key {
		case "type":
			b.out.Type = value
		case "name":
			b.out.Name = value
		case "device":
			b.out.Device = value
		case "path":
			b.out.Path = value
		case "dop":
			b.out.DOP = value
		case "mixer_type":
			b.out.MixerType = value
		case "mixer_control":
			b.out.MixerControl = value
		case "mixer_device":
			b.out.MixerDevice = value
		case "mixer_enabled":
			b.out.MixerEnabled = value
		case "enabled":
			b.out.Enabled = value
		case "format":
			b.out.Format = value
		case "allowed_formats":
			b.out.AllowedFormats = value
		case "filters":
			b.out.Filters = value
		}
	case "resampler":
		switch key {
		case "plugin":
			b.res.Plugin = value
		case "quality":
			b.res.Quality = value
		}
	case "filter":
		switch key {
		case "name":
			b.flt.Name = value
		case "plugin":
			b.flt.Plugin = value
		}
	default:
		// decoder, input, database, and the rest: parsed so a bad
		// quote is still a problem, and not extracted.
	}
}

func (p *prober) closeBlock(b *blockState) {
	if b == nil {
		return
	}
	switch b.kind {
	case "audio_output":
		p.file.Outputs = append(p.file.Outputs, b.out)
	case "filter":
		p.file.Filters = append(p.file.Filters, b.flt)
	case "resampler":
		if p.file.hadResampler {
			p.add(0, "a second resampler block is ignored")
			return
		}
		p.file.Resampler = b.res
		p.file.hadResampler = true
	}
}

func (p *prober) include(line int, name string, optional bool) {
	// MPD expands a pattern in either directive; this reader does not, so
	// the files MPD reads through one are not read here, and a pattern is
	// never a missing file include_optional may skip.
	if strings.ContainsAny(name, "*?[") {
		p.add(line, "include %s: a pattern is not followed", quotePath(name))
		return
	}
	if p.load == nil {
		if !optional {
			p.add(line, "include %s: not followed", quotePath(name))
		}
		return
	}
	host, err := p.load.resolve(p.dir, name)
	if err != nil {
		// A path that leaves the root is refused for either directive.
		// include_optional only swallows a file that is not there.
		p.add(line, "include %s: %s", quotePath(name), cleanErr(err))
		return
	}
	if _, cyc := p.load.stack[host]; cyc {
		p.add(line, "include %s: cycle", quotePath(name))
		return
	}
	if p.load.depth >= maxIncludeDepth {
		p.add(line, "include %s: nested more than %d files", quotePath(name), maxIncludeDepth)
		return
	}
	if p.load.files >= maxIncludeFiles {
		p.add(line, "include %s: more than %d files", quotePath(name), maxIncludeFiles)
		return
	}
	text, err := p.load.open(host)
	if err != nil {
		if optional && errors.Is(err, errNotFound) {
			return
		}
		p.add(line, "include %s: %s", quotePath(name), cleanErr(err))
		return
	}
	p.load.stack[host] = struct{}{}
	p.load.depth++
	child := parseFile(text, logicalFromHost(p.load.root, host), filepath.Dir(host), p.load)
	p.load.depth--
	delete(p.load.stack, host)
	mergeFile(p.file, child)
}

func mergeFile(dst *File, src File) {
	if src.MusicDirectory != "" {
		dst.MusicDirectory = src.MusicDirectory
	}
	dst.BindAddresses = append(dst.BindAddresses, src.BindAddresses...)
	if src.Port != nil {
		dst.Port = src.Port
	}
	dst.passwords = append(dst.passwords, src.passwords...)
	dst.Outputs = append(dst.Outputs, src.Outputs...)
	if src.hadResampler {
		if dst.hadResampler {
			dst.Problems = append(dst.Problems, "a second resampler block is ignored")
		} else {
			dst.Resampler = src.Resampler
			dst.hadResampler = true
		}
	}
	if src.SamplerateConverter != "" {
		dst.SamplerateConverter = src.SamplerateConverter
	}
	if src.ReplayGain != "" {
		dst.ReplayGain = src.ReplayGain
	}
	if src.VolumeNormalization != "" {
		dst.VolumeNormalization = src.VolumeNormalization
	}
	if src.AudioOutputFormat != "" {
		dst.AudioOutputFormat = src.AudioOutputFormat
	}
	if src.MixerType != "" {
		dst.MixerType = src.MixerType
	}
	dst.Filters = append(dst.Filters, src.Filters...)
	if src.ZeroconfEnabled != "" {
		dst.ZeroconfEnabled = src.ZeroconfEnabled
	}
	dst.Problems = append(dst.Problems, src.Problems...)
}

func nextWord(s string) (word, rest string, ok bool) {
	if s == "" {
		return "", "", false
	}
	r, w := utf8.DecodeRuneInString(s)
	if !unicode.IsLetter(r) || r > unicode.MaxASCII {
		return "", s, false
	}
	i := w
	for i < len(s) {
		r, w = utf8.DecodeRuneInString(s[i:])
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			break
		}
		i += w
	}
	return s[:i], s[i:], true
}

func nextQuoted(s string) (value, rest string, err error) {
	if s == "" || s[0] != '"' {
		return "", s, errors.New("quoted value expected")
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", "", errors.New("backslash at end of value")
			}
			i++
			b.WriteByte(s[i])
		case '"':
			rest = trimLeft(s[i+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return "", "", errors.New("trailing tokens after value")
			}
			return b.String(), rest, nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", errors.New("missing closing quote")
}

func trimLeft(s string) string {
	return strings.TrimLeft(s, " \t")
}

func quotePath(s string) string {
	if len(s) > maxQuoted {
		return strconv.Quote(s[:maxQuoted]) + "…"
	}
	return strconv.Quote(s)
}

func cleanErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
