package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type teardownFailureStore struct {
	Store

	armed              atomic.Bool
	instanceID         domain.InstanceID
	getWindowErr       error
	saveWindowErr      error
	deleteInstanceErr  error
	markInstanceErr    error
	clearSessionErr    error
	deleteMu           sync.Mutex
	deleteInstanceErrs []error
	markedMu           sync.Mutex
	markInstanceErrs   []error
	marked             []domain.InstanceID
}

type blockingAttachFactory struct {
	ModelClientFactory

	attached chan domain.InstanceID
	release  chan struct{}
}

type blockingForgetFactory struct {
	ModelClientFactory

	started chan protocol.ClientID
	release chan struct{}
}

func (f *blockingAttachFactory) Attach(
	ctx context.Context,
	sess *Session,
	inst *domain.Instance,
	attachment *protocol.Attachment,
) (protocol.Client, error) {
	client, err := f.ModelClientFactory.Attach(ctx, sess, inst, attachment)
	if err != nil {
		return nil, err
	}

	f.attached <- inst.ID()
	<-f.release

	return client, nil
}

func (f *blockingForgetFactory) Detach(id protocol.ClientID) {
	f.started <- id
	<-f.release
	f.ModelClientFactory.Detach(id)
}

func (s *teardownFailureStore) GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error) {
	if s.armed.Load() && s.getWindowErr != nil {
		return nil, s.getWindowErr
	}

	return s.Store.GetWindow(ctx, name)
}

func (s *teardownFailureStore) MarkInstancePendingDeletion(ctx context.Context, id domain.InstanceID) error {
	s.markedMu.Lock()
	s.marked = append(s.marked, id)
	if len(s.markInstanceErrs) > 0 {
		err := s.markInstanceErrs[0]
		s.markInstanceErrs = s.markInstanceErrs[1:]
		s.markedMu.Unlock()
		if err != nil {
			return err
		}
	} else {
		s.markedMu.Unlock()
	}
	if s.markInstanceErr != nil {
		return s.markInstanceErr
	}

	return s.Store.MarkInstancePendingDeletion(ctx, id)
}

func (s *teardownFailureStore) markedIDs() []domain.InstanceID {
	s.markedMu.Lock()
	defer s.markedMu.Unlock()

	return append([]domain.InstanceID(nil), s.marked...)
}

func (s *teardownFailureStore) SaveWindow(ctx context.Context, window domain.Window) error {
	if s.armed.Load() && s.saveWindowErr != nil {
		return s.saveWindowErr
	}

	return s.Store.SaveWindow(ctx, window)
}

func (s *teardownFailureStore) ClearSessionActive(ctx context.Context) error {
	if s.armed.Load() && s.clearSessionErr != nil {
		return s.clearSessionErr
	}

	return s.Store.ClearSessionActive(ctx)
}

func (s *teardownFailureStore) CommitChannelJoin(
	ctx context.Context,
	join storemod.ChannelJoin,
) (storemod.CommittedChannelEvent, error) {
	if s.armed.Load() && s.saveWindowErr != nil {
		return storemod.CommittedChannelEvent{}, s.saveWindowErr
	}

	return s.Store.CommitChannelJoin(ctx, join)
}

func (s *teardownFailureStore) CommitChannelUpdate(
	ctx context.Context,
	update storemod.ChannelUpdate,
) (storemod.CommittedChannelEvent, error) {
	if s.armed.Load() && s.saveWindowErr != nil {
		return storemod.CommittedChannelEvent{}, s.saveWindowErr
	}

	return s.Store.CommitChannelUpdate(ctx, update)
}

func (s *teardownFailureStore) DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error {
	if err := s.instanceDeletionError(id); err != nil {
		return err
	}

	return s.Store.DeleteInstanceByID(ctx, id)
}

func (s *teardownFailureStore) CommitInstanceDeletion(
	ctx context.Context,
	deletion storemod.InstanceDeletion,
) (storemod.CommittedInstanceDeletion, error) {
	if err := s.instanceDeletionError(deletion.InstanceID); err != nil {
		return storemod.CommittedInstanceDeletion{}, err
	}

	return s.Store.CommitInstanceDeletion(ctx, deletion)
}

func (s *teardownFailureStore) instanceDeletionError(id domain.InstanceID) error {
	if s.armed.Load() && (s.instanceID == "" || id == s.instanceID) {
		s.deleteMu.Lock()
		if len(s.deleteInstanceErrs) > 0 {
			err := s.deleteInstanceErrs[0]
			s.deleteInstanceErrs = s.deleteInstanceErrs[1:]
			s.deleteMu.Unlock()
			if err != nil {
				return err
			}
		} else {
			s.deleteMu.Unlock()
		}

		if s.deleteInstanceErr != nil {
			return s.deleteInstanceErr
		}
	}

	return nil
}

