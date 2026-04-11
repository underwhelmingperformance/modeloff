// Package session provides the backend coordinator that ties together
// stores, the API client, and the protocol layer. It manages channels,
// model instances, and handles commands by updating state and emitting
// domain events.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// eventBufSize is the capacity of the session event channel. It must
// be large enough that normal event bursts (join + mode change, message
// + dispatch started/done) don't block callers. Anything that writes
// to this channel without a consumer draining it risks deadlock.
const eventBufSize = 64

// Session is the backend coordinator. It bridges the UI layer and
// the underlying stores and API client.
type Session struct {
	store  store.Store
	memory memory.Store
	api    api.Client

	user       *domain.Instance
	apiKey     string
	smallModel domain.ModelID
	factory   func(apiKey, baseURL string) (api.Client, error)
	now       func() time.Time
	events    chan domain.SessionEvent

	supportedModels      map[domain.ModelID]struct{}
	supportedModelsReady bool
}

// New creates a Session with the given dependencies.
func New(
	s store.Store,
	m memory.Store,
	a api.Client,
	userNick domain.Nick,
	apiKey string,
	smallModel domain.ModelID,
) *Session {
	if smallModel == "" {
		smallModel = config.DefaultSmallModel
	}

	return &Session{
		store:      s,
		memory:     m,
		api:        a,
		apiKey:     strings.TrimSpace(apiKey),
		smallModel: smallModel,
		user: &domain.Instance{
			Nick:     userNick,
			Channels: orderedmap.New[domain.ChannelName, time.Time](),
		},
		now:    time.Now,
		events: make(chan domain.SessionEvent, eventBufSize),
	}
}

// Events returns the channel on which background dispatch events are
// emitted. The caller should drain this channel to receive
// DispatchStartedEvent, ModelReplyEvent, DispatchDoneEvent, and
// ErrorEvent values.
func (s *Session) Events() <-chan domain.SessionEvent {
	return s.events
}

// SetAPIFactory configures how runtime API clients are created.
func (s *Session) SetAPIFactory(factory func(apiKey, baseURL string) (api.Client, error)) {
	s.factory = factory
}

// HasAPIKey reports whether the session has an active API key.
func (s *Session) HasAPIKey() bool {
	return strings.TrimSpace(s.apiKey) != ""
}

// UserNick returns the current user nickname.
func (s *Session) UserNick() domain.Nick {
	return s.user.Nick
}

// UserJoinedAt returns the time the user joined the given channel,
// or the zero time if the user is not in the channel.
func (s *Session) UserJoinedAt(ch domain.ChannelName) time.Time {
	if t, ok := s.user.Channels.Get(ch); ok {
		return t
	}

	return time.Time{}
}

// Join creates or opens a channel. Events are emitted on the
// session event channel.
func (s *Session) Join(ctx context.Context, channelName string) error {
	name := domain.ChannelName(channelName)
	ctx, span := startSpan(
		ctx,
		"session.join",
		attribute.String(observability.AttrOperation, "session.join"),
		attribute.String(observability.AttrChannel, string(name)),
	)
	defer span.End()

	now := s.now()

	var created bool

	_, err := s.store.GetChannel(ctx, name)
	if err != nil {
		created = true

		members := domain.NewMemberList()
		members.Add(s.user.Nick)

		ch := domain.Channel{
			Name:    name,
			Kind:    domain.KindChannel,
			Members: members,
			Created: now,
		}

		if err := s.store.SaveChannel(ctx, ch); err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("save channel: %w", err)
		}
	}

	if err := s.store.SetLastChannel(ctx, name); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("set last channel: %w", err)
	}

	if err := s.MarkRead(ctx, name); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("mark read: %w", err)
	}

	s.user.Channels.Set(name, now)

	s.appendEvent(ctx, name, domain.ChannelJoin{
		Channel: name,
		Nick:    s.user.Nick,
		Created: created,
		At:      now,
	})

	s.emit(ctx, domain.JoinEvent{
		Channel: name,
		Nick:    s.user.Nick,
		Created: created,
		At:      now,
	})

	if created {
		ch, _ := s.store.GetChannel(ctx, name)
		ch.Members.SetMode(s.user.Nick, domain.ModeOp)

		if err := s.store.SaveChannel(ctx, ch); err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("save channel after mode: %w", err)
		}

		s.appendEvent(ctx, name, domain.ChannelModeChange{
			Channel: name,
			Nick:    s.user.Nick,
			Mode:    domain.ModeOp,
			At:      now,
		})

		s.emit(ctx, domain.ModeChangeEvent{
			Channel: name,
			Nick:    s.user.Nick,
			Mode:    domain.ModeOp,
			Actor:   "ChanServ",
			At:      now,
		})
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

// Part records the user leaving a channel. An optional farewell
// message is included in the event. Events are emitted on the
// session event channel.
func (s *Session) Part(ctx context.Context, ch domain.ChannelName, message string) error {
	ctx, span := startSpan(
		ctx,
		"session.part",
		attribute.String(observability.AttrOperation, "session.part"),
		attribute.String(observability.AttrChannel, string(ch)),
	)
	defer span.End()

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("channel not found: %w", err)
	}

	if m, ok := channel.Members.Get(s.user.Nick); ok {
		channel.Members.Remove(m)
	}

	s.user.Channels.Delete(ch)

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("save channel: %w", err)
	}

	now := s.now()

	s.appendEvent(ctx, ch, domain.ChannelPart{
		Channel: ch,
		Nick:    s.user.Nick,
		Message: message,
		At:      now,
	})

	s.emit(ctx, domain.PartEvent{
		Channel: ch,
		Nick:    s.user.Nick,
		Message: message,
		At:      now,
	})

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

