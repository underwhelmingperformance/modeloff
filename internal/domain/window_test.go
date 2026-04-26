package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatusWindow_accessors(t *testing.T) {
	created := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	w := NewStatusWindow(created)

	require.Equal(t, StatusChannelName, w.Name())
	require.Equal(t, created, w.Created())
	require.Equal(t, KindStatus, w.Kind())
	require.Equal(t, string(StatusChannelName), w.DisplayName())
}

func TestChannelWindow_accessors(t *testing.T) {
	created := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	w := NewChannelWindow("general", created)

	require.Equal(t, ChannelName("#general"), w.Name())
	require.Equal(t, created, w.Created())
	require.Equal(t, KindChannel, w.Kind())
	require.Equal(t, "#general", w.DisplayName())
	require.Equal(t, 0, w.Members.Len())
}

func TestChannelWindow_normalises_bare_name(t *testing.T) {
	w := NewChannelWindow("general", time.Time{})

	require.Equal(t, ChannelName("#general"), w.Name())
}

func TestChannelWindow_keeps_prefixed_name(t *testing.T) {
	w := NewChannelWindow("#general", time.Time{})

	require.Equal(t, ChannelName("#general"), w.Name())
}

func TestDMWindow_accessors(t *testing.T) {
	created := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	bot := NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)

	w := NewDMWindow(bot, created)

	require.Equal(t, ChannelName("botty"), w.Name())
	require.Equal(t, created, w.Created())
	require.Equal(t, KindDM, w.Kind())
	require.Equal(t, "@botty", w.DisplayName())
	require.Same(t, bot, w.Counterpart)
}

func TestWindow_interface_satisfaction(t *testing.T) {
	bot := NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)

	windows := []Window{
		NewStatusWindow(time.Time{}),
		NewChannelWindow("general", time.Time{}),
		NewDMWindow(bot, time.Time{}),
	}

	kinds := make([]ChannelKind, 0, len(windows))
	for _, w := range windows {
		kinds = append(kinds, w.Kind())
	}

	require.Equal(t, []ChannelKind{KindStatus, KindChannel, KindDM}, kinds)
}

func TestWindowFromChannel_round_trip(t *testing.T) {
	created := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	bot := NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)

	resolver := func(nick Nick) *Instance {
		if nick == "botty" {
			return bot
		}

		return nil
	}

	channelMembers := NewMemberList()

	cases := []struct {
		name        string
		in          Channel
		wantName    ChannelName
		wantKind    ChannelKind
		wantCreated time.Time
	}{
		{name: "status", in: Channel{Name: StatusChannelName, Kind: KindStatus, Created: created}, wantName: StatusChannelName, wantKind: KindStatus, wantCreated: created},
		{
			name: "channel",
			in: Channel{
				Name:       "#general",
				Kind:       KindChannel,
				Created:    created,
				Topic:      "welcome",
				TopicSetBy: "alice",
				TopicSetAt: created,
				Members:    channelMembers,
			},
			wantName:    "#general",
			wantKind:    KindChannel,
			wantCreated: created,
		},
		{name: "dm", in: Channel{Name: "botty", Kind: KindDM, Created: created}, wantName: "botty", wantKind: KindDM, wantCreated: created},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WindowFromChannel(tc.in, resolver)
			require.NoError(t, err)
			require.Equal(t, tc.wantName, got.Name())
			require.Equal(t, tc.wantKind, got.Kind())
			require.Equal(t, tc.wantCreated, got.Created())

			projected := ChannelFromWindow(got)

			expected := tc.in
			if tc.in.Kind == KindStatus || tc.in.Kind == KindDM {
				// Status/DM project back without a member list — these
				// kinds carry no `Members` on the `Channel` shape, so
				// the round-trip leaves it zero-valued.
				expected.Members = MemberList{}
			}

			require.Equal(t, expected, projected)
		})
	}
}

func TestWindowFromChannel_missing_dm_counterpart(t *testing.T) {
	resolver := func(Nick) *Instance { return nil }

	_, err := WindowFromChannel(
		Channel{Name: "ghost", Kind: KindDM},
		resolver,
	)

	var missing MissingDMCounterpartError
	require.ErrorAs(t, err, &missing)
	require.Equal(t, Nick("ghost"), missing.Nick)
}

func TestWindowFromChannel_unknown_kind(t *testing.T) {
	_, err := WindowFromChannel(Channel{Name: "weird", Kind: ChannelKind(99)}, nil)

	var unknown UnknownChannelKindError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, ChannelKind(99), unknown.Kind)
}
