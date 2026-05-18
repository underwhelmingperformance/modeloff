package modelclient

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

func TestClassifyEnsureModelError(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"ErrModelListUnavailable": {
			err:  ErrModelListUnavailable,
			want: observability.ErrorKindClientState,
		},
		"wrapped ErrModelListUnavailable": {
			err:  fmt.Errorf("wrap: %w", ErrModelListUnavailable),
			want: observability.ErrorKindClientState,
		},
		"ErrNoAPIKey": {
			err:  ErrNoAPIKey,
			want: observability.ErrorKindClientState,
		},
		"UnsupportedModelError": {
			err:  domain.UnsupportedModelError{ModelID: "foo"},
			want: observability.ErrorKindValidation,
		},
		"wrapped UnsupportedModelError": {
			err:  fmt.Errorf("wrap: %w", domain.UnsupportedModelError{ModelID: "foo"}),
			want: observability.ErrorKindValidation,
		},
		"upstream wrapped as list models": {
			err:  fmt.Errorf("list models: %w", fmt.Errorf("transport")),
			want: observability.ErrorKindDispatch,
		},
		"unrelated error": {
			err:  fmt.Errorf("anything else"),
			want: observability.ErrorKindDispatch,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyEnsureModelError(tc.err))
		})
	}
}