func TestSession_failed_add_releases_the_client_when_instance_deletion_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var dispatches atomic.Int32
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				dispatches.Add(1)
				return api.CompletionResult{}, nil
			},
		}
		backing := storetest.NewMemoryStore(t)
		failing := &teardownFailureStore{
			Store:             backing,
			saveWindowErr:     errors.New("admission failed"),
			deleteInstanceErr: errors.New("delete failed"),
		}
		factory := newTestModelClientFactory(t, fake)
		sess := New(t.Context(), failing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")

		require.NoError(t, userJoin(t.Context(), t, sess, "#general"))
		failing.armed.Store(true)

		err := addModelViaWire(t.Context(), t, sess, "#general", "test/model", "quiet regular")
		require.ErrorIs(t, err, failing.saveWindowErr)

		window, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)

		failing.armed.Store(false)
		_, err = userSendMessage(t.Context(), t, sess, "#general", "after failed add")
		require.NoError(t, err)
		synctest.Wait()
		require.Equal(t, struct {
			dispatches       int32
			deletedInstances []protocol.ClientID
			attached         []protocol.ClientID
			active           []domain.InstanceID
			members          []domain.Nick
		}{
			attached: []protocol.ClientID{},
			active:   []domain.InstanceID{protocol.UserClientID},
			members:  []domain.Nick{"testuser"},
		}, struct {
			dispatches       int32
			deletedInstances []protocol.ClientID
			attached         []protocol.ClientID
			active           []domain.InstanceID
			members          []domain.Nick
		}{
			dispatches:       dispatches.Load(),
			deletedInstances: factory.deletedInstances(),
			attached:         factory.attached(),
			active:           instanceIDs(t, backing),
			members:          memberNicks(window),
		})
	})
}

func TestSession_Shutdown_waits_for_an_accepted_add_model(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	factory := newTestModelClientFactory(t, &apitest.Fake{})
	attached := make(chan struct{})
	release := make(chan struct{})
	factory.afterAttach = func() {
		close(attached)
		<-release
	}

	sess := New(t.Context(), backing, factory, nil)
	attachTestUserClient(t, sess, "testuser")
	require.NoError(t, userJoin(t.Context(), t, sess, "#general"))

	addDone := make(chan error, 1)
	go func() {
		addDone <- addModelViaWire(t.Context(), t, sess, "#general", "test/model", "quiet regular")
	}()
	<-attached

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- sess.Shutdown(t.Context())
	}()
	<-sess.shuttingDown

	select {
	case err := <-shutdownDone:
		require.Failf(t, "Shutdown returned before the accepted command", "error: %v", err)
	default:
	}

	close(release)
	require.ErrorIs(t, <-addDone, ErrSessionClosed)
	require.NoError(t, <-shutdownDone)

	_, err := backing.ResolveNick(t.Context(), "fakenick")
	require.Error(t, err)
}

func TestSession_failed_terminal_tool_lets_the_model_retry(t *testing.T) {
	for _, toolName := range []string{"pass", "quit"} {
		t.Run(toolName, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var rejected []api.ToolResult
				fake := &apitest.Fake{
					SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
						return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
							{ID: "terminal", Name: toolName, Args: json.RawMessage(`{}`)},
						}}, nil
					},
					ContinueWithToolResultsFn: continueOnceWith(&rejected, msgToolCalls(t, "#general", "clean reply")),
				}
				sess, eventStore := newTestSessionWithAPI(t, fake)
				seedInstance(t, sess, eventStore, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#general"),
				})
				seedChannelWithMembers(t, sess, eventStore, "#general", "testuser", "botty")

				dispatchUserMessage(t.Context(), t, sess, "#general", "hello")

				type rejectedResult struct {
					ToolCallID   string
					OK           bool
					Summary      string
					Data         any
					ErrorPresent bool
				}
				gotRejected := make([]rejectedResult, len(rejected))
				for index, result := range rejected {
					var payload modelclient.ToolResultPayload
					require.NoError(t, json.Unmarshal([]byte(result.Content), &payload))
					gotRejected[index] = rejectedResult{
						ToolCallID:   result.ToolCallID,
						OK:           payload.OK,
						Summary:      payload.Summary,
						Data:         payload.Data,
						ErrorPresent: payload.Error != "",
					}
				}
				require.Equal(t, []rejectedResult{{
					ToolCallID:   "terminal",
					ErrorPresent: true,
				}}, gotRejected)
				require.Equal(t, []domain.Message{
					{Target: "#general", Source: domain.ClientSource(domain.InstanceID(""), domain.Nick("testuser")), Body: "hello", At: fixedTime},
					{Target: "#general", Source: domain.ClientSource(testMemberID("botty"), domain.Nick("botty")), Body: "clean reply", At: fixedTime},
				}, channelMessages(t, eventStore, "#general"))
			})
		})
	}
}

