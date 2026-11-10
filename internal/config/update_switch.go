package config

import (
	"errors"

	"gopkg.in/yaml.v3"
)

// errNoSettingsMapping is WithUpdateChecksOff's refusal of a file whose
// document is no mapping of settings, or whose update block is one it cannot
// write the key into (an alias, a list, a word).
var errNoSettingsMapping = errors.New("the settings are not a mapping update.checkEnabled can be written into")

// WithUpdateChecksOff is raw, the bytes of a settings file, with
// update.checkEnabled written false, as the console's save of an off writes
// it, and everything else as raw holds it, the update block and its other keys
// included: the settings a backup's settings check is asked about before the
// console saves an off during an install's probation (updater.ChecksOffRefused,
// backlog B761's round 3), so that the only difference between what the check
// is asked of and the file as it stands is the key. The bytes are yaml.v3's
// encoding of the document (its comments kept where yaml.v3 keeps them), for
// a loader to read, never written over the settings file. A file that is not
// YAML, whose document is no mapping, or whose update block is no mapping, is
// an error; an empty file is the update block alone.
func WithUpdateChecksOff(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, ValueFreeYAMLError(err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errNoSettingsMapping
	}
	top := doc.Content[0]
	update := mappingValue(top, "update")
	switch {
	case update == nil:
		update = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		top.Content = append(top.Content, scalarNode("update", "!!str"), update)
	case update.Kind == yaml.ScalarNode && update.Tag == "!!null":
		// `update:` with nothing after it: a block with no keys.
		*update = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	case update.Kind != yaml.MappingNode:
		return nil, errNoSettingsMapping
	}
	if v := mappingValue(update, "checkEnabled"); v != nil {
		*v = *scalarNode("false", "!!bool")
	} else {
		update.Content = append(update.Content, scalarNode("checkEnabled", "!!str"), scalarNode("false", "!!bool"))
	}
	return yaml.Marshal(&doc)
}

// WritesUpdateCheckKey reports whether raw, a settings file's bytes, writes
// update.checkEnabled at all, whatever its value: true, false, or none (a key
// written `checkEnabled:` or `checkEnabled: null` decodes as the key left out,
// and is written all the same), as a release from before the switch, whose
// strict decoder refuses the key on its line before it reads any variable,
// reads the file (backlog B761's round 4). It reads the document's own keys,
// its aliases and merge keys resolved as yaml.v3 resolves them, never the
// switch the settings decode to, nor the text (a key a comment names is no
// key). Settings that do not parse, and an update block that is no mapping,
// are an error, which a caller that cannot rule the key out takes for one
// written; settings that say nothing write none.
func WritesUpdateCheckKey(raw []byte) (bool, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return false, ValueFreeYAMLError(err)
	}
	update, there := doc["update"]
	if !there || update == nil {
		return false, nil
	}
	block, ok := update.(map[string]any)
	if !ok {
		return false, errNoSettingsMapping
	}
	_, written := block["checkEnabled"]
	return written, nil
}

// mappingValue is the value of the last key named name in the mapping m, as a
// decoder reads a key written twice, nil where m has none.
func mappingValue(m *yaml.Node, name string) *yaml.Node {
	var found *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == name {
			found = m.Content[i+1]
		}
	}
	return found
}

func scalarNode(value, tag string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

// KeepFiledSwitches sets, in c, a config a writer outside the live holder
// loaded in its turn (FileLock.LoadFound: the file with the BRIDGE_*
// variables of this process's environment over it) and changed, the three
// opt-outs (enrich.enabled, enrich.deezerEnabled, update.checkEnabled) as the
// file holds them, file being the bytes its turn found (FileLock.Found), where
// the writer left them as before, a clone of c taken as it loaded, holds
// them; a switch the writer changed is written as the holder writes a change,
// off as false and on as no key (filedSwitches). No writer outside the holder
// changes one today: `bridge library add` and `remove` (Edit) and `bridge
// output dac bind`, each run from a shell whose environment may set a
// variable the service's does not, wrote the variable's value into the file,
// update.checkEnabled: true among them, which a release from before the
// switch refuses as it refuses false, so that a crash rollback to it would
// not put it back (backlog B761's round 3). Only these three fields of c are
// set.
func (c *Config) KeepFiledSwitches(before *Config, file []byte) {
	filedSwitches(c, before, file)
}
