package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// A console save over a config file another writer changed since the live
// holder read it (backlog B535): the holder records what that writer changed,
// setting by setting, and every save writes those settings as the file holds
// them beside its own change. Until B535 every such save was refused until a
// restart, whatever it changed: the holder compared the whole file's bytes.

// outsideSettingMessage is what the holder logs, once, for each setting it
// finds another writer changed in the config file, the setting by its yaml
// path and never its value.
const outsideSettingMessage = "config: another writer changed a setting in the config file; the console's saves keep the file's value, which takes effect when the bridge next starts"

// deploymentSettings is the yaml path of the deployment block, whose
// settings decide what the console may change.
const deploymentSettings = "deployment"

var (
	playerConfigType  = reflect.TypeFor[PlayerConfig]()
	yamlMarshalerType = reflect.TypeFor[yaml.Marshaler]()
)

// compareTurn compares the config file a turn found (found, and ok whether
// there was one; target, the file the config's name opened) with the bytes
// this holder last read or wrote, before the turn's change runs. A holder told
// nothing compares nothing. A file gone, and a name that opens another file
// than the one this holder read, are refused. Where the bytes differ, each is
// read as Load reads a file for its settings (readSettings), and each setting
// they set differently (settingsChanged) joins the record (outside), logged
// once, the turn's bytes then the ones the record is kept against, whatever
// the save does next; a setting a BRIDGE_* variable set at the start never
// joins it, since B761's rule writes it, the file's value where the save left
// it and the change where it made one (filedEnvValues, filedSwitches). A turn's
// file that does not decode is refused, the record as it was. Then, whatever
// the bytes, a recorded deployment setting refuses the save: the console
// decides what it may change from its live copy of the posture and the
// managed lists.
func (r *RuntimeConfig) compareTurn(path, target string, found []byte, ok bool, before *Config) error {
	if !r.fileKnown {
		return nil
	}
	switch {
	case target != r.target:
		return &ChangedOutsideError{Path: path, Kind: OutsideRepointed}
	case !ok:
		return &ChangedOutsideError{Path: path, Kind: OutsideRemoved}
	}
	if !bytes.Equal(found, r.file) {
		was, errWas := readSettings(path, r.file)
		now, errNow := readSettings(path, found)
		if errWas != nil || errNow != nil {
			return &ChangedOutsideError{Path: path, Kind: OutsideUndecodable}
		}
		for _, setting := range settingsChanged(was, now) {
			if slices.Contains(before.envSet, setting) || slices.Contains(r.outside, setting) {
				continue
			}
			r.outside = append(r.outside, setting)
			validateLogger.Info(outsideSettingMessage, "setting", setting)
		}
		slices.Sort(r.outside)
		r.file = found
	}
	if deployment := settingsUnder(r.outside, deploymentSettings); len(deployment) > 0 {
		return &ChangedOutsideError{Path: path, Kind: OutsideDeployment, Settings: deployment}
	}
	return nil
}

// refuseMerge is the save's check of next, the clone fn changes, while
// settings another writer changed are recorded: once fn returns and at each
// check fn asks before work of its own. A setting next changes, against
// before, that another writer changed, or that holds or is held by one (a
// player block and its fields), is refused (OutsideConflict, naming the
// recorded settings it meets): the change was made from a value the console
// never read, and saving it would lose that writer's in silence. Then the file
// the save would write (fileSaved over a copy of next) is loaded as the next
// start loads it (loadsAtNextStart), and one it would not load is refused
// (OutsideUnloadable). With nothing recorded it asks nothing: the save is the
// one the holder made before B535.
func (r *RuntimeConfig) refuseMerge(path string, before, next *Config, found []byte) error {
	if len(r.outside) == 0 {
		return nil
	}
	changed := settingsChanged(before, next)
	var conflict []string
	for _, setting := range r.outside {
		if slices.ContainsFunc(changed, func(c string) bool { return overlap(setting, c) }) {
			conflict = append(conflict, setting)
		}
	}
	if len(conflict) > 0 {
		return &ChangedOutsideError{Path: path, Kind: OutsideConflict, Settings: conflict}
	}
	merged := *next
	r.fileSaved(&merged, before, found)
	data, err := merged.marshal()
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := loadsAtNextStart(path, data); err != nil {
		return &ChangedOutsideError{Path: path, Kind: OutsideUnloadable, Settings: slices.Clone(r.outside), Err: err}
	}
	return nil
}