func TestSession_committed_quit_stops_the_tool_batch_when_teardown_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var continuations int
		message := msgToolCalls(t, "#general", "must not be sent").PendingToolCalls[0]
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
					{ID: "quit", Name: "quit", Args: json.RawMessage(`{"message":null}`)},
					message,
				}}, nil
			},
			ContinueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
				continuations++

				return api.CompletionResult{}, nil
			},
		}

		backing := storetest.NewMemoryStore(t)
		sentinel := errors.New("save failed")
		failing := &teardownFailureStore{Store: backing, saveWindowErr: sentinel}
		sess := New(t.Context(), failing, newTestModelClientFactory(t, fake), nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		botty := seedInstance(t, sess, backing, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")

		failing.instanceID = botty.ID()
		failing.armed.Store(true)

		dispatchUserMessage(t.Context(), t, sess, "#general", "hello")
		window, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)
		require.Equal(t, testMembers(t, sess, backing, "testuser"), window.Members)

		require.Equal(t, struct {
			continuations int
			connected     bool
			eventTypes    []string
			messages      []domain.Message
		}{
			continuations: 0,
			connected:     false,
			eventTypes:    []string{"message", "quit"},
			messages: []domain.Message{
				{Target: "#general", Source: domain.ClientSource(domain.InstanceID(""), domain.Nick("testuser")), Body: "hello", At: fixedTime},
			},
		}, struct {
			continuations int
			connected     bool
			eventTypes    []string
			messages      []domain.Message
		}{
			continuations: continuations,
			connected:     sess.ClientConnected(protocol.ClientID(botty.ID())),
			eventTypes:    channelEventTypes(t, backing, "#general"),
			messages:      channelMessages(t, backing, "#general"),
		})
	})
}

func TestSession_failed_instance_deletion_does_not_commit_quit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var continuations int
		message := msgToolCalls(t, "#general", "must not be sent").PendingToolCalls[0]
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
					{ID: "quit", Name: "quit", Args: json.RawMessage(`{"message":null}`)},
					message,
				}}, nil
			},
			ContinueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
				continuations++

				return api.CompletionResult{}, nil
			},
		}

		backing := storetest.NewMemoryStore(t)
		failing := &teardownFailureStore{Store: backing, deleteInstanceErr: errors.New("delete failed")}
		sess := New(t.Context(), failing, newTestModelClientFactory(t, fake), nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		botty := seedInstance(t, sess, backing, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")

		failing.instanceID = botty.ID()
		failing.armed.Store(true)

		dispatchUserMessage(t.Context(), t, sess, "#general", "hello")
		window, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)

		require.Equal(t, struct {
			continuations int
			connected     bool
			members       domain.MemberList
			eventTypes    []string
			messages      []domain.Message
		}{
			continuations: 0,
			connected:     true,
			members:       testMembers(t, sess, backing, "testuser", "botty"),
			eventTypes:    []string{"message"},
			messages: []domain.Message{
				{Target: "#general", Source: domain.ClientSource(domain.InstanceID(""), domain.Nick("testuser")), Body: "hello", At: fixedTime},
			},
		}, struct {
			continuations int
			connected     bool
			members       domain.MemberList
			eventTypes    []string
			messages      []domain.Message
		}{
			continuations: continuations,
			connected:     sess.ClientConnected(protocol.ClientID(botty.ID())),
			members:       window.Members,
			eventTypes:    channelEventTypes(t, backing, "#general"),
			messages:      channelMessages(t, backing, "#general"),
		})
	})
}

func TestSession_failed_instance_deletion_still_commits_kill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		sentinel := errors.New("delete failed")
		failing := &teardownFailureStore{Store: backing, deleteInstanceErr: sentinel}
		factory := newTestModelClientFactory(t, &apitest.Fake{})
		sess := New(t.Context(), failing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		botty := seedInstance(t, sess, backing, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
		collectEmittedEvents(t, sess)

		failing.instanceID = botty.ID()
		failing.armed.Store(true)

		resp, err := userClient(t, sess).Send(t.Context(), protocol.Kill{Nick: "botty", Reason: "spam"})
		require.ErrorIs(t, err, sentinel)
		synctest.Wait()
		window, loadErr := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, loadErr)
		_, resolveErr := backing.ResolveNick(t.Context(), "botty")
		require.ErrorIs(t, resolveErr, storemod.ErrNoSuchNick)
		pending, pendingErr := backing.ListPendingInstanceDeletions(t.Context())
		require.NoError(t, pendingErr)

		require.Equal(t, struct {
			response   protocol.Response
			connected  bool
			attached   []protocol.ClientID
			forgotten  []protocol.ClientID
			members    []domain.Nick
			visibleIDs []domain.InstanceID
			pending    []domain.InstanceID
			events     []domain.Event
		}{
			response:   protocol.Response{},
			attached:   []protocol.ClientID{},
			members:    []domain.Nick{"testuser"},
			visibleIDs: []domain.InstanceID{protocol.UserClientID},
			pending:    []domain.InstanceID{botty.ID()},
			events: []domain.Event{domain.Quit{
				Source:  domain.ClientSource(botty.ID(), "botty"),
				Message: "Killed by testuser (spam)",
				At:      fixedTime,
			}},
		}, struct {
			response   protocol.Response
			connected  bool
			attached   []protocol.ClientID
			forgotten  []protocol.ClientID
			members    []domain.Nick
			visibleIDs []domain.InstanceID
			pending    []domain.InstanceID
			events     []domain.Event
		}{
			response:   resp,
			connected:  sess.clientOwner(protocol.ClientID(botty.ID())) != nil,
			attached:   factory.attached(),
			forgotten:  factory.forgottenIDs(),
			members:    memberNicks(window),
			visibleIDs: instanceIDs(t, backing),
			pending:    pending,
			events:     collectEmittedEvents(t, sess),
		})
		require.Equal(t, []domain.InstanceID{botty.ID()}, failing.markedIDs())
	})
}

