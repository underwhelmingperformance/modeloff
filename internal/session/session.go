// Package session provides the backend coordinator that ties together
// stores, the API client, and the protocol layer. It manages channels,
// model instances, and handles commands by updating state and emitting
// domain events.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/store"
)

const eventBufSize = 64

// Session is the backend coordinator. It bridges the UI layer and
// the underlying stores and API client.
type Session struct {
	store  store.Store
	memory memory.Store
	api    api.Client
	config config.Store

	userNick  domain.Nick
	apiKey    string
	nickModel domain.ModelID
	factory   func(string) (api.Client, error)
	now       func() time.Time
	events    chan domain.SessionEvent
}

// New creates a Session with the given dependencies.
func New(
	s store.Store,
	m memory.Store,
	a api.Client,
	c config.Store,
	userNick domain.Nick,
) *Session {
	sess := &Session{
		store:    s,
		memory:   m,
		api:      a,
		config:   c,
		userNick: userNick,
		now:      time.Now,
		events:   make(chan domain.SessionEvent, eventBufSize),
	}

	if c != nil {
		cfg, err := c.Load()
		if err == nil {
			sess.apiKey = strings.TrimSpace(cfg.APIKey)
			sess.nickModel = cfg.NickModel
		}
	}

	if sess.nickModel == "" {
		sess.nickModel = config.DefaultNickModel
	}

	return sess
}

// Events returns the channel on which background dispatch events are
// emitted. The caller should drain this channel to receive
// DispatchStartedEvent, ModelReplyEvent, DispatchDoneEvent, and
// ErrorEvent values.
func (s *Session) Events() <-chan domain.SessionEvent {
	return s.events
}

// SetAPIFactory configures how runtime API clients are created.
func (s *Session) SetAPIFactory(factory func(string) (api.Client, error)) {
	s.factory = factory
}

// HasAPIKey reports whether the session has an active API key.
func (s *Session) HasAPIKey() bool {
	return strings.TrimSpace(s.apiKey) != ""
}

// UserNick returns the current user nickname.
func (s *Session) UserNick() domain.Nick {
	return s.userNick
}

// Join creates or opens a channel and returns a JoinEvent.
func (s *Session) Join(ctx context.Context, channelName string) (domain.JoinEvent, error) {
	name := domain.ChannelName(channelName)

	var created bool

	_, err := s.store.GetChannel(ctx, name)
	if err != nil {
		created = true

		ch := domain.Channel{
			Name:    name,
			Kind:    domain.KindChannel,
			Members: set.NewOrdered(s.userNick),
			Created: s.now(),
		}

		if err := s.store.SaveChannel(ctx, ch); err != nil {
			return domain.JoinEvent{}, fmt.Errorf("save channel: %w", err)
		}
	}

	if err := s.store.SetLastChannel(ctx, name); err != nil {
		return domain.JoinEvent{}, fmt.Errorf("set last channel: %w", err)
	}

	if err := s.MarkRead(ctx, name); err != nil {
		return domain.JoinEvent{}, fmt.Errorf("mark read: %w", err)
	}

	return domain.JoinEvent{
		Channel: name,
		Nick:    s.userNick,
		Created: created,
		At:      s.now(),
	}, nil
}

// Leave records the user leaving a channel and returns a PartEvent.
func (s *Session) Leave(ctx context.Context, ch domain.ChannelName) (domain.PartEvent, error) {
	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.PartEvent{}, fmt.Errorf("channel not found: %w", err)
	}

	channel.Members.Remove(s.userNick)

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return domain.PartEvent{}, fmt.Errorf("save channel: %w", err)
	}

	return domain.PartEvent{
		Channel: ch,
		Nick:    s.userNick,
		At:      s.now(),
	}, nil
}

// ListChannels returns all persisted channels.
func (s *Session) ListChannels(ctx context.Context) ([]domain.Channel, error) {
	return s.store.ListChannels(ctx)
}

// ListInstances returns all persisted model instances.
func (s *Session) ListInstances(ctx context.Context) ([]domain.ModelInstance, error) {
	return s.store.ListInstances(ctx)
}