// fileSaved sets, in saved, a copy of an update's next, every setting the
// file is to hold as the file holds it, read from found, the bytes the turn
// found: the three opt-outs (filedSwitches), every other leaf a variable set
// (filedEnvValues), and every setting another writer changed (fileOutside).
func (r *RuntimeConfig) fileSaved(saved, before *Config, found []byte) {
	filedSwitches(saved, before, found)
	filedEnvValues(saved, before, found)
	r.fileOutside(saved, found)
}

// fileOutside sets, in saved, each setting another writer changed (outside)
// as the file holds it, read from found by Load's own decode (DecodeFile) and
// assigned to saved's own field, never through a pointer the published
// snapshot holds, its yaml path recorded in saved.filed, so that a field of
// Config's own whose tag writes its zero value is written as no key at that
// value (marshal), which Load reads as it reads that value; a setting below the
// top level omits its zero value by its own tag. A file that does not decode
// leaves saved as it is: compareTurn refused it at the turn that found it.
func (r *RuntimeConfig) fileOutside(saved *Config, found []byte) {
	if len(r.outside) == 0 {
		return
	}
	var own Config
	if err := DecodeFile(found, &own); err != nil {
		return
	}
	for _, setting := range r.outside {
		dst, ok := settingValue(saved, setting)
		if !ok {
			continue
		}
		src, _ := settingValue(&own, setting)
		dst.Set(src)
		saved.filed = append(saved.filed, setting)
	}
}

// readSettings is the config raw holds, a config file's bytes, read as Load
// reads the file at path for its settings and nothing more: decoded
// (DecodeFile), its defaults given and its relative paths resolved against
// the directory Load takes from path, then Normalize's forms, all without a
// word, and no BRIDGE_* variable applied. So a writer that wrote out a
// default, a resolved path or the library name Normalize gives (config.Edit
// writes all three) changed nothing. Nothing is validated, so a file the next
// start would refuse still has its settings; its error is the decode's.
func readSettings(path string, raw []byte) (*Config, error) {
	var c Config
	if err := DecodeFile(raw, &c); err != nil {
		return nil, err
	}
	c.applyDefaultsAs(false)
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.resolvePaths(filepath.Dir(abs))
	c.normalizeSettings()
	return &c, nil
}

// normalizeSettings is normalize, quietly, for a file's settings: where an
// enrich base URL is one normalize refuses, which leaves the whole config as
// it was, each base URL is kept in the form normalizeBaseURL gives it, or as
// written where it gives none, and everything else is normalized all the same.
func (c *Config) normalizeSettings() {
	if c.normalize(false) == nil {
		return
	}
	bases := []*string{&c.Enrich.MusicBrainzBaseURL, &c.Enrich.CoverArtBaseURL}
	written := make([]string, len(bases))
	for i, base := range bases {
		written[i], *base = *base, ""
	}
	_ = c.normalize(false) // its one refusal is a base URL's, and none is left
	for i, base := range bases {
		*base = written[i]
		if v, err := normalizeBaseURL("", written[i]); err == nil {
			*base = v
		}
	}
}

// loadsAtNextStart answers nil where data, the file a save would write as the
// config at path, loads as the next start loads it: Load's own steps in Load's
// order, decoded, its defaults given, the BRIDGE_* variables applied, its
// relative paths resolved against the directory Load takes from path, then
// normalized and validated, all without a word, so a refused candidate neither
// logs nor moves the player-off reason (NormalizeAndValidateQuiet); and, with
// its OpenHome surface on, at a port no other listener of serve binds
// (OpenHomePortClash), which a start turns the player off over rather than
// refuse, and the settings save refuses. Its error says why, naming no value.
func loadsAtNextStart(path string, data []byte) error {
	var c Config
	if err := DecodeFile(data, &c); err != nil {
		return ValueFreeYAMLError(err)
	}
	c.applyDefaultsAs(false)
	c.applyEnvOverridesAs(false)
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("abs config path: %w", err)
	}
	c.resolvePaths(filepath.Dir(abs))
	if err := c.NormalizeAndValidateQuiet(); err != nil {
		return err
	}
	if c.Output.Player.OpenHome {
		return c.OpenHomePortClash()
	}
	return nil
}