func TestSession_forced_kill_discards_a_recovered_tombstone_error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		deleteErr := errors.New("delete failed")
		markErr := errors.New("mark failed")
		failing := &teardownFailureStore{
			Store:              backing,
			markInstanceErr:    markErr,
			deleteInstanceErrs: []error{deleteErr, nil},
		}
		factory := newTestModelClientFactory(t, &apitest.Fake{})
		sess := New(t.Context(), failing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		botty := seedInstance(t, sess, backing, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
		failing.instanceID = botty.ID()
		failing.armed.Store(true)

		resp, err := userClient(t, sess).Send(t.Context(), protocol.Kill{Nick: "botty", Reason: "spam"})
		require.NoError(t, err)
		synctest.Wait()
		pending, pendingErr := backing.ListPendingInstanceDeletions(t.Context())
		require.NoError(t, pendingErr)

		require.Equal(t, struct {
			response  protocol.Response
			connected bool
			attached  []protocol.ClientID
			forgotten []protocol.ClientID
			active    []domain.InstanceID
			pending   []domain.InstanceID
			marked    []domain.InstanceID
		}{
			response:  protocol.Response{},
			attached:  []protocol.ClientID{},
			forgotten: []protocol.ClientID{protocol.ClientID(botty.ID())},
			active:    []domain.InstanceID{protocol.UserClientID},
			pending:   nil,
			marked:    []domain.InstanceID{botty.ID()},
		}, struct {
			response  protocol.Response
			connected bool
			attached  []protocol.ClientID
			forgotten []protocol.ClientID
			active    []domain.InstanceID
			pending   []domain.InstanceID
			marked    []domain.InstanceID
		}{
			response:  resp,
			connected: sess.clientOwner(protocol.ClientID(botty.ID())) != nil,
			attached:  factory.attached(),
			forgotten: factory.forgottenIDs(),
			active:    instanceIDs(t, backing),
			pending:   pending,
			marked:    failing.markedIDs(),
		})
	})
}

func TestSession_forced_kill_retries_tombstone_after_delete_failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		deleteErr := errors.New("delete failed")
		markErr := errors.New("mark failed")
		failing := &teardownFailureStore{
			Store:             backing,
			deleteInstanceErr: deleteErr,
			markInstanceErrs:  []error{markErr, nil},
		}
		factory := newTestModelClientFactory(t, &apitest.Fake{})
		sess := New(t.Context(), failing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		botty := seedInstance(t, sess, backing, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
		failing.instanceID = botty.ID()
		failing.armed.Store(true)

		resp, err := userClient(t, sess).Send(t.Context(), protocol.Kill{Nick: "botty", Reason: "spam"})
		require.ErrorIs(t, err, deleteErr)
		synctest.Wait()
		pending, pendingErr := backing.ListPendingInstanceDeletions(t.Context())
		require.NoError(t, pendingErr)

		require.Equal(t, struct {
			response  protocol.Response
			connected bool
			attached  []protocol.ClientID
			forgotten []protocol.ClientID
			active    []domain.InstanceID
			pending   []domain.InstanceID
			marked    []domain.InstanceID
		}{
			response:  protocol.Response{},
			attached:  []protocol.ClientID{},
			forgotten: nil,
			active:    []domain.InstanceID{protocol.UserClientID},
			pending:   []domain.InstanceID{botty.ID()},
			marked:    []domain.InstanceID{botty.ID(), botty.ID()},
		}, struct {
			response  protocol.Response
			connected bool
			attached  []protocol.ClientID
			forgotten []protocol.ClientID
			active    []domain.InstanceID
			pending   []domain.InstanceID
			marked    []domain.InstanceID
		}{
			response:  resp,
			connected: sess.clientOwner(protocol.ClientID(botty.ID())) != nil,
			attached:  factory.attached(),
			forgotten: factory.forgottenIDs(),
			active:    instanceIDs(t, backing),
			pending:   pending,
			marked:    failing.markedIDs(),
		})
	})
}

