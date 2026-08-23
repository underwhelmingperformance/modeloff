package modelclient

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

func TestExecuteTools_refuses_a_tool_in_the_wrong_window(t *testing.T) {
	channelKind := domain.KindChannel
	executed := false
	registry := NewToolRegistry(ToolSpec{
		Definition:   api.ToolDefinition{Name: "topic"},
		RequiredKind: &channelKind,
		Execute: func(_ context.Context, _ ToolContext, _ json.RawMessage) (ToolResultPayload, error) {
			executed = true
			return ToolResultPayload{OK: true}, nil
		},
	})

	outcome, err := executeTools(
		t.Context(),
		newFakeSession(),
		ToolContext{Target: protocol.ClientTarget("peer")},
		registry,
		[]api.PendingToolCall{{ID: "call-1", Name: "topic", Args: json.RawMessage(`{}`)}},
		nil,
	)

	require.NoError(t, err)
	require.False(t, executed)
	require.Equal(t, toolBatchOutcome{
		results: []api.ToolResult{{
			ToolCallID: "call-1",
			Content:    `{"ok":false,"error":"tool \"topic\" is not available in this window"}`,
		}},
	}, outcome)
}

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
