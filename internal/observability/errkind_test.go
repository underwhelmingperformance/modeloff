package observability_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/observability"
)

// TestErrWithKind covers the pair a component installs as its span
// classifier: what [observability.ErrWithKind] attaches and what
// [observability.ErrorKindOf] reads back out, including through the
// `%w` wrapping every call site adds around it.
func TestErrWithKind(t *testing.T) {
	t.Parallel()

	cause := errors.New("upstream refused")

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "an untagged error carries no kind",
			err:  cause,
			want: "",
		},
		{
			name: "a tagged error carries the kind it was given",
			err:  observability.ErrWithKind(cause, observability.ErrorKindDispatch),
			want: observability.ErrorKindDispatch,
		},
		{
			name: "a tag survives being wrapped again",
			err:  fmt.Errorf("send events: %w", observability.ErrWithKind(cause, observability.ErrorKindValidation)),
			want: observability.ErrorKindValidation,
		},
		{
			name: "the outermost tag wins",
			err: observability.ErrWithKind(
				observability.ErrWithKind(cause, observability.ErrorKindStore),
				observability.ErrorKindTransport,
			),
			want: observability.ErrorKindTransport,
		},
		{
			name: "a nil error carries no kind",
			err:  nil,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, observability.ErrorKindOf(tc.err))
		})
	}
}

// TestErrWithKind_preserves_the_chain pins the two properties every
// call site depends on: a nil error stays nil so the helper can wrap
// the tail of a return, and a tagged error still matches its cause
// under `errors.Is`.
func TestErrWithKind_preserves_the_chain(t *testing.T) {
	t.Parallel()

	require.NoError(t, observability.ErrWithKind(nil, observability.ErrorKindStore))

	cause := errors.New("no such channel")
	tagged := observability.ErrWithKind(cause, observability.ErrorKindNotFound)

	require.ErrorIs(t, tagged, cause)
	require.Equal(t, cause.Error(), tagged.Error())
}