func TestSession_Shutdown_waits_for_teardown_cleanup(t *testing.T) {
	type teardownResult struct {
		response protocol.Response
		err      error
	}

	for _, tt := range []struct {
		name  string
		start func(context.Context, *Session, domain.InstanceID) <-chan teardownResult
	}{
		{
			name: "kill",
			start: func(ctx context.Context, sess *Session, _ domain.InstanceID) <-chan teardownResult {
				done := make(chan teardownResult, 1)
				go func() {
					response, err := userClient(t, sess).Send(ctx, protocol.Kill{Nick: "botty", Reason: "spam"})
					done <- teardownResult{response: response, err: err}
				}()

				return done
			},
		},
		{
			name: "quit",
			start: func(ctx context.Context, sess *Session, id domain.InstanceID) <-chan teardownResult {
				done := make(chan teardownResult, 1)
				go func() {
					response, err := sess.clientOwner(protocol.ClientID(id)).Send(ctx, protocol.Quit{Reason: "bye"})
					done <- teardownResult{response: response, err: err}
				}()

				return done
			},
		},
		{
			name: "disconnect",
			start: func(ctx context.Context, sess *Session, id domain.InstanceID) <-chan teardownResult {
				done := make(chan teardownResult, 1)
				go func() {
					sess.Disconnect(ctx, protocol.ClientID(id), sendQExceededReason)
					done <- teardownResult{}
				}()

				return done
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				backing := storetest.NewMemoryStore(t)
				baseFactory := newTestModelClientFactory(t, &apitest.Fake{})
				factory := &blockingForgetFactory{
					ModelClientFactory: baseFactory,
					started:            make(chan protocol.ClientID, 1),
					release:            make(chan struct{}),
				}
				sess := New(t.Context(), backing, factory, nil)
				attachTestUserClient(t, sess, "testuser")
				sess.now = func() time.Time { return fixedTime }

				botty := seedInstance(t, sess, backing, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#general"),
				})
				seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")

				teardownDone := tt.start(t.Context(), sess, botty.ID())
				require.Equal(t, protocol.ClientID(botty.ID()), <-factory.started)

				shutdownDone := make(chan error, 1)
				go func() { shutdownDone <- sess.Shutdown(t.Context()) }()
				synctest.Wait()

				returnedEarly := false
				var shutdownErr error
				select {
				case shutdownErr = <-shutdownDone:
					returnedEarly = true
				default:
				}

				close(factory.release)
				teardown := <-teardownDone
				if !returnedEarly {
					shutdownErr = <-shutdownDone
				}
				require.Equal(t, struct {
					returnedEarly bool
					teardown      teardownResult
					shutdownErr   error
				}{
					teardown: teardownResult{response: protocol.Response{}},
				}, struct {
					returnedEarly bool
					teardown      teardownResult
					shutdownErr   error
				}{
					returnedEarly: returnedEarly,
					teardown:      teardown,
					shutdownErr:   shutdownErr,
				})
			})
		})
	}
}

func TestSession_forced_disconnect_reaps_client_when_instance_deletion_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &apitest.Fake{
			SendEventsFn: func(ctx context.Context, _ domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				<-ctx.Done()

				return api.CompletionResult{}, ctx.Err()
			},
		}
		backing := storetest.NewMemoryStore(t)
		failing := &teardownFailureStore{
			Store:             backing,
			saveWindowErr:     errors.New("save window failed"),
			deleteInstanceErr: errors.New("delete instance failed"),
		}
		factory := newTestModelClientFactory(t, fake)
		sess := New(t.Context(), failing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		botty := seedInstance(t, sess, backing, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
		collectEmittedEvents(t, sess)

		failing.instanceID = botty.ID()
		failing.armed.Store(true)

		dispatchUserMessage(t.Context(), t, sess, "#general", "are you there?")
		sess.Disconnect(t.Context(), protocol.ClientID(botty.ID()), sendQExceededReason)
		synctest.Wait()

		window, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)
		pending, err := backing.ListPendingInstanceDeletions(t.Context())
		require.NoError(t, err)

		require.Equal(t, struct {
			connected  bool
			attached   []protocol.ClientID
			forgotten  []protocol.ClientID
			members    []domain.Nick
			visibleIDs []domain.InstanceID
			pending    []domain.InstanceID
			eventTypes []string
		}{
			connected:  false,
			attached:   []protocol.ClientID{},
			forgotten:  nil,
			members:    []domain.Nick{"testuser"},
			visibleIDs: []domain.InstanceID{""},
			pending:    []domain.InstanceID{botty.ID()},
			eventTypes: []string{"message", "quit"},
		}, struct {
			connected  bool
			attached   []protocol.ClientID
			forgotten  []protocol.ClientID
			members    []domain.Nick
			visibleIDs []domain.InstanceID
			pending    []domain.InstanceID
			eventTypes []string
		}{
			connected:  sess.clientOwner(protocol.ClientID(botty.ID())) != nil,
			attached:   factory.attached(),
			forgotten:  factory.forgottenIDs(),
			members:    memberNicks(window),
			visibleIDs: instanceIDs(t, backing),
			pending:    pending,
			eventTypes: channelEventTypes(t, backing, "#general"),
		})
		require.Equal(t, []domain.InstanceID{botty.ID()}, failing.markedIDs())
	})
}