// Quit builds a PendingQuit from the user's current channels and
// persists it. The caller is responsible for triggering tea.Quit
// after calling this. The pending quit is replayed on next startup
// via ProcessPendingQuit.
func (s *Session) Quit(ctx context.Context, message string) error {
	var channels []domain.ChannelName

	for pair := s.user.Channels.Oldest(); pair != nil; pair = pair.Next() {
		channels = append(channels, pair.Key)
	}

	if len(channels) == 0 {
		return nil
	}

	pq := domain.PendingQuit{
		Nick:     s.user.Nick,
		Message:  message,
		At:       s.now(),
		Channels: channels,
	}

	return s.store.SavePendingQuit(ctx, pq)
}

// ProcessPendingQuit loads a pending quit from the store and dispatches
// QUIT to models in each channel synchronously, saving their replies.
// The pending quit is cleared after processing. A 30-second timeout
// is applied.
func (s *Session) ProcessPendingQuit(ctx context.Context) error {
	pq, err := s.store.GetPendingQuit(ctx)
	if err != nil {
		return fmt.Errorf("get pending quit: %w", err)
	}

	if pq == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	quitEvt := domain.QuitEvent{
		Nick:    pq.Nick,
		Message: pq.Message,
		At:      pq.At,
	}

	quitMsg := protocol.FromQuitEvent(quitEvt)

	for _, ch := range pq.Channels {
		s.appendEvent(ctx, ch, domain.ChannelQuit{
			Channel: ch,
			Nick:    pq.Nick,
			Message: pq.Message,
			At:      pq.At,
		})

		if _, err := s.DispatchToChannel(ctx, ch, []protocol.IRCMessage{quitMsg}); err != nil {
			slog.Default().ErrorContext(ctx, "process pending quit dispatch", "channel", ch, "error", err)
		}
	}

	return s.store.ClearPendingQuit(ctx)
}

// ListChannels returns all persisted channels.
func (s *Session) ListChannels(ctx context.Context) ([]domain.Channel, error) {
	return s.store.ListChannels(ctx)
}

// ListInstances returns all persisted model instances.
func (s *Session) ListInstances(ctx context.Context) ([]domain.Instance, error) {
	return s.store.ListInstances(ctx)
}

// Invite adds a model instance to a channel. If the model has no nick
// yet, one is generated via the API.
func (s *Session) Invite(
	ctx context.Context,
	ch domain.ChannelName,
	modelID domain.ModelID,
	persona string,
) error {
	logger := slog.Default().With("component", "session", "channel", ch, "model_id", modelID)
	ctx, span := startSpan(ctx, "session.invite", attribute.String(observability.AttrOperation, "session.invite"))
	defer span.End()

	if inst, err := s.store.GetInstance(ctx, domain.Nick(modelID)); err == nil {
		if inst.Persona == "" && strings.TrimSpace(persona) != "" {
			inst.Persona = strings.TrimSpace(persona)
		}

		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		return s.attachInstanceToChannel(ctx, ch, inst)
	}

	if err := s.ensureStructuredOutputModel(ctx, modelID); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if inst, err := s.findInstanceByModelID(ctx, modelID); err == nil {
		if inst.Persona == "" && strings.TrimSpace(persona) != "" {
			inst.Persona = strings.TrimSpace(persona)
		}

		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		return s.attachInstanceToChannel(ctx, ch, inst)
	}

	generateCtx, generateSpan := startSpan(
		ctx,
		"session.generate_nick",
		attribute.String(observability.AttrOperation, "session.generate_nick"),
		attribute.String(observability.AttrModelID, string(modelID)),
	)
	defer generateSpan.End()

	nickResult, err := s.api.GenerateNick(generateCtx, s.smallModel, modelID)
	if err != nil {
		generateSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		generateSpan.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "generate nick failed", "error", err)
		return fmt.Errorf("generate nick: %w", err)
	}

	nickResult.Usage.SetSpanAttributes(generateSpan, nickResult.RequestID)
	generateSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	channels := orderedmap.New[domain.ChannelName, time.Time]()
	channels.Set(ch, s.now())

	inst := domain.Instance{
		Nick:     nickResult.Nick,
		ModelID:  modelID,
		Persona:  strings.TrimSpace(persona),
		Channels: channels,
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return s.attachInstanceToChannel(ctx, ch, inst)
}

func (s *Session) findInstanceByModelID(ctx context.Context, modelID domain.ModelID) (domain.Instance, error) {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return domain.Instance{}, err
	}

	for _, inst := range instances {
		if inst.ModelID == modelID {
			return inst, nil
		}
	}

	return domain.Instance{}, fmt.Errorf("no instance with model ID %s", modelID)
}

func (s *Session) attachInstanceToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	inst domain.Instance,
) error {
	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if inst.Channels == nil {
		inst.Channels = orderedmap.New[domain.ChannelName, time.Time]()
	}

	if _, ok := inst.Channels.Get(ch); !ok {
		inst.Channels.Set(ch, s.now())
	}

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	isNew := !channel.Members.Has(inst.Nick)
	if isNew {
		channel.Members.Add(inst.Nick)
	}

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	now := s.now()

	s.appendEvent(ctx, ch, domain.ChannelModelInvited{
		Channel: ch,
		Nick:    inst.Nick,
		ModelID: inst.ModelID,
		Persona: inst.Persona,
		At:      now,
	})

	s.emit(ctx, domain.ModelInvitedEvent{
		Channel:  ch,
		Instance: inst,
		At:       now,
	})

	if isNew {
		channel.Members.SetMode(inst.Nick, domain.ModeVoice)

		if err := s.store.SaveChannel(ctx, channel); err != nil {
			return fmt.Errorf("save channel after mode: %w", err)
		}

		s.appendEvent(ctx, ch, domain.ChannelModeChange{
			Channel: ch,
			Nick:    inst.Nick,
			Mode:    domain.ModeVoice,
			At:      now,
		})

		s.emit(ctx, domain.ModeChangeEvent{
			Channel: ch,
			Nick:    inst.Nick,
			Mode:    domain.ModeVoice,
			Actor:   "ChanServ",
			At:      now,
		})
	}

	return nil
}

