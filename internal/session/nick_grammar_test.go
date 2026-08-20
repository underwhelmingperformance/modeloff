package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// TestSession_handleNick_refuses_an_erroneous_nick covers RFC 2812
// §2.3.1 at the dispatcher, which is where both `/nick` and the
// model-callable `nick` tool arrive. A nick outside the grammar is
// ERR_ERRONEUSNICKNAME (432) and the client keeps the nick it had.
func TestSession_handleNick_refuses_an_erroneous_nick(t *testing.T) {
	tests := []struct {
		name   string
		nick   domain.Nick
		reason domain.NickRejection
	}{
		{name: "empty", nick: "", reason: domain.NickEmpty},
		{name: "leading digit", nick: "9bot", reason: domain.NickBadFirstCharacter},
		{name: "embedded space", nick: "bo tty", reason: domain.NickBadCharacter},
		{name: "embedded colon", nick: "bo:tty", reason: domain.NickBadCharacter},
		{name: "over the length limit", nick: domain.Nick(strings.Repeat("b", domain.NickMaxLen+1)), reason: domain.NickTooLong},
		{name: "the anonymous mask", nick: domain.AnonymousNick, reason: domain.NickReserved},
		{name: "the anonymous mask in another case", nick: "Anonymous", reason: domain.NickReserved},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, _ := newTestSession(t)
			ctx := t.Context()

			before := userNick(t, sess)

			resp, err := userClient(t, sess).Send(ctx, protocol.Nick{New: tt.nick})
			require.NoError(t, err)

			var erroneous domain.ErroneousNicknameError
			require.ErrorAs(t, resp.Err, &erroneous)
			require.Equal(t,
				domain.ErroneousNicknameError{Nick: tt.nick, Reason: tt.reason, At: fixedTime},
				erroneous)

			require.Equal(t, before, userNick(t, sess), "a refused NICK leaves the nick alone")
		})
	}
}

// TestSession_handleNick_accepts_the_grammar pins the other side:
// every shape RFC 2812 §2.3.1 admits is taken.
func TestSession_handleNick_accepts_the_grammar(t *testing.T) {
	tests := []struct {
		name string
		nick domain.Nick
	}{
		{name: "letters", nick: "botty"},
		{name: "digits after the first character", nick: "b0t9"},
		{name: "hyphen after the first character", nick: "bo-t"},
		{name: "leading special", nick: "[bot]"},
		{name: "at the length limit", nick: domain.Nick(strings.Repeat("b", domain.NickMaxLen))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, _ := newTestSession(t)
			ctx := t.Context()

			resp, err := userClient(t, sess).Send(ctx, protocol.Nick{New: tt.nick})
			require.NoError(t, err)
			require.NoError(t, resp.Err)
			require.Equal(t, tt.nick, userNick(t, sess))
		})
	}
}

// TestSession_add_model_refuses_an_erroneous_generated_nick covers
// the other place a nick is claimed. Nick generation asks a model
// for a word, so the answer is only as well-formed as the model
// made it; the registration refuses one the grammar does not admit,
// and leaves no instance behind.
func TestSession_add_model_refuses_an_erroneous_generated_nick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	factory, ok := sess.modelClientFactory.(*testModelClientFactory)
	require.True(t, ok)
	factory.nick = "bot ty"

	err := addModelViaWire(ctx, t, sess, "#dev", "test/model", "")

	var erroneous domain.ErroneousNicknameError
	require.ErrorAs(t, err, &erroneous)
	require.Equal(t,
		domain.ErroneousNicknameError{Nick: "bot ty", Reason: domain.NickBadCharacter, At: fixedTime},
		erroneous)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Empty(t, instances)

	_, resolveErr := sess.ResolveNick(ctx, "bot ty")
	require.ErrorIs(t, resolveErr, storemod.ErrNoSuchNick)
}