// settingsChanged is the yaml path of each setting a and b hold differently,
// sorted: every exported field yaml writes, reached through the structs that
// hold it, a setting of its own, compared by sameValue; a list one setting
// whatever it holds (upnpUpstream.servers among them), never walked by its
// entries; a struct that writes itself as one value (usbid.Serial, whose one
// field is unexported) one setting; and a player block that did not read, on
// either side, one setting, output.player, compared by the text it keeps,
// since its fields read nothing.
func settingsChanged(a, b *Config) []string {
	var out []string
	walkSettings(reflect.ValueOf(a).Elem(), reflect.ValueOf(b).Elem(), "", &out)
	slices.Sort(out)
	return out
}

// walkSettings adds to out the yaml path, under prefix, of each setting a and
// b, values of one struct type, hold differently (settingsChanged).
func walkSettings(a, b reflect.Value, prefix string, out *[]string) {
	for i := range a.NumField() {
		f := a.Type().Field(i)
		key := settingKey(f)
		if !f.IsExported() || key == "" {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		fa, fb := a.Field(i), b.Field(i)
		switch {
		case f.Type == playerConfigType && (unreadBlock(fa) || unreadBlock(fb)):
			if !sameKeptBlock(fa.Interface().(PlayerConfig), fb.Interface().(PlayerConfig)) {
				*out = append(*out, path)
			}
		case f.Type.Kind() == reflect.Struct && !oneValue(f.Type):
			walkSettings(fa, fb, path, out)
		case !sameValue(fa, fb):
			*out = append(*out, path)
		}
	}
}

// settingKey is the key yaml writes f under: its tag's, or its name in lower
// case where its tag names none, and "" for a field yaml leaves out.
func settingKey(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "-" {
		return ""
	}
	if key, _, _ := strings.Cut(tag, ","); key != "" {
		return key
	}
	return strings.ToLower(f.Name)
}

// oneValue reports whether t, a struct type, writes itself as one value, so
// that it is one setting, compared whole: a type with a MarshalYAML of its own
// (usbid.Serial), but the player block, whose fields are its settings.
func oneValue(t reflect.Type) bool {
	return t != playerConfigType && t.Implements(yamlMarshalerType)
}

// unreadBlock reports whether v, a PlayerConfig, is a block that did not read.
func unreadBlock(v reflect.Value) bool {
	return v.Interface().(PlayerConfig).unread != ""
}

// sameKeptBlock reports whether a and b, player blocks one of which did not
// read, are one setting: both did not read, and keep one text (the block as
// the file held it, which a save writes back), or keep none.
func sameKeptBlock(a, b PlayerConfig) bool {
	if (a.unread == "") != (b.unread == "") {
		return false
	}
	return bytes.Equal(keptText(a), keptText(b))
}

// keptText is the text a player block that did not read keeps, as a save
// writes it back, nil for none (a block a save drops, or one no save could
// write, whose save fails all the same).
func keptText(p PlayerConfig) []byte {
	if p.written == nil {
		return nil
	}
	text, err := yaml.Marshal(p.written)
	if err != nil {
		return nil
	}
	return text
}

// settingValue is the field of c the yaml path names, settable through c, and
// whether one does.
func settingValue(c *Config, path string) (reflect.Value, bool) {
	v := reflect.ValueOf(c).Elem()
	for _, key := range strings.Split(path, ".") {
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		field := -1
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() && settingKey(f) == key {
				field = i
				break
			}
		}
		if field < 0 {
			return reflect.Value{}, false
		}
		v = v.Field(field)
	}
	return v, true
}

// overlap reports whether settings a and b, yaml paths, are one setting, or
// one holds the other (a player block and its fields).
func overlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+".") || strings.HasPrefix(b, a+".")
}

// settingsUnder is the settings, of settings, at or below the yaml path root.
func settingsUnder(settings []string, root string) []string {
	var out []string
	for _, s := range settings {
		if overlap(s, root) {
			out = append(out, s)
		}
	}
	return out
}
