package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

type providerTranscript struct {
	History []protocol.IRCMessage
	Events  []protocol.IRCMessage
}

func TestProviderTargetProjection_binds_only_peer_message_commands(t *testing.T) {
	direct := providerTargetProjection{
		direct: true, peerID: "inst-alice", peerNick: "Alice",
	}

	tests := []struct {
		name       string
		projection providerTargetProjection
		command    protocol.Command
		want       protocol.Command
	}{
		{
			name:       "DM message to projected peer",
			projection: direct,
			command:    protocol.PrivMsg{Target: protocol.NickTarget("Alice"), Body: "hello"},
			want:       protocol.PrivMsg{Target: protocol.ClientTarget("inst-alice"), Body: "hello"},
		},
		{
			name:       "DM action under server casemapping",
			projection: direct,
			command:    protocol.Action{Target: protocol.NickTarget("alice"), Body: "waves"},
			want:       protocol.Action{Target: protocol.ClientTarget("inst-alice"), Body: "waves"},
		},
		{
			name:       "other nick",
			projection: direct,
			command:    protocol.PrivMsg{Target: protocol.NickTarget("bob"), Body: "hello"},
			want:       protocol.PrivMsg{Target: protocol.NickTarget("bob"), Body: "hello"},
		},
		{
			name:       "channel target",
			projection: direct,
			command:    protocol.Action{Target: protocol.ChannelTarget("#dev"), Body: "waves"},
			want:       protocol.Action{Target: protocol.ChannelTarget("#dev"), Body: "waves"},
		},
		{
			name:       "unrelated command",
			projection: direct,
			command:    protocol.Nick{New: "Alice"},
			want:       protocol.Nick{New: "Alice"},
		},
		{
			name:       "channel turn",
			projection: providerTargetProjection{},
			command:    protocol.PrivMsg{Target: protocol.NickTarget("Alice"), Body: "hello"},
			want:       protocol.PrivMsg{Target: protocol.NickTarget("Alice"), Body: "hello"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.projection.bindCommand(tt.command))
		})
	}
}

func TestModelClient_current_event_budget_keeps_the_dispatch_trigger(t *testing.T) {
	at := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	self := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	trigger := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: strings.Repeat("trigger ", 40), At: at,
	}
	senderHistory := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		Source: domain.ClientSource(self.ID(), self.Nick()),
		Target: "#dev", Body: "noted", At: at.Add(time.Second),
	}

	var provider providerTranscript
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return attr
		},
	})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	upstream := &apitest.Fake{SendEventsFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		_ api.SystemPrompt,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (api.CompletionResult, error) {
		provider = providerTranscript{
			History: slices.Clone(history),
			Events:  slices.Clone(events),
		}

		return api.CompletionResult{}, nil
	}}
	mc := New(Config{
		Instance: self, Session: newFakeSession(),
		APIClient: func() api.Client { return upstream },
		Tools:     NewToolRegistry(),
	})

	window := testChannelContext(domain.NewChannelWindow("#dev", at))
	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
		events:   []protocol.IRCMessage{trigger, senderHistory},
		triggers: []protocol.IRCMessage{trigger},
	})
	require.NoError(t, err)

	var records []map[string]any
	decoder := json.NewDecoder(&logs)
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		records = append(records, record)
	}

	type assertionSnapshot struct {
		Provider providerTranscript
		Logs     []map[string]any
	}

	require.Equal(t, assertionSnapshot{
		Provider: providerTranscript{
			History: nil,
			Events:  []protocol.IRCMessage{trigger, senderHistory},
		},
		Logs: []map[string]any{{
			"component":       "modelclient",
			"channel":         "#dev",
			"nick":            "botty",
			"model_id":        "test/model",
			"trigger_count":   float64(1),
			"trigger_summary": "PRIVMSG from alice",
			"tool_turns":      float64(0),
			"pass_reason":     "model_pass",
			"msg":             "dispatch to instance",
			"level":           "INFO",
		}},
	}, assertionSnapshot{
		Provider: provider,
		Logs:     records,
	})
}

