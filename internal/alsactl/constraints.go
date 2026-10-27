package alsactl

import (
	"bytes"
	"errors"
)

// Constraint identifies one channel semantically; enumeration ids never bind.
type Constraint struct {
	Interface, Device, Index uint32
	Name                     string
	Channel                  int
	Value                    []byte
}

// CheckConstraints compares only the approved semantic controls.
func (s Snapshot) CheckConstraints(want []Constraint) error {
	for _, w := range want {
		matched := 0
		for _, c := range s.Controls {
			if c.Interface != w.Interface || c.Device != w.Device || c.Name != w.Name || c.Index != w.Index {
				continue
			}
			matched++
			width := 0
			switch c.Type {
			case 1, 2, 6:
				width = 8
			case 3:
				width = 4
			case 4:
				width = 1
			}
			if c.ReadError != "" || c.Access&1 == 0 || width == 0 || w.Channel < 0 || w.Channel >= int(c.Count) || len(c.Values) != int(c.Count)*width || !bytes.Equal(c.Values[w.Channel*width:(w.Channel+1)*width], w.Value) {
				return errors.New("constrained control unreadable or changed")
			}
		}
		if matched != 1 {
			return errors.New("constrained control absent or ambiguous")
		}
	}
	return nil
}