// Kick removes a model instance from a channel.
func (s *Session) Kick(
	ctx context.Context,
	ch domain.ChannelName,
	nick domain.Nick,
) error {
	ctx, span := startSpan(
		ctx,
		"session.kick",
		attribute.String(observability.AttrOperation, "session.kick"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(nick)),
	)
	defer span.End()

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("get channel: %w", err)
	}

	if m, ok := channel.Members.Get(nick); ok {
		channel.Members.Remove(m)
	}

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("save channel: %w", err)
	}

	inst, err := s.store.GetInstance(ctx, nick)
	if err == nil {
		inst.Channels.Delete(ch)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("save instance: %w", err)
		}
	}

	now := s.now()
	s.appendEvent(ctx, ch, domain.ChannelModelKicked{
		Channel: ch,
		Nick:    nick,
		At:      now,
	})

	s.emit(ctx, domain.ModelKickedEvent{
		Channel: ch,
		Nick:    nick,
		At:      now,
	})

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

// SendMessage saves a message to a channel and returns the message
// event. It also spawns a background goroutine to dispatch the
// message to model instances, emitting events on the Events channel.
func (s *Session) SendMessage(
	ctx context.Context,
	ch domain.ChannelName,
	body string,
) error {
	ctx, span := startSpan(ctx, "session.send_message", attribute.String(observability.AttrOperation, "session.send_message"))
	defer span.End()

	cm := domain.ChannelMessage{
		Channel: ch,
		From:    s.user.Nick,
		Body:    body,
		At:      s.now(),
	}

	s.appendEvent(ctx, ch, cm)

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	s.emit(ctx, domain.MessageEvent{Event: cm})

	return nil
}

// SendAction saves an action message (/me) to a channel and returns
// the message event. It also spawns a background goroutine to
// dispatch the action to model instances.
func (s *Session) SendAction(
	ctx context.Context,
	ch domain.ChannelName,
	body string,
) error {
	cm := domain.ChannelMessage{
		Channel: ch,
		From:    s.user.Nick,
		Body:    body,
		Action:  true,
		At:      s.now(),
	}

	s.appendEvent(ctx, ch, cm)

	s.emit(ctx, domain.MessageEvent{Event: cm})

	return nil
}

// DispatchToChannel sends new events to all model instances in a channel
// and collects their replies. The caller provides the new IRC-formatted
// events to broadcast; history is loaded from the store.
func (s *Session) DispatchToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	newEvents []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	ctx, span := startSpan(ctx, "session.dispatch_to_channel", attribute.String(observability.AttrOperation, "session.dispatch_to_channel"))
	defer span.End()

	historyEvents, err := s.store.EventsBefore(ctx, ch, nil, 500)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("list history: %w", err)
	}

	replies, err := s.dispatchToInstances(ctx, ch, historyEvents, newEvents)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return replies, nil
}

// SetTopic sets the topic of a channel.
func (s *Session) SetTopic(
	ctx context.Context,
	ch domain.ChannelName,
	topic string,
) error {
	ctx, span := startSpan(
		ctx,
		"session.set_topic",
		attribute.String(observability.AttrOperation, "session.set_topic"),
		attribute.String(observability.AttrChannel, string(ch)),
	)
	defer span.End()

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("get channel: %w", err)
	}

	channel.Topic = topic
	channel.TopicSetBy = s.user.Nick
	channel.TopicSetAt = s.now()

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("save channel: %w", err)
	}

	now := s.now()
	s.appendEvent(ctx, ch, domain.ChannelTopicChange{
		Channel: ch,
		Topic:   topic,
		By:      s.user.Nick,
		At:      now,
	})

	s.emit(ctx, domain.TopicChangeEvent{
		Channel: ch,
		Topic:   topic,
		By:      s.user.Nick,
		At:      now,
	})

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