func TestModelClient_refreshes_the_context_budget_before_the_first_request(t *testing.T) {
	at := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	self := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	older := domain.StoredEvent{ID: 1, Event: domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: "older " + strings.Repeat("x", 3000), At: at,
	}}
	newer := domain.StoredEvent{ID: 2, Event: domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: "newer " + strings.Repeat("x", 3000), At: at.Add(time.Second),
	}}
	trigger := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: "what do you think?", At: at.Add(2 * time.Second),
	}

	contextLen := 0
	var provider providerTranscript
	upstream := &apitest.Fake{SendEventsFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		_ api.SystemPrompt,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (api.CompletionResult, error) {
		provider = providerTranscript{
			History: slices.Clone(history),
			Events:  slices.Clone(events),
		}

		return api.CompletionResult{}, nil
	}, SummarizeContextFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		_ []string,
		_ []protocol.IRCMessage,
	) (api.ContextSummaryResult, error) {
		return api.ContextSummaryResult{Summary: "Earlier context was compacted."}, nil
	}}
	contexts := &recordingContextStore{}
	sess := newFakeSession()
	mc := New(Config{
		Instance: self, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     NewToolRegistry(),
		EnsureModel: func(context.Context, domain.ModelID) error {
			contextLen = 6500

			return nil
		},
		ContextLen: func(domain.ModelID) int { return contextLen },
		Contexts:   contexts,
	})
	olderMessage, ok := protocol.FromChannelEvent(older.Event)
	require.True(t, ok)
	newerMessage, ok := protocol.FromChannelEvent(newer.Event)
	require.True(t, ok)

	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api:      upstream,
		window:   testChannelContext(domain.NewChannelWindow("#dev", at)),
		target:   protocol.ChannelTarget("#dev"),
		history:  []domain.StoredEvent{older, newer},
		events:   []protocol.IRCMessage{trigger},
		triggers: []protocol.IRCMessage{trigger},
	})
	require.NoError(t, err)

	type assertionSnapshot struct {
		Provider providerTranscript
		Updates  []store.ContextSummaryUpdate
	}

	require.Equal(t, assertionSnapshot{
		Provider: providerTranscript{
			History: []protocol.IRCMessage{{
				Kind: protocol.KindServerReply, Source: domain.ServerSource("modeloff"),
				Target: "#dev", Body: "summary of earlier context: Earlier context was compacted.",
				At: sess.Now(),
			}},
			Events: []protocol.IRCMessage{trigger},
		},
		Updates: []store.ContextSummaryUpdate{{
			InstanceID: self.ID(), Window: protocol.ChannelWindowTarget("#dev"),
			Summary: "Earlier context was compacted.",
			Sources: []protocol.IRCMessage{olderMessage, newerMessage}, CreatedAt: sess.Now(),
		}},
	}, assertionSnapshot{Provider: provider, Updates: contexts.updates})
}

func TestModelClient_dispatch_log_counts_only_real_triggers(t *testing.T) {
	at := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	self := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	trigger := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: "question", At: at,
	}
	senderHistory := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource(self.ID(), self.Nick()),
		Target: "#dev", Body: "earlier response", At: at.Add(time.Second),
	}

	var provider providerTranscript
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return attr
		},
	})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	upstream := &apitest.Fake{SendEventsFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		_ api.SystemPrompt,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (api.CompletionResult, error) {
		provider = providerTranscript{
			History: slices.Clone(history),
			Events:  slices.Clone(events),
		}

		return api.CompletionResult{}, nil
	}}
	sess := newFakeSession()
	mc := New(Config{
		Instance: self, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     NewToolRegistry(),
	})
	sess.sub.nick = self.Nick()
	mc.sub = sess.sub

	err := mc.dispatchTurn(t.Context(), &turnBatch{
		channel:  "#dev",
		events:   []protocol.IRCMessage{trigger, senderHistory},
		triggers: []protocol.IRCMessage{trigger},
	})
	require.NoError(t, err)

	var records []map[string]any
	decoder := json.NewDecoder(&logs)
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		records = append(records, record)
	}

	type assertionSnapshot struct {
		Provider providerTranscript
		Logs     []map[string]any
	}

	require.Equal(t, assertionSnapshot{
		Provider: providerTranscript{
			History: nil,
			Events:  []protocol.IRCMessage{trigger, senderHistory},
		},
		Logs: []map[string]any{{
			"component":       "modelclient",
			"channel":         "#dev",
			"nick":            "botty",
			"model_id":        "test/model",
			"trigger_count":   float64(1),
			"trigger_summary": "PRIVMSG from alice",
			"tool_turns":      float64(0),
			"pass_reason":     "model_pass",
			"msg":             "dispatch to instance",
			"level":           "INFO",
		}},
	}, assertionSnapshot{
		Provider: provider,
		Logs:     records,
	})
}