// Invite adds a model instance to a channel. If the model has no nick
// yet, one is generated via the API.
func (s *Session) Invite(
	ctx context.Context,
	ch domain.ChannelName,
	modelID domain.ModelID,
	persona string,
) (domain.ModelInvitedEvent, error) {
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

	nickResult, err := s.api.GenerateNick(generateCtx, s.nickModel, modelID)
	if err != nil {
		generateSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		generateSpan.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "generate nick failed", "error", err)
		return domain.ModelInvitedEvent{}, fmt.Errorf("generate nick: %w", err)
	}

	nickResult.Usage.SetSpanAttributes(generateSpan, nickResult.RequestID)
	generateSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	inst := domain.ModelInstance{
		Nick:     nickResult.Nick,
		ModelID:  modelID,
		Persona:  strings.TrimSpace(persona),
		Channels: set.NewOrdered(ch),
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return s.attachInstanceToChannel(ctx, ch, inst)
}

func (s *Session) findInstanceByModelID(ctx context.Context, modelID domain.ModelID) (domain.ModelInstance, error) {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return domain.ModelInstance{}, err
	}

	for _, inst := range instances {
		if inst.ModelID == modelID {
			return inst, nil
		}
	}

	return domain.ModelInstance{}, fmt.Errorf("no instance with model ID %s", modelID)
}

func (s *Session) attachInstanceToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	inst domain.ModelInstance,
) (domain.ModelInvitedEvent, error) {
	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("get channel: %w", err)
	}

	inst.Channels.Add(ch)

	if inst.JoinedAt == nil {
		inst.JoinedAt = make(map[domain.ChannelName]time.Time)
	}

	if _, ok := inst.JoinedAt[ch]; !ok {
		inst.JoinedAt[ch] = s.now()
	}

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("save instance: %w", err)
	}

	channel.Members.Add(inst.Nick)

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("save channel: %w", err)
	}

	return domain.ModelInvitedEvent{
		Channel:  ch,
		Instance: inst,
		At:       s.now(),
	}, nil
}

// Kick removes a model instance from a channel.
func (s *Session) Kick(
	ctx context.Context,
	ch domain.ChannelName,
	nick domain.Nick,
) (domain.ModelKickedEvent, error) {
	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.ModelKickedEvent{}, fmt.Errorf("get channel: %w", err)
	}

	channel.Members.Remove(nick)

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return domain.ModelKickedEvent{}, fmt.Errorf("save channel: %w", err)
	}

	inst, err := s.store.GetInstance(ctx, nick)
	if err == nil {
		inst.Channels.Remove(ch)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return domain.ModelKickedEvent{}, fmt.Errorf("save instance: %w", err)
		}
	}

	return domain.ModelKickedEvent{
		Channel: ch,
		Nick:    nick,
		At:      s.now(),
	}, nil
}

// SendMessage saves a message to a channel and returns the message
// event. It also spawns a background goroutine to dispatch the
// message to model instances, emitting events on the Events channel.
func (s *Session) SendMessage(
	ctx context.Context,
	ch domain.ChannelName,
	body string,
) (domain.MessageEvent, error) {
	ctx, span := startSpan(ctx, "session.send_message", attribute.String(observability.AttrOperation, "session.send_message"))
	defer span.End()

	msg := domain.Message{
		ID:      fmt.Sprintf("%d", s.now().UnixNano()),
		Channel: ch,
		From:    s.userNick,
		Body:    body,
		SentAt:  s.now(),
	}

	if err := s.store.SaveMessage(ctx, msg); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return domain.MessageEvent{}, fmt.Errorf("save message: %w", err)
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	s.dispatchInBackground(ctx, ch, []protocol.IRCMessage{protocol.FromMessage(msg)})

	return domain.MessageEvent{Message: msg}, nil
}

