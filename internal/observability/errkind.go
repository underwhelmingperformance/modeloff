package observability

import "errors"

// kindError tags an error with one of the `ErrorKind` constants so a
// [SpanRunner] can attach `AttrErrorKind` without every call site
// carrying the kind through an auxiliary return value.
type kindError struct {
	kind string
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

// ErrWithKind annotates err with the given error kind. It returns nil
// when err is nil, so it can wrap the tail of a return.
func ErrWithKind(err error, kind string) error {
	if err == nil {
		return nil
	}

	return &kindError{kind: kind, err: err}
}

// ErrorKindOf returns the kind [ErrWithKind] attached to err, or the
// empty string when nothing in the chain carries one. It has the shape
// [SpanRunner.ClassifyError] takes, so a component whose call sites tag
// their errors installs this directly and the runner falls back to
// `DefaultErrKind` for everything else.
func ErrorKindOf(err error) string {
	if ke, ok := errors.AsType[*kindError](err); ok {
		return ke.kind
	}

	return ""
}