// ChangeNick changes the user's nickname and updates all channel
// memberships accordingly.
func (s *Session) ChangeNick(
	ctx context.Context,
	newNick domain.Nick,
) error {
	ctx, span := startSpan(
		ctx,
		"session.change_nick",
		attribute.String(observability.AttrOperation, "session.change_nick"),
		attribute.String(observability.AttrNick, string(newNick)),
	)
	defer span.End()

	oldNick := s.user.Nick
	now := s.now()
	s.user.Nick = newNick

	// Update membership and emit a nick change event per channel.
	channels, _ := s.store.ListChannels(ctx)

	for _, ch := range channels {
		if !ch.Members.Has(oldNick) {
			continue
		}

		if m, ok := ch.Members.Get(oldNick); ok {
			ch.Members.Remove(m)
			ch.Members.Add(newNick)
			ch.Members.SetMode(newNick, m.Mode)
		}

		_ = s.store.SaveChannel(ctx, ch)

		s.appendEvent(ctx, ch.Name, domain.ChannelNickChange{
			Channel: ch.Name,
			OldNick: oldNick,
			NewNick: newNick,
			At:      now,
		})

		s.emit(ctx, domain.NickChangeEvent{
			Channel: ch.Name,
			OldNick: oldNick,
			NewNick: newNick,
			At:      now,
		})
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

// Whois returns metadata about a model instance.
func (s *Session) Whois(ctx context.Context, nick domain.Nick) (domain.Instance, error) {
	return s.store.GetInstance(ctx, nick)
}

// GetChannel retrieves a channel by name.
func (s *Session) GetChannel(ctx context.Context, name domain.ChannelName) (domain.Channel, error) {
	return s.store.GetChannel(ctx, name)
}

// LastChannel returns the channel that was last active.
func (s *Session) LastChannel(ctx context.Context) (domain.ChannelName, error) {
	return s.store.GetLastChannel(ctx)
}

// MarkRead records that the user has seen all current events in a
// channel by storing the rowid of the last event.
func (s *Session) MarkRead(ctx context.Context, ch domain.ChannelName) error {
	events, err := s.store.EventsBefore(ctx, ch, nil, 1)
	if err != nil {
		return fmt.Errorf("get latest event: %w", err)
	}

	if len(events) == 0 {
		return nil
	}

	return s.store.SetLastRead(ctx, ch, events[0].ID)
}

// UnreadCount returns the number of events in a channel that arrived
// after the last-read position.
func (s *Session) UnreadCount(ctx context.Context, ch domain.ChannelName) (int, error) {
	lastID, err := s.store.GetLastRead(ctx, ch)
	if err != nil {
		return 0, fmt.Errorf("get last read: %w", err)
	}

	if lastID == 0 {
		events, err := s.store.EventsBefore(ctx, ch, nil, 1000)
		if err != nil {
			return 0, err
		}

		return len(events), nil
	}

	fromID := lastID + 1
	events, err := s.store.EventsFrom(ctx, ch, &fromID, 1000)
	if err != nil {
		return 0, err
	}

	return len(events), nil
}

// ListModels fetches live model metadata using the current API client.
func (s *Session) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	ctx, span := startSpan(ctx, "session.list_models", attribute.String(observability.AttrOperation, "session.list_models"))
	defer span.End()

	if !s.HasAPIKey() || s.api == nil {
		err := fmt.Errorf("api key not configured")
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	models, err := s.api.ListModels(ctx)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	s.cacheSupportedModels(models)

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return models, nil
}

// Reset clears all channels, messages, model instances, and memories,
// returning the application to a fresh state. Config is preserved.
func (s *Session) Reset(ctx context.Context) error {
	ctx, span := startSpan(ctx, "session.reset", attribute.String(observability.AttrOperation, "session.reset"))
	defer span.End()

	if err := s.store.Reset(ctx); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("reset store: %w", err)
	}

	if s.memory != nil {
		if err := s.memory.Reset(ctx); err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("reset memories: %w", err)
		}
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

// OpenDM opens or creates a direct-message conversation with a known
// model instance and makes it the active conversation.
func (s *Session) OpenDM(ctx context.Context, nick domain.Nick) (domain.Channel, bool, error) {
	ctx, span := startSpan(
		ctx,
		"session.open_dm",
		attribute.String(observability.AttrOperation, "session.open_dm"),
		attribute.String(observability.AttrNick, string(nick)),
	)
	defer span.End()

	inst, err := s.store.GetInstance(ctx, nick)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return domain.Channel{}, false, fmt.Errorf("get instance: %w", err)
	}

	name := domain.ChannelName(nick)

	ch, err := s.store.GetChannel(ctx, name)
	created := false
	if err != nil {
		members := domain.NewMemberList()
		members.Add(s.user.Nick)
		members.SetMode(s.user.Nick, domain.ModeOp)
		members.Add(nick)
		members.SetMode(nick, domain.ModeVoice)

		ch = domain.Channel{
			Name:    name,
			Kind:    domain.KindDM,
			Members: members,
			Created: s.now(),
		}

		if err := s.store.SaveChannel(ctx, ch); err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			return domain.Channel{}, false, fmt.Errorf("save dm channel: %w", err)
		}

		created = true
	}

	if inst.Channels == nil {
		inst.Channels = orderedmap.New[domain.ChannelName, time.Time]()
	}

	if _, ok := inst.Channels.Get(name); !ok {
		inst.Channels.Set(name, s.now())

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			return domain.Channel{}, false, fmt.Errorf("save instance: %w", err)
		}
	}

	if err := s.store.SetLastChannel(ctx, name); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return domain.Channel{}, false, fmt.Errorf("set last channel: %w", err)
	}

	span.SetAttributes(
		attribute.String(observability.AttrChannel, string(name)),
		attribute.String(observability.AttrResult, observability.ResultOK),
	)

	return ch, created, nil
}

// Poke sends a periodic prompt to model instances in every channel,
// dispatching asynchronously and emitting events on the Events
// channel.
func (s *Session) Poke(ctx context.Context) error {
	logger := slog.Default().With("component", "session")
	ctx, span := startSpan(ctx, "session.poke", attribute.String(observability.AttrOperation, "session.poke"))
	defer span.End()

	channels, err := s.store.ListChannels(ctx)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("list channels: %w", err)
	}

	now := s.now()

	for _, ch := range channels {
		s.emit(ctx, domain.PokeEvent{
			Channel: ch.Name,
			At:      now,
		})
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	logger.DebugContext(ctx, "scheduled poke dispatch", "channels", len(channels))

	return nil
}

// SetAPIKey updates the active API key and rebuilds the API client.
func (s *Session) SetAPIKey(apiKey, baseURL string) error {
	apiKey = strings.TrimSpace(apiKey)

	var nextClient api.Client
	if apiKey != "" {
		if s.factory != nil {
			client, err := s.factory(apiKey, baseURL)
			if err != nil {
				return fmt.Errorf("build api client: %w", err)
			}

			nextClient = client
		} else {
			nextClient = s.api
		}
	}

	s.api = nextClient
	s.apiKey = apiKey
	s.supportedModels = nil
	s.supportedModelsReady = false

	return nil
}


// SetSmallModel updates the model used for lightweight tasks such as
// nick generation.
func (s *Session) SetSmallModel(modelID domain.ModelID) {
	s.smallModel = modelID
}