// SendAction saves an action message (/me) to a channel and returns
// the message event. It also spawns a background goroutine to
// dispatch the action to model instances.
func (s *Session) SendAction(
	ctx context.Context,
	ch domain.ChannelName,
	body string,
) (domain.MessageEvent, error) {
	msg := domain.Message{
		ID:      fmt.Sprintf("%d", s.now().UnixNano()),
		Channel: ch,
		From:    s.userNick,
		Body:    body,
		Action:  true,
		SentAt:  s.now(),
	}

	if err := s.store.SaveMessage(ctx, msg); err != nil {
		return domain.MessageEvent{}, fmt.Errorf("save action: %w", err)
	}

	s.dispatchInBackground(ctx, ch, []protocol.IRCMessage{protocol.FromMessage(msg)})

	return domain.MessageEvent{Message: msg}, nil
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

	historyMessages, err := s.store.ListMessages(ctx, ch)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("list history: %w", err)
	}

	replies, err := s.dispatchToInstances(ctx, ch, historyMessages, newEvents)
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
) (domain.TopicChangeEvent, error) {
	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.TopicChangeEvent{}, fmt.Errorf("get channel: %w", err)
	}

	channel.Topic = topic
	channel.TopicSetBy = s.userNick
	channel.TopicSetAt = s.now()

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return domain.TopicChangeEvent{}, fmt.Errorf("save channel: %w", err)
	}

	return domain.TopicChangeEvent{
		Channel: ch,
		Topic:   topic,
		By:      s.userNick,
		At:      s.now(),
	}, nil
}

// ChangeNick changes the user's nickname and persists it through the
// config store.
func (s *Session) ChangeNick(
	_ context.Context,
	newNick domain.Nick,
) (domain.NickChangeEvent, error) {
	if s.config == nil {
		return domain.NickChangeEvent{}, fmt.Errorf("config store not configured")
	}

	cfg, err := s.config.Load()
	if err != nil {
		return domain.NickChangeEvent{}, fmt.Errorf("load config: %w", err)
	}

	cfg.UserNick = string(newNick)

	if err := s.config.Save(cfg); err != nil {
		return domain.NickChangeEvent{}, fmt.Errorf("save config: %w", err)
	}

	evt := domain.NickChangeEvent{
		OldNick: s.userNick,
		NewNick: newNick,
		At:      s.now(),
	}

	s.userNick = newNick

	return evt, nil
}

