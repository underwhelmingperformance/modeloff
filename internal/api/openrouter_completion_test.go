package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
)

type completionAllowanceCase struct {
	Name      string
	Allowance int
}

// TestCompletionAllowance_is_bounded_by_what_the_prompt_leaves pins the
// one rule the planner's reserve and the request's cap both follow.
//
// The planner reserves the ceiling before it knows what the prompt will
// cost. A request whose prompt turned out larger has to ask for less, or
// a small window is handed a cap that never shrank with the reserve, and
// a tool exchange that fitted when it was planned stops fitting once the
// assistant message and the tool results are appended.
func TestCompletionAllowance_is_bounded_by_what_the_prompt_leaves(t *testing.T) {
	cases := []struct {
		name          string
		contextLength int
		promptTokens  int
		want          completionAllowanceCase
	}{
		{
			name:          "a wide window gives the ceiling",
			contextLength: 128000, promptTokens: 4000,
			want: completionAllowanceCase{
				Name:      "a wide window gives the ceiling",
				Allowance: api.DispatchCompletionTokens,
			},
		},
		{
			name:          "a small window gives what is left",
			contextLength: 4096, promptTokens: 1000,
			want: completionAllowanceCase{
				Name: "a small window gives what is left", Allowance: 3096,
			},
		},
		{
			name:          "a grown tool exchange asks for less",
			contextLength: 8000, promptTokens: 5000,
			want: completionAllowanceCase{
				Name: "a grown tool exchange asks for less", Allowance: 3000,
			},
		},
		{
			name:          "a prompt past the window asks for one token",
			contextLength: 8000, promptTokens: 9000,
			want: completionAllowanceCase{
				Name: "a prompt past the window asks for one token", Allowance: 1,
			},
		},
		{
			name:          "an unknown window gives the ceiling",
			contextLength: 0, promptTokens: 9000,
			want: completionAllowanceCase{
				Name:      "an unknown window gives the ceiling",
				Allowance: api.DispatchCompletionTokens,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, completionAllowanceCase{
				Name: testCase.name,
				Allowance: api.CompletionAllowance(
					testCase.contextLength, testCase.promptTokens,
				),
			})
		})
	}
}
