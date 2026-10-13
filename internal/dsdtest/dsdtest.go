// Package dsdtest builds small DSDIFF forms for tests. The bridge reads a
// DFF header to time a render, so a test that needs a compression the
// writers do not emit builds one here.
//
// It is a normal package, as internal/flactest is: two test packages need
// the same form, and a _test.go helper cannot be shared. Nothing in
// production imports it.
package dsdtest

import "encoding/binary"

// StereoDSD64Rate is 64 times 44.1 kHz, the rate every form here names.
const StereoDSD64Rate = 2_822_400

// Form is a minimal FRM8 form of type DSD. Its PROP chunk names a stereo
// stream at StereoDSD64Rate and a CMPR chunk whose four-byte compression
// is compression. A dsdSize below zero omits the DSD chunk. Otherwise the
// form ends with a DSD chunk header that declares dsdSize payload bytes
// and carries none of them. An odd PROP body is padded, and the FRM8 size
// covers that pad and the DSD header.
func Form(compression string, dsdSize int64) []byte {
	rate := make([]byte, 4)
	binary.BigEndian.PutUint32(rate, StereoDSD64Rate)
	propBody := append([]byte("SND "), chunk("FS  ", rate)...)
	propBody = append(propBody, chunk("CHNL", []byte{0, 2, 'S', 'L', 'F', 'T'})...)
	propBody = append(propBody, chunk("CMPR", append([]byte(compression), 0))...)
	prop := chunk("PROP", propBody)
	if len(propBody)%2 == 1 {
		prop = append(prop, 0)
	}
	var dsd []byte
	if dsdSize >= 0 {
		dsd = append([]byte("DSD "), binary.BigEndian.AppendUint64(nil, uint64(dsdSize))...)
	}
	body := append(append([]byte("DSD "), prop...), dsd...)
	return chunk("FRM8", body)
}

func chunk(id string, payload []byte) []byte {
	b := append([]byte(id), binary.BigEndian.AppendUint64(nil, uint64(len(payload)))...)
	return append(b, payload...)
}
