package protocol_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestParseMsgTarget pins how a raw target reads: the channel
// prefixes name a channel and everything else names a nick. A value
// that looks like an instance id is a nick like any other, so a
// mistyped nick reaches the server as a nick and is answered with
// 401 there.
func TestParseMsgTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want protocol.MsgTarget
	}{
		{"hash channel", "#dev", protocol.ChannelTarget("#dev")},
		{"local channel", "&modeloff", protocol.ChannelTarget("&modeloff")},
		{"nick", "botty", protocol.NickTarget("botty")},
		{"nick spelled like an instance id", "a1b2c3d4e5f60718", protocol.NickTarget("a1b2c3d4e5f60718")},
		{"empty", "", protocol.NickTarget("")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, protocol.ParseMsgTarget(tc.raw))
		})
	}
}

// TestTargetForWindow pins the other construction: a client that
// already holds a window addresses it by what it keeps that window
// under. A DM window is keyed by its counterpart's id, so it
// addresses that client by identity and survives the counterpart
// renaming itself.
func TestTargetForWindow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		window domain.ChannelName
		want   protocol.MsgTarget
	}{
		{"channel window", "#dev", protocol.ChannelTarget("#dev")},
		{"status window", domain.StatusChannelName, protocol.ChannelTarget(domain.StatusChannelName)},
		{"dm window", "a1b2c3d4e5f60718", protocol.ClientTarget("a1b2c3d4e5f60718")},
		{"dm window with the user", "", protocol.ClientTarget("")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, protocol.TargetForWindow(tc.window))
		})
	}
}

func TestMsgTarget_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target protocol.MsgTarget
		want   string
	}{
		{"channel", protocol.ChannelTarget("#dev"), "#dev"},
		{"nick", protocol.NickTarget("botty"), "botty"},
		{"client", protocol.ClientTarget("a1b2c3d4e5f60718"), "a1b2c3d4e5f60718"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, tc.target.String())
		})
	}
}
