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

func TestChannel_DisplayName(t *testing.T) {
	tests := []struct {
		name    string
		channel Channel
		want    string
	}{
		{
			name:    "channel keeps the hash prefix",
			channel: Channel{Name: "#general", Kind: KindChannel},
			want:    "#general",
		},
		{
			name:    "dm prefixes the nick with @",
			channel: Channel{Name: "botty", Kind: KindDM},
			want:    "@botty",
		},
		{
			name:    "status channel renders the reserved name",
			channel: Channel{Name: StatusChannelName, Kind: KindStatus},
			want:    string(StatusChannelName),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.channel.DisplayName())
		})
	}
}