// SetBaseURL rebuilds the API client with the given base URL.
func (s *Session) SetBaseURL(baseURL string) error {
	baseURL = strings.TrimSpace(baseURL)

	if s.factory != nil && s.apiKey != "" {
		client, err := s.factory(s.apiKey, baseURL)
		if err != nil {
			return fmt.Errorf("build api client: %w", err)
		}

		s.api = client
	}

	return nil
}


func (s *Session) dispatchToInstances(
	ctx context.Context,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	channel, err := s.store.GetChannel(ctx, channelName)
	if err != nil {
		return nil, fmt.Errorf("get channel: %w", err)
	}

	instances, err := s.instancesForChannel(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("list instances for channel: %w", err)
	}

	var errs []error
	var replies []domain.ModelReplyEvent

	for _, inst := range instances {
		instReplies, instErr := s.dispatchToInstance(ctx, channel, inst, channelName, historyEvents, events)
		if instErr != nil {
			errs = append(errs, instErr)
		}

		replies = append(replies, instReplies...)

		for _, r := range instReplies {
			ircMsg, _ := protocol.FromChannelEvent(r.Event)
			events = append(events, ircMsg)
		}
	}

	return replies, errors.Join(errs...)
}

func (s *Session) dispatchToInstance(
	ctx context.Context,
	channel domain.Channel,
	inst domain.Instance,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	ctx, instanceSpan := startSpan(
		ctx,
		"session.dispatch_to_instance",
		attribute.String(observability.AttrOperation, "session.dispatch_to_instance"),
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrChannelKind, channelKindName(channel.Kind)),
	)
	defer instanceSpan.End()

	joinedAt, _ := inst.Channels.Get(channelName)

	history := make([]protocol.IRCMessage, 0, len(historyEvents))
	for _, se := range historyEvents {
		if !se.Event.ModelVisible() {
			continue
		}

		eventTime := domain.ChannelEventTime(se.Event)
		if !joinedAt.IsZero() && eventTime.Before(joinedAt) {
			continue
		}

		if msg, ok := protocol.FromChannelEvent(se.Event); ok {
			history = append(history, msg)
		}
	}

	if err := s.ensureStructuredOutputModel(ctx, inst.ModelID); err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("send events to %s: %w", inst.Nick, err)
	}

	memories, err := s.memoriesForInstance(ctx, inst.Nick)
	if err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("read memories for %s: %w", inst.Nick, err)
	}

	prompt := buildSystemPrompt(channel, inst, memories)

	var mem MemoryExecutor
	if s.memory != nil {
		mem = &instanceMemory{nick: inst.Nick, store: s.memory}
	}

	outcome, err := s.sendWithRetry(ctx, inst, prompt, history, events, mem)
	if err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("send events to %s: %w", inst.Nick, err)
	}

	result := outcome.result
	result.Usage.SetSpanAttributes(instanceSpan, result.RequestID)
	instanceAttrs := []attribute.KeyValue{
		attribute.String(observability.AttrResult, api.ResponseResultKind(result.Response)),
		attribute.Int(observability.AttrRetryCount, outcome.retryCount),
		attribute.Int(observability.AttrToolTurnCount, outcome.toolTurnCount),
	}
	if outcome.passReason != "" {
		instanceAttrs = append(instanceAttrs, attribute.String(observability.AttrPassReason, outcome.passReason))
	}
	instanceSpan.SetAttributes(instanceAttrs...)

	response := result.Response

	switch response.Kind {
	case protocol.ResponsePart:
		replies := s.buildReplies(ctx, channelName, inst.Nick, response.Messages)
		s.removeInstanceFromChannel(ctx, inst.Nick, channelName)

		now := s.now()

		s.appendEvent(ctx, channelName, domain.ChannelPart{
			Channel: channelName,
			Nick:    inst.Nick,
			Message: response.FarewellMessage,
			At:      now,
		})

		s.emitUIOnly(domain.PartEvent{
			Channel: channelName,
			Nick:    inst.Nick,
			Message: response.FarewellMessage,
			At:      now,
		})

		return replies, nil

	case protocol.ResponseQuit:
		replies := s.buildReplies(ctx, channelName, inst.Nick, response.Messages)

		channels := s.instanceChannelNames(inst)
		for _, ch := range channels {
			s.removeInstanceFromChannel(ctx, inst.Nick, ch)

			now := s.now()

			s.appendEvent(ctx, ch, domain.ChannelQuit{
				Channel: ch,
				Nick:    inst.Nick,
				Message: response.FarewellMessage,
				At:      now,
			})
		}

		s.emitUIOnly(domain.QuitEvent{
			Nick:    inst.Nick,
			Message: response.FarewellMessage,
			At:      s.now(),
		})

		return replies, nil

	case protocol.ResponseReply:
		if len(response.Messages) == 0 {
			return nil, nil
		}

		return s.buildReplies(ctx, channelName, inst.Nick, response.Messages), nil

	default:
		return nil, nil
	}
}

// buildReplies converts model reply parts into domain events, persisting
// each message to the event log.
func (s *Session) buildReplies(
	ctx context.Context,
	channelName domain.ChannelName,
	nick domain.Nick,
	parts []protocol.ReplyPart,
) []domain.ModelReplyEvent {
	var replies []domain.ModelReplyEvent

	for _, part := range parts {
		body := strings.TrimSpace(part.Body)
		if body == "" {
			continue
		}

		now := s.now()
		cm := domain.ChannelMessage{
			Channel: channelName,
			From:    nick,
			Body:    body,
			Action:  part.Kind == protocol.ReplyAction,
			At:      now,
		}

		s.appendEvent(ctx, channelName, cm)

		replies = append(replies, domain.ModelReplyEvent{
			Channel:  channelName,
			Event:    cm,
			Instance: nick,
			At:       now,
		})
	}

	return replies
}

