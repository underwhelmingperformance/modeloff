package protocol_test

import (
	"reflect"
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
		window domain.Window
		want   protocol.MsgTarget
		ok     bool
	}{
		{"channel window", domain.WindowKey("#dev"), protocol.ChannelTarget("#dev"), true},
		{"status window", domain.WindowKey(domain.StatusChannelName), nil, false},
		{"dm window", domain.WindowKey("a1b2c3d4e5f60718"), protocol.ClientTarget("a1b2c3d4e5f60718"), true},
		{"dm window with the user", domain.WindowKey(""), protocol.ClientTarget(""), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := protocol.TargetForWindow(tc.window)
			type assertionSnapshot struct {
				Target protocol.MsgTarget
				OK     bool
			}

			require.Equal(t, assertionSnapshot{Target: tc.want, OK: tc.ok}, assertionSnapshot{Target: got, OK: ok})
		})
	}
}

func TestWindowTargetForKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  domain.ChannelName
		want protocol.WindowTarget
	}{
		{name: "status", key: domain.StatusChannelName},
		{name: "channel", key: "#dev", want: protocol.ChannelWindowTarget("#dev")},
		{name: "model direct message", key: "inst-botty", want: protocol.DirectWindowTarget("inst-botty")},
		{name: "user direct message", want: protocol.DirectWindowTarget("")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, protocol.WindowTargetForKey(tc.key))
		})
	}
}

func TestWindowTarget_variants(t *testing.T) {
	t.Parallel()

	type projection struct {
		Key         domain.ChannelName
		Kind        domain.ChannelKind
		Channel     domain.ChannelName
		ChannelOK   bool
		Peer        domain.InstanceID
		PeerOK      bool
		DynamicKind reflect.Kind
	}
	cases := []struct {
		name   string
		target protocol.WindowTarget
		want   projection
	}{
		{
			name: "channel", target: protocol.ChannelWindowTarget("#Dev"),
			want: projection{
				Key: "#Dev", Kind: domain.KindChannel,
				Channel: "#Dev", ChannelOK: true, DynamicKind: reflect.String,
			},
		},
		{
			name: "model direct message", target: protocol.DirectWindowTarget("inst-botty"),
			want: projection{
				Key: "inst-botty", Kind: domain.KindDM,
				Peer: "inst-botty", PeerOK: true, DynamicKind: reflect.String,
			},
		},
		{
			name: "user direct message", target: protocol.DirectWindowTarget(""),
			want: projection{
				Kind: domain.KindDM, PeerOK: true, DynamicKind: reflect.String,
			},
		},
		{
			name: "status window",
			want: projection{Kind: domain.KindStatus},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			channel, channelOK := protocol.ChannelWindowName(tc.target)
			peer, peerOK := protocol.DirectWindowPeer(tc.target)
			dynamicKind := reflect.Invalid
			if tc.target != nil {
				dynamicKind = reflect.TypeOf(tc.target).Kind()
			}
			got := projection{
				Key: protocol.WindowKey(tc.target), Kind: protocol.WindowTargetKind(tc.target),
				Channel: channel, ChannelOK: channelOK,
				Peer: peer, PeerOK: peerOK, DynamicKind: dynamicKind,
			}

			require.Equal(t, tc.want, got)
		})
	}
}

func TestEqualWindowTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a    protocol.WindowTarget
		b    protocol.WindowTarget
		want bool
	}{
		{name: "no targets", want: true},
		{name: "one target", a: protocol.ChannelWindowTarget("#dev")},
		{name: "folded channels", a: protocol.ChannelWindowTarget("#Dev"), b: protocol.ChannelWindowTarget("#dev"), want: true},
		{name: "different channels", a: protocol.ChannelWindowTarget("#dev"), b: protocol.ChannelWindowTarget("#other")},
		{name: "same peer", a: protocol.DirectWindowTarget("inst-botty"), b: protocol.DirectWindowTarget("inst-botty"), want: true},
		{name: "different peers", a: protocol.DirectWindowTarget("inst-botty"), b: protocol.DirectWindowTarget("inst-other")},
		{name: "different variants", a: protocol.ChannelWindowTarget("#dev"), b: protocol.DirectWindowTarget("#dev")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, protocol.EqualWindowTarget(tc.a, tc.b))
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