func TestSession_failed_add_reaps_client_when_instance_deletion_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		saveSentinel := errors.New("save window failed")
		failing := &teardownFailureStore{
			Store:             backing,
			saveWindowErr:     saveSentinel,
			deleteInstanceErr: errors.New("delete instance failed"),
		}
		factory := newTestModelClientFactory(t, &apitest.Fake{})
		sess := New(t.Context(), failing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		require.NoError(t, userJoin(t.Context(), t, sess, "#general"))
		collectEmittedEvents(t, sess)
		failing.armed.Store(true)

		err := addModelViaWire(t.Context(), t, sess, "#general", "test/model", "")
		require.ErrorIs(t, err, saveSentinel)
		synctest.Wait()

		window, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)
		_, resolveErr := backing.ResolveNick(t.Context(), "fakenick")
		require.ErrorIs(t, resolveErr, storemod.ErrNoSuchNick)
		pending, err := backing.ListPendingInstanceDeletions(t.Context())
		require.NoError(t, err)
		marked := failing.markedIDs()

		require.Equal(t, struct {
			attached   []protocol.ClientID
			forgotten  []protocol.ClientID
			members    []domain.Nick
			visibleIDs []domain.InstanceID
			pending    []domain.InstanceID
			events     []domain.Event
		}{
			attached:   []protocol.ClientID{},
			forgotten:  nil,
			members:    []domain.Nick{"testuser"},
			visibleIDs: []domain.InstanceID{""},
			pending:    marked,
			events:     nil,
		}, struct {
			attached   []protocol.ClientID
			forgotten  []protocol.ClientID
			members    []domain.Nick
			visibleIDs []domain.InstanceID
			pending    []domain.InstanceID
			events     []domain.Event
		}{
			attached:   factory.attached(),
			forgotten:  factory.forgottenIDs(),
			members:    memberNicks(window),
			visibleIDs: instanceIDs(t, backing),
			pending:    pending,
			events:     collectEmittedEvents(t, sess),
		})
	})
}

func TestSession_discardModel_retries_a_failed_tombstone(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	deleteErr := errors.New("delete failed")
	markErr := errors.New("mark failed")
	failing := &teardownFailureStore{
		Store:             backing,
		deleteInstanceErr: deleteErr,
		markInstanceErrs:  []error{markErr, nil},
	}
	factory := newTestModelClientFactory(t, &apitest.Fake{})
	sess := New(t.Context(), failing, factory, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
	attachTestUserClient(t, sess, "testuser")

	botty := seedInstance(t, sess, backing, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})
	failing.instanceID = botty.ID()
	failing.armed.Store(true)

	sess.discardModel(t.Context(), botty)
	pending, err := backing.ListPendingInstanceDeletions(t.Context())
	require.NoError(t, err)

	require.Equal(t, struct {
		connected bool
		attached  []protocol.ClientID
		forgotten []protocol.ClientID
		active    []domain.InstanceID
		pending   []domain.InstanceID
		marked    []domain.InstanceID
	}{
		attached:  []protocol.ClientID{},
		forgotten: nil,
		active:    []domain.InstanceID{protocol.UserClientID},
		pending:   []domain.InstanceID{botty.ID()},
		marked:    []domain.InstanceID{botty.ID(), botty.ID()},
	}, struct {
		connected bool
		attached  []protocol.ClientID
		forgotten []protocol.ClientID
		active    []domain.InstanceID
		pending   []domain.InstanceID
		marked    []domain.InstanceID
	}{
		connected: sess.clientOwner(protocol.ClientID(botty.ID())) != nil,
		attached:  factory.attached(),
		forgotten: factory.forgottenIDs(),
		active:    instanceIDs(t, backing),
		pending:   pending,
		marked:    failing.markedIDs(),
	})
}

func TestSession_discardModel_records_a_tombstone_after_session_cancellation(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	factory := newTestModelClientFactory(t, &apitest.Fake{})
	baseCtx, cancelBase := context.WithCancel(t.Context())
	sess := New(baseCtx, backing, factory, nil)

	inst := seedInstance(t, sess, backing, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})
	cancelBase()
	require.NoError(t, sess.Shutdown(context.Background()))

	sess.discardModel(t.Context(), inst)

	pending, err := backing.ListPendingInstanceDeletions(t.Context())
	require.NoError(t, err)
	require.Equal(t, struct {
		connected bool
		attached  []protocol.ClientID
		forgotten []protocol.ClientID
		active    []domain.InstanceID
		pending   []domain.InstanceID
	}{
		connected: false,
		attached:  []protocol.ClientID{},
		forgotten: nil,
		active:    []domain.InstanceID{},
		pending:   []domain.InstanceID{inst.ID()},
	}, struct {
		connected bool
		attached  []protocol.ClientID
		forgotten []protocol.ClientID
		active    []domain.InstanceID
		pending   []domain.InstanceID
	}{
		connected: sess.clientOwner(protocol.ClientID(inst.ID())) != nil,
		attached:  factory.attached(),
		forgotten: factory.forgottenIDs(),
		active:    instanceIDs(t, backing),
		pending:   pending,
	})
}