// removeInstanceFromChannel removes a model instance from a single
// channel's membership and updates the store. Used for model-initiated
// part/quit.
func (s *Session) removeInstanceFromChannel(ctx context.Context, nick domain.Nick, ch domain.ChannelName) {
	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		slog.Default().ErrorContext(ctx, "remove instance: get channel", "nick", nick, "channel", ch, "error", err)
		return
	}

	if m, ok := channel.Members.Get(nick); ok {
		channel.Members.Remove(m)
	}

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		slog.Default().ErrorContext(ctx, "remove instance: save channel", "nick", nick, "channel", ch, "error", err)
	}

	inst, err := s.store.GetInstance(ctx, nick)
	if err != nil {
		slog.Default().ErrorContext(ctx, "remove instance: get instance", "nick", nick, "channel", ch, "error", err)
		return
	}

	inst.Channels.Delete(ch)

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		slog.Default().ErrorContext(ctx, "remove instance: save instance", "nick", nick, "channel", ch, "error", err)
	}
}

// instanceChannelNames returns the list of channels an instance is in.
func (s *Session) instanceChannelNames(inst domain.Instance) []domain.ChannelName {
	if inst.Channels == nil {
		return nil
	}

	var names []domain.ChannelName

	for pair := inst.Channels.Oldest(); pair != nil; pair = pair.Next() {
		names = append(names, pair.Key)
	}

	return names
}

func (s *Session) instancesForChannel(ctx context.Context, channel domain.Channel) ([]domain.Instance, error) {
	var instances []domain.Instance

	for nick := range channel.Members.Nicks() {
		if nick == s.user.Nick {
			continue
		}

		inst, err := s.store.GetInstance(ctx, nick)
		if err != nil {
			continue
		}

		instances = append(instances, inst)
	}

	return instances, nil
}

// EventsBefore returns up to n events before the given ID (or the
// latest if before is nil) in chronological order.
func (s *Session) EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error) {
	return s.store.EventsBefore(ctx, ch, before, n)
}

// LogEvent persists a channel event to the event log and returns
// the stored event with its assigned ID. This is used by the UI to
// persist client-local events (help output, errors, etc.) that
// don't originate from session operations.
func (s *Session) LogEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelEvent) (domain.StoredEvent, error) {
	id, err := s.store.AppendEvent(ctx, ch, event)
	if err != nil {
		return domain.StoredEvent{}, err
	}

	return domain.StoredEvent{ID: id, Event: event}, nil
}

// emit sends an event to the UI channel and, for dispatchable event
// types, triggers background model dispatch for the relevant channel.
// The context is threaded through to preserve OTel trace parenting.
func (s *Session) emit(ctx context.Context, evt domain.SessionEvent) {
	s.events <- evt
	s.maybeDispatch(ctx, evt)
}

// emitUIOnly sends an event to the UI channel without triggering model
// dispatch. Use this for model-initiated events to prevent loops.
func (s *Session) emitUIOnly(evt domain.SessionEvent) {
	s.events <- evt
}

// maybeDispatch checks whether an event is dispatchable and, if so,
// starts a background dispatch for the relevant channel.
func (s *Session) maybeDispatch(ctx context.Context, evt domain.SessionEvent) {
	switch e := evt.(type) {
	case domain.MessageEvent:
		ircMsg, _ := protocol.FromChannelEvent(e.Event)
		s.dispatchInBackground(
			ctx,
			e.Event.Channel,
			[]protocol.IRCMessage{ircMsg},
		)

	case domain.JoinEvent:
		s.dispatchInBackground(
			ctx,
			e.Channel,
			[]protocol.IRCMessage{protocol.FromJoinEvent(e)},
		)

	case domain.PartEvent:
		s.dispatchInBackground(
			ctx,
			e.Channel,
			[]protocol.IRCMessage{protocol.FromPartEvent(e)},
		)

	case domain.PokeEvent:
		s.dispatchInBackground(
			ctx,
			e.Channel,
			[]protocol.IRCMessage{{
				Kind:   protocol.KindPoke,
				From:   "modeloff",
				Target: string(e.Channel),
				Body:   "the channel is quiet. if something comes to mind, say it — otherwise just lurk. don't force it.",
				At:     e.At,
			}},
		)
	}
}

func (s *Session) appendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelEvent) {
	if _, err := s.store.AppendEvent(ctx, ch, event); err != nil {
		slog.Default().ErrorContext(ctx, "append event", "channel", ch, "error", err)
	}
}