func TestModelClient_provider_transcript_uses_IRC_nicks_for_DM_targets(t *testing.T) {
	// A window-scoped notice carries the window key as its target,
	// which for a DM is the counterpart's InstanceID. The user's is
	// the empty sentinel, so that notice reaches the model with no
	// target: an empty target is what a WHOIS or LIST answer carries,
	// and the projection leaves it alone so those answers are not
	// addressed to the counterpart.
	tests := []struct {
		name             string
		peerID           domain.InstanceID
		oldPeerNick      domain.Nick
		peerNick         domain.Nick
		wantNoticeTarget domain.Nick
		peer             func(domain.Nick) *domain.Instance
	}{
		{
			name:             "model counterpart",
			peerID:           "inst-alice",
			oldPeerNick:      "alice",
			peerNick:         "ally",
			wantNoticeTarget: "ally",
			peer: func(nick domain.Nick) *domain.Instance {
				return domain.NewModelInstance("inst-alice", nick, "test/model", "", nil)
			},
		},
		{
			name:        "user counterpart",
			oldPeerNick: "iain",
			peerNick:    "laney",
			peer:        domain.NewUserInstance,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
			self := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
			peer := tc.peer(tc.peerNick)
			sess := newFakeSession()
			sess.instances = map[domain.InstanceID]*domain.Instance{tc.peerID: peer}

			var provider providerTranscript
			upstream := &apitest.Fake{SendEventsFn: func(
				_ context.Context,
				_ domain.ModelID,
				_ domain.InstanceID,
				_ api.SystemPrompt,
				history []protocol.IRCMessage,
				events []protocol.IRCMessage,
			) (api.CompletionResult, error) {
				provider = providerTranscript{
					History: slices.Clone(history),
					Events:  slices.Clone(events),
				}

				return api.CompletionResult{}, nil
			}}
			mc := New(Config{
				Instance: self, Session: sess,
				APIClient: func() api.Client { return upstream },
				Tools:     NewToolRegistry(),
			})

			history := []domain.StoredEvent{
				{ID: 1, Event: domain.Message{
					Source: domain.ClientSource(tc.peerID, tc.oldPeerNick),
					Target: domain.ChannelName(self.ID()), Body: "before the rename", At: at,
				}},
				{ID: 2, Event: domain.Message{
					Source: domain.ClientSource(self.ID(), self.Nick()),
					Target: domain.ChannelName(tc.peerID), Body: "still here", Action: true, At: at.Add(time.Second),
				}},
			}
			replies := []storedReply{
				{
					window: protocol.DirectWindowTarget(tc.peerID),
					event: domain.StoredEvent{ID: 3, Event: domain.SystemNotice{
						Target: domain.ChannelName(tc.peerID), Text: "reply context", At: at.Add(2 * time.Second),
					}},
				},
				{
					event: domain.StoredEvent{ID: 4, Event: domain.SystemNotice{
						Text: "global reply", At: at.Add(3 * time.Second),
					}},
				},
			}
			trigger := protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.ClientSource(tc.peerID, tc.peerNick),
				Target: string(self.ID()), Body: "after the rename", At: at.Add(4 * time.Second),
			}
			triggers := []protocol.IRCMessage{trigger}
			window := testDirectContext(tc.peerID)
			toolTarget := protocol.ClientTarget(tc.peerID)

			err := mc.dispatchToInstance(t.Context(), turnRequest{
				api: upstream, window: window, target: toolTarget,
				history: history, replies: replies, events: triggers,
			})
			require.NoError(t, err)

			type assertionSnapshot struct {
				Provider    providerTranscript
				RawTriggers []protocol.IRCMessage
				Window      protocol.WindowTarget
				ToolTarget  protocol.MsgTarget
			}

			require.Equal(t, assertionSnapshot{
				Provider: providerTranscript{
					History: []protocol.IRCMessage{
						{
							Kind:   protocol.KindPrivMsg,
							Source: domain.ClientSource(tc.peerID, tc.oldPeerNick),
							Target: string(self.Nick()), Body: "before the rename", At: at,
						},
						{
							Kind:   protocol.KindAction,
							Source: domain.ClientSource(self.ID(), self.Nick()),
							Target: string(tc.peerNick), Body: "still here", At: at.Add(time.Second),
						},
						{
							Kind: protocol.KindServerReply, Source: domain.ServerSource("modeloff"),
							Target: string(tc.wantNoticeTarget), Body: "reply context", At: at.Add(2 * time.Second),
						},
						{
							Kind: protocol.KindServerReply, Source: domain.ServerSource("modeloff"),
							Body: "global reply", At: at.Add(3 * time.Second),
						},
					},
					Events: []protocol.IRCMessage{{
						Kind:   protocol.KindPrivMsg,
						Source: domain.ClientSource(tc.peerID, tc.peerNick),
						Target: string(self.Nick()), Body: "after the rename", At: at.Add(4 * time.Second),
					}},
				},
				RawTriggers: []protocol.IRCMessage{trigger},
				Window:      protocol.DirectWindowTarget(tc.peerID),
				ToolTarget:  protocol.ClientTarget(tc.peerID),
			}, assertionSnapshot{
				Provider:    provider,
				RawTriggers: triggers,
				Window:      window.Target(),
				ToolTarget:  toolTarget,
			})
		})
	}
}