func TestSession_Shutdown_waits_for_add_model_rollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		baseCtx, cancelBase := context.WithCancel(t.Context())
		backing := storetest.NewMemoryStore(t)
		delegate := newTestModelClientFactory(t, &apitest.Fake{})
		factory := &blockingAttachFactory{
			ModelClientFactory: delegate,
			attached:           make(chan domain.InstanceID, 1),
			release:            make(chan struct{}),
		}
		sess := New(baseCtx, backing, factory, nil)
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }
		require.NoError(t, sess.Connect(t.Context()))
		require.NoError(t, userJoin(t.Context(), t, sess, "#general"))

		type addModelResult struct {
			response protocol.Response
			err      error
		}
		user := userClient(t, sess)
		addDone := make(chan addModelResult, 1)
		go func() {
			resp, err := user.Send(t.Context(), protocol.AddModel{
				Channel: "#general",
				Model:   "test/model",
			})
			addDone <- addModelResult{response: resp, err: err}
		}()
		id := <-factory.attached

		cancelBase()
		<-sess.writerStopped
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- sess.Shutdown(context.Background()) }()
		synctest.Wait()

		var shutdownErr error
		returnedEarly := false
		select {
		case shutdownErr = <-shutdownDone:
			returnedEarly = true
		default:
		}

		close(factory.release)
		result := <-addDone
		require.ErrorIs(t, result.err, ErrSessionClosed)
		require.Equal(t, protocol.Response{}, result.response)
		if !returnedEarly {
			shutdownErr = <-shutdownDone
		}

		active := instanceIDs(t, backing)
		pending, err := backing.ListPendingInstanceDeletions(t.Context())
		require.NoError(t, err)
		require.Equal(t, struct {
			returnedEarly bool
			shutdownErr   error
			active        []domain.InstanceID
			pending       []domain.InstanceID
		}{
			active:  []domain.InstanceID{protocol.UserClientID},
			pending: []domain.InstanceID{id},
		}, struct {
			returnedEarly bool
			shutdownErr   error
			active        []domain.InstanceID
			pending       []domain.InstanceID
		}{
			returnedEarly: returnedEarly,
			shutdownErr:   shutdownErr,
			active:        active,
			pending:       pending,
		})
	})
}

func TestSession_add_model_refuses_a_client_killed_before_admission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		delegate := newTestModelClientFactory(t, &apitest.Fake{})
		factory := &blockingAttachFactory{
			ModelClientFactory: delegate,
			attached:           make(chan domain.InstanceID, 1),
			release:            make(chan struct{}),
		}
		sess := New(t.Context(), backing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }
		require.NoError(t, userJoin(t.Context(), t, sess, "#general"))

		type addModelResult struct {
			response protocol.Response
			err      error
		}
		user := userClient(t, sess)
		addDone := make(chan addModelResult, 1)
		go func() {
			resp, err := user.Send(t.Context(), protocol.AddModel{
				Channel: "#general",
				Model:   "test/model",
			})
			addDone <- addModelResult{response: resp, err: err}
		}()
		id := <-factory.attached

		killResp, err := user.Send(t.Context(), protocol.Kill{Nick: "fakenick", Reason: "gone"})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{}, killResp)
		close(factory.release)

		result := <-addDone
		require.ErrorIs(t, result.err, protocol.ErrSubscriptionClosed)
		window, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)
		pending, err := backing.ListPendingInstanceDeletions(t.Context())
		require.NoError(t, err)
		require.Equal(t, struct {
			response           protocol.Response
			subscriptionClosed bool
			connected          bool
			members            []domain.Nick
			active             []domain.InstanceID
			pending            []domain.InstanceID
		}{
			response:           protocol.Response{},
			subscriptionClosed: true,
			members:            []domain.Nick{"testuser"},
			active:             []domain.InstanceID{protocol.UserClientID},
		}, struct {
			response           protocol.Response
			subscriptionClosed bool
			connected          bool
			members            []domain.Nick
			active             []domain.InstanceID
			pending            []domain.InstanceID
		}{
			response:           result.response,
			subscriptionClosed: errors.Is(result.err, protocol.ErrSubscriptionClosed),
			connected:          sess.clientOwner(protocol.ClientID(id)) != nil,
			members:            memberNicks(window),
			active:             instanceIDs(t, backing),
			pending:            pending,
		})
	})
}

