// Package session provides the backend coordinator that ties together
// stores, the API client, and the protocol layer. It manages channels,
// model instances, and handles commands by updating state and emitting
// domain events.
package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/store"
)

// Session is the backend coordinator. It bridges the UI layer and
// the underlying stores and API client.
type Session struct {
	store  store.Store
	memory memory.Store
	api    api.Client
	config config.Store

	userNick domain.Nick
	now      func() time.Time
}

// New creates a Session with the given dependencies.
func New(
	s store.Store,
	m memory.Store,
	a api.Client,
	c config.Store,
	userNick domain.Nick,
) *Session {
	return &Session{
		store:    s,
		memory:   m,
		api:      a,
		config:   c,
		userNick: userNick,
		now:      time.Now,
	}
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

// Invite adds a model instance to a channel. If the model has no nick
// yet, one is generated via the API.
func (s *Session) Invite(
	ctx context.Context,
	ch domain.ChannelName,
	modelID domain.ModelID,
	persona string,
) (domain.ModelInvitedEvent, error) {
	if inst, err := s.store.GetInstance(ctx, domain.Nick(modelID)); err == nil {
		if inst.Persona == "" && strings.TrimSpace(persona) != "" {
			inst.Persona = strings.TrimSpace(persona)
		}

		return s.attachInstanceToChannel(ctx, ch, inst)
	}

	nick, err := s.api.GenerateNick(ctx, modelID)
	if err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("generate nick: %w", err)
	}

	inst := domain.ModelInstance{
		Nick:     nick,
		ModelID:  modelID,
		Persona:  strings.TrimSpace(persona),
		Channels: set.NewOrdered(ch),
	}

	return s.attachInstanceToChannel(ctx, ch, inst)
}

func (s *Session) attachInstanceToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	inst domain.ModelInstance,
) (domain.ModelInvitedEvent, error) {
	inst.Channels.Add(ch)

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("save instance: %w", err)
	}

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("get channel: %w", err)
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
// event.
func (s *Session) SendMessage(
	ctx context.Context,
	ch domain.ChannelName,
	body string,
) (domain.MessageEvent, error) {
	historyMessages, err := s.store.ListMessages(ctx, ch)
	if err != nil {
		return domain.MessageEvent{}, fmt.Errorf("list history: %w", err)
	}

	msg := domain.Message{
		ID:      fmt.Sprintf("%d", s.now().UnixNano()),
		Channel: ch,
		From:    s.userNick,
		Body:    body,
		SentAt:  s.now(),
	}

	if err := s.store.SaveMessage(ctx, msg); err != nil {
		return domain.MessageEvent{}, fmt.Errorf("save message: %w", err)
	}

	if err := s.dispatchToInstances(
		ctx,
		msg.Channel,
		historyMessages,
		[]protocol.IRCMessage{protocol.FromMessage(msg)},
	); err != nil {
		return domain.MessageEvent{}, err
	}

	return domain.MessageEvent{Message: msg}, nil
}

// SetTitle sets the title of a channel.
func (s *Session) SetTitle(
	ctx context.Context,
	ch domain.ChannelName,
	title string,
) (domain.TopicChangeEvent, error) {
	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.TopicChangeEvent{}, fmt.Errorf("get channel: %w", err)
	}

	channel.Title = title

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return domain.TopicChangeEvent{}, fmt.Errorf("save channel: %w", err)
	}

	return domain.TopicChangeEvent{
		Channel: ch,
		Title:   title,
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
		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return domain.Channel{}, false, fmt.Errorf("save instance: %w", err)
		}
	}

	if err := s.store.SetLastChannel(ctx, name); err != nil {
		return domain.Channel{}, false, fmt.Errorf("set last channel: %w", err)
	}

	return ch, created, nil
}

// Poke sends a periodic prompt to model instances in every channel and
// persists any replies they choose to make.
func (s *Session) Poke(ctx context.Context) error {
	channels, err := s.store.ListChannels(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}

	for _, ch := range channels {
		historyMessages, err := s.store.ListMessages(ctx, ch.Name)
		if err != nil {
			return fmt.Errorf("list history for %s: %w", ch.Name, err)
		}

		events := []protocol.IRCMessage{
			{
				Kind:   protocol.KindPoke,
				From:   "modeloff",
				Target: string(ch.Name),
				At:     s.now(),
			},
		}

		if err := s.dispatchToInstances(ctx, ch.Name, historyMessages, events); err != nil {
			return err
		}
	}

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

	cfg.APIKey = apiKey

	if err := s.config.Save(cfg); err != nil {
		return config.Config{}, fmt.Errorf("save config: %w", err)
	}

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

func (s *Session) dispatchToInstances(
	ctx context.Context,
	channelName domain.ChannelName,
	historyMessages []domain.Message,
	events []protocol.IRCMessage,
) error {
	channel, err := s.store.GetChannel(ctx, channelName)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	instances, err := s.instancesForChannel(ctx, channel)
	if err != nil {
		return fmt.Errorf("list instances for channel: %w", err)
	}

	history := make([]protocol.IRCMessage, 0, len(historyMessages))
	for _, historyMessage := range historyMessages {
		history = append(history, protocol.FromMessage(historyMessage))
	}

	for _, inst := range instances {
		memories, err := s.memoriesForInstance(ctx, inst.Nick)
		if err != nil {
			return fmt.Errorf("read memories for %s: %w", inst.Nick, err)
		}

		response, err := s.api.SendEvents(
			ctx,
			inst.ModelID,
			buildSystemPrompt(channel, inst, memories),
			history,
			events,
		)
		if err != nil {
			return fmt.Errorf("send events to %s: %w", inst.Nick, err)
		}

		if response.Kind != protocol.ResponseReply {
			continue
		}

		body := strings.TrimSpace(response.Body)
		if body == "" {
			continue
		}

		reply := domain.Message{
			ID:      fmt.Sprintf("%d~%s", s.now().UnixNano(), inst.Nick),
			Channel: channelName,
			From:    inst.Nick,
			Body:    body,
			SentAt:  s.now(),
		}

		if err := s.store.SaveMessage(ctx, reply); err != nil {
			return fmt.Errorf("save model reply: %w", err)
		}
	}

	return nil
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
	for nick := range channel.Members.Except(set.NewOrdered(s.userNick)) {
		inst, ok := indexed[nick]
		if !ok {
			continue
		}

		filtered = append(filtered, inst)
	}

	return filtered, nil
}

func (s *Session) memoriesForInstance(ctx context.Context, nick domain.Nick) ([]memory.Entry, error) {
	if s.memory == nil {
		return nil, nil
	}

	return s.memory.Read(ctx, nick)
}

func buildSystemPrompt(ch domain.Channel, inst domain.ModelInstance, memories []memory.Entry) string {
	prompt := fmt.Sprintf(
		"You are %s, a participant in an IRC-style chat on %s. Reply only when you have something useful to add.",
		inst.Nick,
		ch.Name,
	)

	if ch.Title != "" {
		prompt = fmt.Sprintf("%s The channel title is %q.", prompt, ch.Title)
	}

	if inst.Persona != "" {
		prompt = fmt.Sprintf("%s Your persona is %q.", prompt, inst.Persona)
	}

	if len(memories) == 0 {
		return prompt
	}

	prompt += " Your remembered context is:"
	for _, entry := range memories {
		prompt = fmt.Sprintf("%s [%s=%s]", prompt, entry.Key, entry.Content)
	}

	return prompt
}