// Whois returns metadata about a model instance.
func (s *Session) Whois(ctx context.Context, nick domain.Nick) (domain.ModelInstance, error) {
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

// Messages returns all messages for a channel.
func (s *Session) Messages(ctx context.Context, ch domain.ChannelName) ([]domain.Message, error) {
	return s.store.ListMessages(ctx, ch)
}

// MarkRead records that the user has seen all current messages in a
// channel by storing the ID of the last message.
func (s *Session) MarkRead(ctx context.Context, ch domain.ChannelName) error {
	msgs, err := s.store.ListMessages(ctx, ch)
	if err != nil {
		return fmt.Errorf("list messages: %w", err)
	}

	if len(msgs) == 0 {
		return nil
	}

	last := msgs[len(msgs)-1]

	return s.store.SetLastRead(ctx, ch, last.ID)
}

// UnreadCount returns the number of messages in a channel that arrived
// after the last-read position. If nothing has been read yet, all
// messages are considered unread.
func (s *Session) UnreadCount(ctx context.Context, ch domain.ChannelName) (int, error) {
	msgs, err := s.store.ListMessages(ctx, ch)
	if err != nil {
		return 0, fmt.Errorf("list messages: %w", err)
	}

	lastRead, err := s.store.GetLastRead(ctx, ch)
	if err != nil {
		return 0, fmt.Errorf("get last read: %w", err)
	}

	if lastRead == "" {
		return len(msgs), nil
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ID == lastRead {
			return len(msgs) - 1 - i, nil
		}
	}

	return len(msgs), nil
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

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return models, nil
}

// Reset clears all channels, messages, model instances, and memories,
// returning the application to a fresh state. Config is preserved.
func (s *Session) Reset(ctx context.Context) error {
	if err := s.store.Reset(ctx); err != nil {
		return fmt.Errorf("reset store: %w", err)
	}

	if s.memory != nil {
		if err := s.memory.Reset(ctx); err != nil {
			return fmt.Errorf("reset memories: %w", err)
		}
	}

	return nil
}

// OpenDM opens or creates a direct-message conversation with a known
// model instance and makes it the active conversation.
func (s *Session) OpenDM(ctx context.Context, nick domain.Nick) (domain.Channel, bool, error) {
	inst, err := s.store.GetInstance(ctx, nick)
	if err != nil {
		return domain.Channel{}, false, fmt.Errorf("get instance: %w", err)
	}

	name := domain.ChannelName(nick)

	ch, err := s.store.GetChannel(ctx, name)
	created := false
	if err != nil {
		ch = domain.Channel{
			Name:    name,
			Kind:    domain.KindDM,
			Members: set.NewOrdered(s.userNick, nick),
			Created: s.now(),
		}

		if err := s.store.SaveChannel(ctx, ch); err != nil {
			return domain.Channel{}, false, fmt.Errorf("save dm channel: %w", err)
		}

		created = true
	}

	if inst.Channels.Add(name) {
		if inst.JoinedAt == nil {
			inst.JoinedAt = make(map[domain.ChannelName]time.Time)
		}

		if _, ok := inst.JoinedAt[name]; !ok {
			inst.JoinedAt[name] = s.now()
		}

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return domain.Channel{}, false, fmt.Errorf("save instance: %w", err)
		}
	}

	if err := s.store.SetLastChannel(ctx, name); err != nil {
		return domain.Channel{}, false, fmt.Errorf("set last channel: %w", err)
	}

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

	for _, ch := range channels {
		pokeEvent := protocol.IRCMessage{
			Kind:   protocol.KindPoke,
			From:   "modeloff",
			Target: string(ch.Name),
			Body:   "the channel is quiet. if something comes to mind, say it — otherwise just lurk. don't force it.",
			At:     s.now(),
		}

		s.dispatchInBackground(ctx, ch.Name, []protocol.IRCMessage{pokeEvent})
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	logger.DebugContext(ctx, "scheduled poke dispatch", "channels", len(channels))

	return nil
}

// SetAPIKey persists a new API key through the config store.
func (s *Session) SetAPIKey(_ context.Context, apiKey string) (config.Config, error) {
	if s.config == nil {
		return config.Config{}, fmt.Errorf("config store not configured")
	}

	cfg, err := s.config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	apiKey = strings.TrimSpace(apiKey)

	var nextClient api.Client
	if apiKey != "" {
		if s.factory != nil {
			client, err := s.factory(apiKey)
			if err != nil {
				return config.Config{}, fmt.Errorf("build api client: %w", err)
			}

			nextClient = client
		} else {
			nextClient = s.api
		}
	}

	cfg.APIKey = apiKey

	if err := s.config.Save(cfg); err != nil {
		return config.Config{}, fmt.Errorf("save config: %w", err)
	}

	s.api = nextClient
	s.apiKey = apiKey

	return cfg, nil
}

// SetPokeInterval persists a new poke interval through the config
// store.
func (s *Session) SetPokeInterval(_ context.Context, interval time.Duration) (config.Config, error) {
	if s.config == nil {
		return config.Config{}, fmt.Errorf("config store not configured")
	}

	cfg, err := s.config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	cfg.PokeInterval = interval

	if err := s.config.Save(cfg); err != nil {
		return config.Config{}, fmt.Errorf("save config: %w", err)
	}

	return cfg, nil
}

// SetNickModel persists a new nick generation model through the
// config store.
func (s *Session) SetNickModel(_ context.Context, modelID domain.ModelID) (config.Config, error) {
	if s.config == nil {
		return config.Config{}, fmt.Errorf("config store not configured")
	}

	cfg, err := s.config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	cfg.NickModel = modelID

	if err := s.config.Save(cfg); err != nil {
		return config.Config{}, fmt.Errorf("save config: %w", err)
	}

	s.nickModel = modelID

	return cfg, nil
}

// HighlightWords returns the currently configured highlight words.
func (s *Session) HighlightWords() []string {
	if s.config == nil {
		return config.DefaultHighlightWords
	}

	cfg, err := s.config.Load()
	if err != nil {
		return config.DefaultHighlightWords
	}

	return cfg.HighlightWords
}

// SetHighlightWords persists a new set of highlight words through
// the config store.
func (s *Session) SetHighlightWords(_ context.Context, words []string) (config.Config, error) {
	if s.config == nil {
		return config.Config{}, fmt.Errorf("config store not configured")
	}

	cfg, err := s.config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	cfg.HighlightWords = words

	if err := s.config.Save(cfg); err != nil {
		return config.Config{}, fmt.Errorf("save config: %w", err)
	}

	return cfg, nil
}

func (s *Session) dispatchToInstances(
	ctx context.Context,
	channelName domain.ChannelName,
	historyMessages []domain.Message,
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
		instReplies, instErr := s.dispatchToInstance(ctx, channel, inst, channelName, historyMessages, events)
		if instErr != nil {
			errs = append(errs, instErr)
		}

		replies = append(replies, instReplies...)

		for _, r := range instReplies {
			events = append(events, protocol.FromMessage(r.Message))
		}
	}

	return replies, errors.Join(errs...)
}

