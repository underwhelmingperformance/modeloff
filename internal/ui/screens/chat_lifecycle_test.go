package screens_test

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store/storetest"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/screens/screenstest"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// TestChatScreen_PartEvent_model_part_keeps_user_in_channel pins the
// membership rule the sidebar has to respect: a PART names the actor
// that left, and only the user's own PART takes the window away. A
// model leaving a channel the user is still in drops the model from
// the nick list and narrates the departure — the channel stays in the
// sidebar and stays focused.
func TestChatScreen_PartEvent_model_part_keeps_user_in_channel(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")
	uitest.AddModel(t, h.user, "#general", "anthropic/claude-3-haiku", "")

	model, err := h.sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, h)
	waitForChannelAndModelSeedDrain(tm)

	screenstest.SendProtocolEvent(tm.TestModel, domain.Part{
		Target:     "#general",
		Nick:       model.Nick(),
		InstanceID: model.ID(),
		Instance:   model,
		At:         time.Now(),
	}, []domain.ChannelName{"#general"})

	view := tm.WaitForViewContains("fakenick has left #general")

	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)

	require.Equal(t, []string{"Channels", "&modeloff", "▸#general"},
		uitest.NonEmptyColumn(columns[0]),
		"a model's PART must not take the user's channel out of the sidebar")
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** fakenick has joined #general",
		"*** fakenick has left #general",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])),
		"the user stays in the channel and sees the model leave it")
}

// TestChatScreen_Init_restores_persisted_last_channel pins the
// restart contract from the flow document: the window that was open
// last time is opened again. Without it the freshest join wins, which
// is whichever autojoin happened to land last.
func TestChatScreen_Init_restores_persisted_last_channel(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	apiClient := &uitest.FakeAPI{}
	sess, mgr, user := uitest.NewTestSession(t, s, apiClient, nil, nil, "", "", t.Context)

	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	require.NoError(t, s.SetLastChannel(t.Context(), "#general"))

	chatScreen, err := screens.NewChatScreen(t.Context, sess, mgr, user, newFakeConfigStore(), s, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen), teatest.WithInitialTermSize(termWidth, termHeight))

	view := tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#general") &&
			strings.Contains(view, "*** Created channel #general")
	})

	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{"Channels", "&modeloff", "▸#general", "#random"},
		uitest.NonEmptyColumn(uitest.VisibleColumns(body)[0]),
		"the persisted last channel must win over the newest join")
}

// TestChatScreen_nick_from_status_window_confirms_and_updates_prompt
// exercises the end-to-end shape of the same rule the unit test in
// the `screens` package pins: `&modeloff` receives no NICK broadcast,
// so the confirmation and the prompt have to track the user's own
// rename directly, independent of which windows the broadcast
// reached.
func TestChatScreen_nick_from_status_window_confirms_and_updates_prompt(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")

	tm := newChatApp(t, h)
	waitForChannelSeedDrain(tm)

	tm.Send(chatcmd.ChannelFocusMsg{Channel: domain.StatusChannelName, At: time.Now()})
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸&modeloff")
	})

	tm.Submit("/nick newnick")

	// The confirmation line and the prompt refresh ride the same
	// batch but reach the renderer on separate ticks, so both are
	// part of the predicate and the frame that satisfied it is what
	// the assertion reads.
	view := tm.WaitForViewContains("testuser is now known as newnick", "newnick >")

	body, _ := uitest.SplitBodyAndStatus(view)
	content := uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])
	require.Equal(t, []string{
		"*** testuser is now known as newnick",
		"newnick >",
	}, normaliseContent(content),
		"the confirmation and the prompt must both follow a rename issued from &modeloff")
}
