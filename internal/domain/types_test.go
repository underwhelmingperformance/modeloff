package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormaliseChannelName(t *testing.T) {
	tests := []struct {
		name string
		in   ChannelName
		want ChannelName
	}{
		{name: "bare name gets prefix", in: "foo", want: "#foo"},
		{name: "already prefixed unchanged", in: "#foo", want: "#foo"},
		{name: "double prefix unchanged", in: "##foo", want: "##foo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, NormaliseChannelName(tt.in))
		})
	}
}

func TestInferChannelKind(t *testing.T) {
	tests := []struct {
		name string
		in   ChannelName
		want ChannelKind
	}{
		{name: "status reserved name", in: StatusChannelName, want: KindStatus},
		{name: "hash-prefixed is a channel", in: "#general", want: KindChannel},
		{name: "bare name is a dm", in: "botty", want: KindDM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, InferChannelKind(tt.in))
		})
	}
}
