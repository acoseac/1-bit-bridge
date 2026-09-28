package transcode

// RedactError returns err with the absolute paths this job can name taken
// out, as a failed job's own message is (redactSoxErr): the source by its
// library-relative path, and the variants directory, the render scratch and
// the tempDir as the privacy page allows. It is for an error this job's work
// produced that is logged anywhere but the pool's failure path, which
// redacts already: the album survey's measurement of an album-mate is the
// first. errors.Is and errors.As still see err. A nil err stays nil.
func (j JobSpec) RedactError(err error) error {
	if err == nil {
		return nil
	}
	return redactedError{msg: redactSoxErr(err.Error(), j), err: err}
}

// redactedError is the error RedactError returns: the redacted text, with
// the original behind it for errors.Is.
type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }

func (e redactedError) Unwrap() error { return e.err }