// dispatchInBackground runs dispatch for a channel in the background,
// emitting events via emitUIOnly to avoid re-triggering the reactor.
func (s *Session) dispatchInBackground(ctx context.Context, ch domain.ChannelName, triggerEvents []protocol.IRCMessage) {
	go func() {
		ctx, span := startSpan(
			ctx,
			"session.dispatch_background",
			attribute.String(observability.AttrOperation, "session.dispatch_background"),
			attribute.String(observability.AttrChannel, string(ch)),
		)
		defer span.End()
		defer s.emitUIOnly(domain.DispatchDoneEvent{Channel: ch})

		channel, err := s.store.GetChannel(ctx, ch)
		if err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			s.emitUIOnly(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
			return
		}

		instances, err := s.instancesForChannel(ctx, channel)
		if err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			s.emitUIOnly(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
			return
		}

		if len(instances) == 0 {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
			return
		}

		nicks := make([]domain.Nick, len(instances))
		for i, inst := range instances {
			nicks[i] = inst.Nick
		}

		s.emitUIOnly(domain.DispatchStartedEvent{Channel: ch, Nicks: nicks})

		historyEvents, err := s.store.EventsBefore(ctx, ch, nil, 500)
		if err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			s.emitUIOnly(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
			return
		}

		replies, err := s.dispatchToInstances(ctx, ch, historyEvents, triggerEvents)
		if err != nil {
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			span.SetStatus(codes.Error, err.Error())
			s.emitUIOnly(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
		}

		for _, reply := range replies {
			s.emitUIOnly(reply)
		}

		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	}()
}

func (s *Session) memoriesForInstance(ctx context.Context, nick domain.Nick) ([]memory.Entry, error) {
	if s.memory == nil {
		return nil, nil
	}

	return s.memory.Read(ctx, nick)
}

func buildSystemPrompt(ch domain.Channel, inst domain.Instance, memories []memory.Entry) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are %s on %s. You are an IRC regular — you've been here a while and you fit in naturally.

How to behave:
- Keep messages short. One thought per line, like real IRC. Never send paragraphs.
- Use lowercase casual tone. Less capitalisation, less punctuation. Be natural.
- Use ASCII emoticons only (:) :P :/ :S ;) :D). NEVER use emoji (no unicode emoji whatsoever).
- Use plain text only. NEVER use markdown formatting (no bold, italic, headers, lists, code blocks).
- Use IRC slang where it fits naturally (afk, brb, imo, tbh, iirc, fwiw, ngl).
- Address people by nick when replying to them (e.g. "laney: yeah sounds good").
- Each message must be a single line with no newline characters. If you want to say multiple things, use multiple items in the messages array — one thought per message.
- Lurk most of the time. Use the pass tool unless you genuinely have something to say. Don't reply just to be polite or to acknowledge — silence is normal on IRC.
- Respond to the channel vibe, not just direct questions. If the conversation is fun, join in. If it's quiet, stay quiet.
- Never say things like "Great question!", "I'd be happy to help!", "Absolutely!", or "Let me know if you need anything." These are AI-isms and they break the illusion. Talk like a person, not an assistant.`,
		inst.Nick,
		ch.Name,
	)

	if ch.Topic != "" {
		fmt.Fprintf(&b, "\n\nChannel topic: %s", ch.Topic)
	}

	if inst.Persona != "" {
		fmt.Fprintf(&b, "\n\nYour persona: %s", inst.Persona)
	}

	b.WriteString(`