func (s *Session) dispatchToInstance(
	ctx context.Context,
	channel domain.Channel,
	inst domain.ModelInstance,
	channelName domain.ChannelName,
	historyMessages []domain.Message,
	events []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	_, instanceSpan := startSpan(
		ctx,
		"session.dispatch_to_instance",
		attribute.String(observability.AttrOperation, "session.dispatch_to_instance"),
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrChannelKind, channelKindName(channel.Kind)),
	)
	defer instanceSpan.End()

	joinedAt := inst.JoinedAt[channelName]

	history := make([]protocol.IRCMessage, 0, len(historyMessages))
	for _, msg := range historyMessages {
		if !joinedAt.IsZero() && msg.SentAt.Before(joinedAt) {
			continue
		}

		history = append(history, protocol.FromMessage(msg))
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

	result, err := s.sendWithRetry(ctx, inst, prompt, history, events, mem)
	if err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("send events to %s: %w", inst.Nick, err)
	}

	result.Usage.SetSpanAttributes(instanceSpan, result.RequestID)
	instanceSpan.SetAttributes(attribute.String(observability.AttrResult, api.ResponseResultKind(result.Response)))

	response := result.Response
	if response.Kind != protocol.ResponseReply || len(response.Messages) == 0 {
		return nil, nil
	}

	var replies []domain.ModelReplyEvent

	for i, part := range response.Messages {
		body := strings.TrimSpace(part.Body)
		if body == "" {
			continue
		}

		reply := domain.Message{
			ID:      fmt.Sprintf("%d~%s~%d", s.now().UnixNano(), inst.Nick, i),
			Channel: channelName,
			From:    inst.Nick,
			Body:    body,
			Action:  part.Kind == protocol.ReplyAction,
			SentAt:  s.now(),
		}

		if err := s.store.SaveMessage(ctx, reply); err != nil {
			instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			instanceSpan.SetStatus(codes.Error, err.Error())
			return replies, fmt.Errorf("save model reply: %w", err)
		}

		replies = append(replies, domain.ModelReplyEvent{
			Channel:  channelName,
			Message:  reply,
			Instance: inst.Nick,
			At:       s.now(),
		})
	}

	return replies, nil
}

func (s *Session) instancesForChannel(ctx context.Context, channel domain.Channel) ([]domain.ModelInstance, error) {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}

	indexed := make(map[domain.Nick]domain.ModelInstance, len(instances))
	for _, inst := range instances {
		indexed[inst.Nick] = inst
	}

	filtered := make([]domain.ModelInstance, 0, len(channel.Members))
	for nick := range channel.Members.Sorted() {
		if nick == s.userNick {
			continue
		}

		inst, ok := indexed[nick]
		if !ok {
			continue
		}

		filtered = append(filtered, inst)
	}

	return filtered, nil
}

func (s *Session) emit(evt domain.SessionEvent) {
	s.events <- evt
}