func TestSession_failed_self_kill_keeps_recovery_marker_until_restart_reconciles_membership(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "modeloff.db")
	db, err := sql.Open("sqlite3", storemod.SQLitePragmaDSN(path))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	backing, err := storemod.NewSQLiteStore(ctx, db)
	require.NoError(t, err)

	saveErr := errors.New("save window failed")
	deleteErr := errors.New("delete instance failed")
	failing := &teardownFailureStore{
		Store:             backing,
		instanceID:        domain.InstanceID(protocol.UserClientID),
		saveWindowErr:     saveErr,
		deleteInstanceErr: deleteErr,
	}
	factory := newTestModelClientFactory(t, &apitest.Fake{})
	sess := New(t.Context(), failing, factory, nil)
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }
	require.NoError(t, sess.Connect(ctx))
	drainDeliveries(userClient(t, sess))
	require.NoError(t, userJoin(ctx, t, sess, "#general"))
	botty := seedInstance(t, sess, backing, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
	failing.armed.Store(true)

	resp, killErr := userClient(t, sess).Send(ctx, protocol.Kill{Nick: "testuser", Reason: "enough"})
	require.ErrorIs(t, killErr, saveErr)
	require.Equal(t, protocol.Response{}, resp)
	marker, err := backing.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Equal(t, fixedTime.Format(time.RFC3339Nano), marker)
	pending, err := backing.ListPendingInstanceDeletions(ctx)
	require.NoError(t, err)
	require.Equal(t, struct {
		pending []domain.InstanceID
		marked  []domain.InstanceID
	}{
		pending: nil,
		marked:  nil,
	}, struct {
		pending []domain.InstanceID
		marked  []domain.InstanceID
	}{
		pending: pending,
		marked:  failing.markedIDs(),
	})

	require.NoError(t, sess.Shutdown(context.Background()))
	require.NoError(t, backing.Close())

	reopenedDB, err := sql.Open("sqlite3", storemod.SQLitePragmaDSN(path))
	require.NoError(t, err)
	reopenedDB.SetMaxOpenConns(1)
	reopened, err := storemod.NewSQLiteStore(ctx, reopenedDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })

	nextFactory := newTestModelClientFactory(t, &apitest.Fake{})
	next := New(t.Context(), reopened, nextFactory, nil)
	t.Cleanup(func() { _ = next.Shutdown(context.Background()) })
	attachTestUserClient(t, next, "testuser")
	next.now = func() time.Time { return fixedTime }
	require.NoError(t, next.Connect(ctx))

	window, err := next.loadChannelWindow(ctx, "#general")
	require.NoError(t, err)
	marker, err = reopened.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Equal(t, struct {
		members   []domain.Nick
		activeIDs []domain.InstanceID
		marker    string
	}{
		members:   []domain.Nick{botty.Nick()},
		activeIDs: []domain.InstanceID{botty.ID(), protocol.UserClientID},
		marker:    fixedTime.Format(time.RFC3339Nano),
	}, struct {
		members   []domain.Nick
		activeIDs []domain.InstanceID
		marker    string
	}{
		members:   memberNicks(window),
		activeIDs: instanceIDs(t, reopened),
		marker:    marker,
	})
}

func TestSession_user_reactivation_restores_identity_before_reconciling_membership(t *testing.T) {
	ctx := t.Context()
	backing := storetest.NewMemoryStore(t)
	reloadErr := errors.New("reload window failed")
	failing := &teardownFailureStore{Store: backing, getWindowErr: reloadErr}
	factory := newTestModelClientFactory(t, &apitest.Fake{})
	sess := New(t.Context(), failing, factory, nil)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }
	require.NoError(t, sess.Connect(ctx))
	client := userClient(t, sess)
	drainDeliveries(client)
	require.NoError(t, userJoin(ctx, t, sess, "#general"))
	botty := seedInstance(t, sess, backing, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
	failing.armed.Store(true)

	resp, killErr := client.Send(ctx, protocol.Kill{Nick: "testuser", Reason: "enough"})
	require.ErrorIs(t, killErr, reloadErr)
	require.Equal(t, protocol.Response{}, resp)
	failing.armed.Store(false)
	require.NoError(t, sess.Connect(ctx))

	window, err := sess.loadChannelWindow(ctx, "#general")
	require.NoError(t, err)
	stored, err := backing.ResolveNick(ctx, "testuser")
	require.NoError(t, err)
	require.Equal(t, struct {
		members  []domain.Nick
		storedID domain.InstanceID
		active   protocol.Client
	}{
		members:  []domain.Nick{botty.Nick()},
		storedID: protocol.UserClientID,
		active:   client,
	}, struct {
		members  []domain.Nick
		storedID domain.InstanceID
		active   protocol.Client
	}{
		members:  memberNicks(window),
		storedID: stored.ID(),
		active:   sess.clientOwner(protocol.UserClientID),
	})
}

func TestSession_mixed_pass_is_rejected_and_continues_the_tool_loop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var rejected []api.ToolResult
		message := msgToolCalls(t, "#general", "must not be sent").PendingToolCalls[0]
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
					{ID: "pass", Name: "pass", Args: json.RawMessage(`{"reason":"done"}`)},
					message,
				}}, nil
			},
			ContinueWithToolResultsFn: continueOnceWith(&rejected, msgToolCalls(t, "#general", "clean reply")),
		}
		sess, eventStore := newTestSessionWithAPI(t, fake)
		seedInstance(t, sess, eventStore, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, eventStore, "#general", "testuser", "botty")

		dispatchUserMessage(t.Context(), t, sess, "#general", "hello")

		outcomes := make([]bool, len(rejected))
		for index, result := range rejected {
			var payload modelclient.ToolResultPayload
			require.NoError(t, json.Unmarshal([]byte(result.Content), &payload))
			outcomes[index] = payload.OK
		}
		require.Equal(t, []bool{false, false}, outcomes)
		require.Equal(t, []domain.Message{
			{Target: "#general", Source: domain.ClientSource(domain.InstanceID(""), domain.Nick("testuser")), Body: "hello", At: fixedTime},
			{Target: "#general", Source: domain.ClientSource(testMemberID("botty"), domain.Nick("botty")), Body: "clean reply", At: fixedTime},
		}, channelMessages(t, eventStore, "#general"))
	})
}