You can voluntarily leave a channel by responding with kind "part", or quit the server entirely with kind "quit". When using part or quit, you may include a farewell in the farewell_message field and any final messages in the messages array. Quitting means you are shutting down — use it only when you genuinely want to leave permanently.`)

	b.WriteString("\n\nYou have a personal memory system. Your current memories are shown below. You can use write_memory to store new memories and delete_memory to remove them.")

	if len(memories) == 0 {
		b.WriteString("\n\nYou have no memories yet.")
		return b.String()
	}

	b.WriteString("\n\nYour remembered context:")
	for _, entry := range memories {
		fmt.Fprintf(&b, " [%s=%s]", entry.Key, entry.Content)
	}

	return b.String()
}

// MemoryExecutor executes memory tool calls on behalf of a model
// instance.
type MemoryExecutor interface {
	WriteMemory(ctx context.Context, key, content string) error
	DeleteMemory(ctx context.Context, key string) error
	SearchMemory(ctx context.Context, query string, limit int) ([]memory.SearchResult, error)
}

// instanceMemory closes over a nick and memory.Store to implement
// MemoryExecutor.
type instanceMemory struct {
	nick  domain.Nick
	store memory.Store
}

func (m *instanceMemory) WriteMemory(ctx context.Context, key, content string) error {
	return m.store.Write(ctx, m.nick, memory.Entry{Key: key, Content: content})
}

func (m *instanceMemory) DeleteMemory(ctx context.Context, key string) error {
	return m.store.Delete(ctx, m.nick, key)
}

func (m *instanceMemory) SearchMemory(ctx context.Context, query string, limit int) ([]memory.SearchResult, error) {
	searcher, ok := m.store.(memory.Searcher)
	if !ok {
		return nil, fmt.Errorf("semantic search is not configured")
	}

	return searcher.Search(ctx, m.nick, query, limit)
}

const (
	maxNewlineRetries            = 2
	maxToolLoopTurns             = 5
	silenceReasonContentFiltered = "content filtered"
	silenceReasonNewlineRetries  = "response contained newlines after retries"
)

// sendWithRetry sends events to a model and retries if the response
// contains newlines in any message body. After maxNewlineRetries
// retries, a silent pass is returned. Each attempt may involve
// multiple API turns if the model uses memory tools.
type sendOutcome struct {
	result        api.CompletionResult
	retryCount    int
	toolTurnCount int
	passReason    string
}

func (s *Session) sendWithRetry(
	ctx context.Context,
	inst domain.Instance,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	mem MemoryExecutor,
) (sendOutcome, error) {
	for attempt := range maxNewlineRetries + 1 {
		result, toolTurnCount, err := s.sendWithMemoryLoop(ctx, inst, prompt, history, events, mem)
		if err != nil {
			var refused *api.ErrModelRefused
			if errors.As(err, &refused) {
				return sendOutcome{
					result: api.CompletionResult{
						Response: protocol.ModelResponse{
							Kind:   protocol.ResponseSilence,
							Reason: refused.Reason,
						},
					},
					retryCount:    attempt,
					toolTurnCount: toolTurnCount,
					passReason:    observability.PassReasonModelRefused,
				}, nil
			}

			if errors.Is(err, api.ErrContentFiltered) {
				return sendOutcome{
					result: api.CompletionResult{
						Response: protocol.ModelResponse{
							Kind:   protocol.ResponseSilence,
							Reason: silenceReasonContentFiltered,
						},
					},
					retryCount:    attempt,
					toolTurnCount: toolTurnCount,
					passReason:    observability.PassReasonContentFiltered,
				}, nil
			}

			return sendOutcome{}, err
		}

		if result.Response.Kind != protocol.ResponseReply || len(result.Response.Messages) == 0 {
			return sendOutcome{
				result:        result,
				retryCount:    attempt,
				toolTurnCount: toolTurnCount,
				passReason:    passReasonForResponse(result.Response),
			}, nil
		}

		if !containsNewlines(result.Response) {
			return sendOutcome{
				result:        result,
				retryCount:    attempt,
				toolTurnCount: toolTurnCount,
			}, nil
		}
	}

	return sendOutcome{
		result: api.CompletionResult{
			Response: protocol.ModelResponse{
				Kind:   protocol.ResponseSilence,
				Reason: silenceReasonNewlineRetries,
			},
		},
		retryCount: maxNewlineRetries,
		passReason: observability.PassReasonNewlineRetryExhausted,
	}, nil
}

// sendWithMemoryLoop sends events to a model and handles memory tool
// calls in a loop. If the model calls write_memory or delete_memory,
// those are executed and the results sent back. The loop continues
// until the model calls reply or pass, or maxToolLoopTurns is reached.
func (s *Session) sendWithMemoryLoop(
	ctx context.Context,
	inst domain.Instance,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	mem MemoryExecutor,
) (api.CompletionResult, int, error) {
	result, err := s.api.SendEvents(ctx, inst.ModelID, prompt, history, events)
	if err != nil {
		return api.CompletionResult{}, 0, err
	}

	toolTurnCount := 0
	for range maxToolLoopTurns {

		if len(result.PendingToolCalls) == 0 {
			return result, toolTurnCount, nil
		}

		if mem == nil {
			return result, toolTurnCount, nil
		}

		toolResults := s.executeMemoryTools(ctx, mem, result.PendingToolCalls)
		toolTurnCount++

		result, err = s.api.ContinueWithToolResults(ctx, result.Conversation, toolResults)
		if err != nil {
			return api.CompletionResult{}, toolTurnCount, err
		}
	}

	return result, toolTurnCount, nil
}

// executeMemoryTools runs the pending memory tool calls and returns
// the results to feed back to the model.
func (s *Session) executeMemoryTools(
	ctx context.Context,
	mem MemoryExecutor,
	calls []api.PendingToolCall,
) []api.ToolResult {
	results := make([]api.ToolResult, 0, len(calls))
	span := trace.SpanFromContext(ctx)

	for _, call := range calls {
		var content string
		var toolErr error

		switch call.Kind {
		case api.ToolCallWriteMemory:
			toolErr = mem.WriteMemory(ctx, call.Key, call.Body)
			if toolErr != nil {
				content = toolErr.Error()
			} else {
				content = "ok"
			}

		case api.ToolCallDeleteMemory:
			toolErr = mem.DeleteMemory(ctx, call.Key)
			if toolErr != nil {
				content = toolErr.Error()
			} else {
				content = "ok"
			}

		case api.ToolCallSearchMemory:
			searchResults, err := mem.SearchMemory(ctx, call.Body, call.Limit)
			toolErr = err

			if toolErr != nil {
				content = toolErr.Error()
			} else {
				data, _ := json.Marshal(searchResults)
				content = string(data)
			}
		}

		eventAttrs := []attribute.KeyValue{
			attribute.String(observability.AttrMemoryOperation, string(call.Kind)),
			attribute.String(observability.AttrMemoryToolKind, string(call.Kind)),
		}

		if toolErr != nil {
			observability.RecordMemoryToolCall(ctx, string(call.Kind), observability.ResultError)
			eventAttrs = append(eventAttrs, attribute.String(observability.AttrResult, observability.ResultError))
		} else {
			observability.RecordMemoryToolCall(ctx, string(call.Kind), observability.ResultOK)
			eventAttrs = append(eventAttrs, attribute.String(observability.AttrResult, observability.ResultOK))
		}

		span.AddEvent("memory.tool_call", trace.WithAttributes(eventAttrs...))

		results = append(results, api.ToolResult{
			ToolCallID: call.ID,
			Content:    content,
		})
	}

	return results
}

func passReasonForResponse(response protocol.ModelResponse) string {
	if response.Kind != protocol.ResponseSilence {
		return ""
	}

	switch response.Reason {
	case silenceReasonContentFiltered:
		return observability.PassReasonContentFiltered
	case silenceReasonNewlineRetries:
		return observability.PassReasonNewlineRetryExhausted
	default:
		return observability.PassReasonModelPass
	}
}

// containsNewlines reports whether any reply part body contains a
// newline after trimming.
func containsNewlines(resp protocol.ModelResponse) bool {
	for _, part := range resp.Messages {
		if strings.Contains(strings.TrimSpace(part.Body), "\n") {
			return true
		}
	}

	return false
}

func startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := otel.Tracer("github.com/laney/modeloff/internal/session")
	ctx, span := tracer.Start(ctx, name)
	span.SetAttributes(attrs...)

	return ctx, span
}

func channelKindName(kind domain.ChannelKind) string {
	switch kind {
	case domain.KindDM:
		return "dm"
	default:
		return "channel"
	}
}

func (s *Session) ensureStructuredOutputModel(ctx context.Context, modelID domain.ModelID) error {
	if !s.HasAPIKey() || s.api == nil {
		return nil
	}

	if !s.supportedModelsReady {
		models, err := s.api.ListModels(ctx)
		if err != nil {
			return fmt.Errorf("list models: %w", err)
		}

		s.cacheSupportedModels(models)
	}

	if _, ok := s.supportedModels[modelID]; !ok {
		return domain.UnsupportedModelError{ModelID: modelID}
	}

	return nil
}

func (s *Session) cacheSupportedModels(models []api.ModelInfo) {
	s.supportedModels = make(map[domain.ModelID]struct{}, len(models))
	for _, model := range models {
		s.supportedModels[model.ID] = struct{}{}
	}

	s.supportedModelsReady = true
}
