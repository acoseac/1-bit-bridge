package decode

import "io"

// offsetSeeker presents a view of rs starting at base, so Seek(0) is the
// container origin (e.g. after an ID3v2 tag).
type offsetSeeker struct {
	rs   io.ReadSeeker
	base int64
	pos  int64 // relative to base
}

func newOffsetSeeker(rs io.ReadSeeker, base int64) (*offsetSeeker, error) {
	if _, err := rs.Seek(base, io.SeekStart); err != nil {
		return nil, err
	}
	return &offsetSeeker{rs: rs, base: base}, nil
}

func (o *offsetSeeker) Read(p []byte) (int, error) {
	n, err := o.rs.Read(p)
	o.pos += int64(n)
	return n, err
}

func (o *offsetSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = o.base + offset
	case io.SeekCurrent:
		abs = o.base + o.pos + offset
	case io.SeekEnd:
		end, err := o.rs.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		abs = end + offset
	default:
		return 0, io.ErrUnexpectedEOF
	}
	if abs < o.base {
		return 0, io.ErrUnexpectedEOF
	}
	n, err := o.rs.Seek(abs, io.SeekStart)
	if err != nil {
		return 0, err
	}
	o.pos = n - o.base
	return o.pos, nil
}
