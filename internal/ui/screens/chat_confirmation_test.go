package screens

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestHumanDuration(t *testing.T) {
	tests := map[string]struct {
		d    time.Duration
		want string
	}{
		"zero":                  {d: 0, want: "0s"},
		"seconds only":          {d: 30 * time.Second, want: "30s"},
		"minutes only":          {d: 10 * time.Minute, want: "10m"},
		"hours only":            {d: 2 * time.Hour, want: "2h"},
		"hours and minutes":     {d: 90 * time.Minute, want: "1h30m"},
		"hours minutes seconds": {d: time.Hour + 2*time.Minute + 3*time.Second, want: "1h2m3s"},
		"drain-timeout floor":   {d: time.Second, want: "1s"},
		"poke-interval floor":   {d: 30 * time.Second, want: "30s"},
		"negative":              {d: -90 * time.Second, want: "-1m30s"},
		"sub-second remainder":  {d: 90*time.Second + 500*time.Millisecond, want: (90*time.Second + 500*time.Millisecond).String()},
		"pure sub-second falls back to Duration.String": {d: 500 * time.Millisecond, want: "500ms"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, humanDuration(tc.d))
		})
	}
}

func TestHumanWordList(t *testing.T) {
	tests := map[string]struct {
		words []string
		want  string
	}{
		"empty":         {words: nil, want: "(none)"},
		"single word":   {words: []string{"$nick"}, want: "$nick"},
		"several words": {words: []string{"alice", "bob", "$nick"}, want: "alice, bob, $nick"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, humanWordList(tc.words))
		})
	}
}

func TestShortErrorText(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	wrappedDialErr := fmt.Errorf("send message: %w", dialErr)

	tests := map[string]struct {
		err  error
		want string
	}{
		"context deadline exceeded": {
			err:  context.DeadlineExceeded,
			want: "timed out; try again",
		},
		"wrapped context deadline exceeded": {
			err:  fmt.Errorf("send message: %w", context.DeadlineExceeded),
			want: "timed out; try again",
		},
		"context cancelled": {
			err:  context.Canceled,
			want: "cancelled",
		},
		"a bare network error": {
			err:  dialErr,
			want: "could not reach the API; check your network connection and base URL",
		},
		"a network error wrapped in other detail": {
			err:  wrappedDialErr,
			want: "could not reach the API; check your network connection and base URL",
		},
		"a domain-typed error passes through unchanged": {
			err:  domain.PokeIntervalOutOfRangeError{Interval: 5 * time.Second, Floor: 30 * time.Second},
			want: "poke interval must be at least 30s: got 5s",
		},
		"an ordinary error passes through unchanged": {
			err:  errors.New("unknown command: /foo"),
			want: "unknown command: /foo",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, shortErrorText(tc.err))
		})
	}
}

func TestCommandErrorText_prefixes_the_operation(t *testing.T) {
	got := commandErrorText("join", errors.New("unknown command: /foo"))
	require.Equal(t, "join: unknown command: /foo", got)
}

func TestCommandErrorText_collapses_a_wrapped_network_chain(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("lookup openrouter.ai: no such host")}
	chain := fmt.Errorf("send: %w", fmt.Errorf("send message: %w", dialErr))

	got := commandErrorText("send", chain)

	require.Equal(t, "send: could not reach the API; check your network connection and base URL", got)
	require.NotContains(t, got, "openrouter.ai", "the raw chain must not leak into the transcript")
	require.NotContains(t, got, "dial tcp", "the raw chain must not leak into the transcript")
}