// dispatchInBackground runs dispatch for a channel in the background,
// emitting events to the events channel.
func (s *Session) dispatchInBackground(ctx context.Context, ch domain.ChannelName, triggerEvents []protocol.IRCMessage) {
	go func() {
		defer s.emit(domain.DispatchDoneEvent{Channel: ch})

		channel, err := s.store.GetChannel(ctx, ch)
		if err != nil {
			s.emit(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
			return
		}

		instances, err := s.instancesForChannel(ctx, channel)
		if err != nil {
			s.emit(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
			return
		}

		if len(instances) == 0 {
			return
		}

		nicks := make([]domain.Nick, len(instances))
		for i, inst := range instances {
			nicks[i] = inst.Nick
		}

		s.emit(domain.DispatchStartedEvent{Channel: ch, Nicks: nicks})

		historyMessages, err := s.store.ListMessages(ctx, ch)
		if err != nil {
			s.emit(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
			return
		}

		replies, err := s.dispatchToInstances(ctx, ch, historyMessages, triggerEvents)
		if err != nil {
			s.emit(domain.ErrorEvent{Operation: "dispatch", Err: err, At: s.now()})
		}

		for _, reply := range replies {
			s.emit(reply)
		}
	}()
}

func (s *Session) memoriesForInstance(ctx context.Context, nick domain.Nick) ([]memory.Entry, error) {
	if s.memory == nil {
		return nil, nil
	}

	return s.memory.Read(ctx, nick)
}

func buildSystemPrompt(ch domain.Channel, inst domain.ModelInstance, memories []memory.Entry) string {
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

const (
	maxNewlineRetries = 2
	maxToolLoopTurns  = 5
)

// sendWithRetry sends events to a model and retries if the response
// contains newlines in any message body. After maxNewlineRetries
// retries, a silent pass is returned. Each attempt may involve
// multiple API turns if the model uses memory tools.
func (s *Session) sendWithRetry(
	ctx context.Context,
	inst domain.ModelInstance,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	mem MemoryExecutor,
) (api.CompletionResult, error) {
	for attempt := range maxNewlineRetries + 1 {
		_ = attempt

		result, err := s.sendWithMemoryLoop(ctx, inst, prompt, history, events, mem)
		if err != nil {
			return api.CompletionResult{}, err
		}

		if result.Response.Kind != protocol.ResponseReply || len(result.Response.Messages) == 0 {
			return result, nil
		}

		if !containsNewlines(result.Response) {
			return result, nil
		}
	}

	return api.CompletionResult{
		Response: protocol.ModelResponse{
			Kind:   protocol.ResponseSilence,
			Reason: "response contained newlines after retries",
		},
	}, nil
}

// sendWithMemoryLoop sends events to a model and handles memory tool
// calls in a loop. If the model calls write_memory or delete_memory,
// those are executed and the results sent back. The loop continues
// until the model calls reply or pass, or maxToolLoopTurns is reached.
func (s *Session) sendWithMemoryLoop(
	ctx context.Context,
	inst domain.ModelInstance,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	mem MemoryExecutor,
) (api.CompletionResult, error) {
	result, err := s.api.SendEvents(ctx, inst.ModelID, prompt, history, events)
	if err != nil {
		return api.CompletionResult{}, err
	}

	for turn := range maxToolLoopTurns {
		_ = turn

		if len(result.PendingToolCalls) == 0 {
			return result, nil
		}

		if mem == nil {
			return result, nil
		}

		toolResults := s.executeMemoryTools(ctx, mem, result.PendingToolCalls)

		result, err = s.api.ContinueWithToolResults(ctx, result.Conversation, toolResults)
		if err != nil {
			return api.CompletionResult{}, err
		}
	}

	return result, nil
}

// executeMemoryTools runs the pending memory tool calls and returns
// the results to feed back to the model.
func (s *Session) executeMemoryTools(
	ctx context.Context,
	mem MemoryExecutor,
	calls []api.PendingToolCall,
) []api.ToolResult {
	results := make([]api.ToolResult, 0, len(calls))

	for _, call := range calls {
		var err error

		switch call.Kind {
		case api.ToolCallWriteMemory:
			err = mem.WriteMemory(ctx, call.Key, call.Body)
		case api.ToolCallDeleteMemory:
			err = mem.DeleteMemory(ctx, call.Key)
		}

		content := "ok"
		if err != nil {
			content = err.Error()
		}

		results = append(results, api.ToolResult{
			ToolCallID: call.ID,
			Content:    content,
		})
	}

	return results
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