type projectedMessageCase struct {
	name       string
	projection providerTargetProjection
	message    protocol.IRCMessage
	want       projectedMessageEffect
}

type projectedMessageEffect struct {
	Message protocol.IRCMessage
}

// TestProviderTargetProjection_message covers which recipient ids the
// DM transcript projection rewrites into nicks. The user's
// [domain.InstanceID] is the empty sentinel, which a WHOIS or LIST
// answer also carries as its target because the answer is about the
// server and not about a window. The message kind is what separates
// the two.
func TestProviderTargetProjection_message(t *testing.T) {
	at := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	withUser := providerTargetProjection{
		direct: true,
		selfID: "inst-botty", selfNick: "botty",
		peerID: "", peerNick: "laney",
	}
	serverReply := func(target domain.ChannelName, body string) protocol.IRCMessage {
		return protocol.IRCMessage{
			Kind: protocol.KindServerReply, Source: domain.ServerSource("modeloff"),
			Target: string(target), Body: body, At: at,
		}
	}

	tests := []projectedMessageCase{
		{
			name:       "a LIST answer keeps its empty target",
			projection: withUser,
			message:    serverReply("", "#dev 2 :go stuff"),
			want:       projectedMessageEffect{Message: serverReply("", "#dev 2 :go stuff")},
		},
		{
			name:       "a WHOIS answer keeps its empty target",
			projection: withUser,
			message:    serverReply("", "laney is on #dev"),
			want:       projectedMessageEffect{Message: serverReply("", "laney is on #dev")},
		},
		{
			name: "a notice naming a model counterpart's window resolves to its nick",
			projection: providerTargetProjection{
				direct: true,
				selfID: "inst-botty", selfNick: "botty",
				peerID: "inst-alice", peerNick: "alice",
			},
			message: serverReply("inst-alice", "cannot send to that client"),
			want:    projectedMessageEffect{Message: serverReply("alice", "cannot send to that client")},
		},
		{
			name:       "the model's own line to the user resolves to the user's nick",
			projection: withUser,
			message: protocol.IRCMessage{
				Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-botty", "botty"),
				Target: "", Body: "hello", At: at,
			},
			want: projectedMessageEffect{Message: protocol.IRCMessage{
				Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-botty", "botty"),
				Target: "laney", Body: "hello", At: at,
			}},
		},
		{
			name:       "the user's line to the model resolves to the model's nick",
			projection: withUser,
			message: protocol.IRCMessage{
				Kind: protocol.KindAction, Source: domain.LegacyClientSource("laney"),
				Target: "inst-botty", Body: "waves", At: at,
			},
			want: projectedMessageEffect{Message: protocol.IRCMessage{
				Kind: protocol.KindAction, Source: domain.LegacyClientSource("laney"),
				Target: "botty", Body: "waves", At: at,
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, projectedMessageEffect{
				Message: tc.projection.message(tc.message),
			})
		})
	}
}
