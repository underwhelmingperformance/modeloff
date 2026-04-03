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
			Members: []domain.Nick{s.userNick},
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
	_, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.PartEvent{}, fmt.Errorf("channel not found: %w", err)
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
) (domain.ModelInvitedEvent, error) {
	nick, err := s.api.GenerateNick(ctx, modelID)
	if err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("generate nick: %w", err)
	}

	inst := domain.ModelInstance{
		Nick:     nick,
		ModelID:  modelID,
		Channels: []domain.ChannelName{ch},
	}

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("save instance: %w", err)
	}

	channel, err := s.store.GetChannel(ctx, ch)
	if err != nil {
		return domain.ModelInvitedEvent{}, fmt.Errorf("get channel: %w", err)
	}

	channel.Members = append(channel.Members, nick)

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

	filtered := make([]domain.Nick, 0, len(channel.Members))
	for _, m := range channel.Members {
		if m != nick {
			filtered = append(filtered, m)
		}
	}

	channel.Members = filtered

	if err := s.store.SaveChannel(ctx, channel); err != nil {
		return domain.ModelKickedEvent{}, fmt.Errorf("save channel: %w", err)
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

	if err := s.broadcastMessage(ctx, msg, historyMessages); err != nil {
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

// ChangeNick changes the user's nickname.
func (s *Session) ChangeNick(newNick domain.Nick) domain.NickChangeEvent {
	evt := domain.NickChangeEvent{
		OldNick: s.userNick,
		NewNick: newNick,
		At:      s.now(),
	}

	s.userNick = newNick

	return evt
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

func (s *Session) broadcastMessage(
	ctx context.Context,
	msg domain.Message,
	historyMessages []domain.Message,
) error {
	channel, err := s.store.GetChannel(ctx, msg.Channel)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	instances, err := s.instancesForChannel(ctx, msg.Channel)
	if err != nil {
		return fmt.Errorf("list instances for channel: %w", err)
	}

	history := make([]protocol.IRCMessage, 0, len(historyMessages))
	for _, historyMessage := range historyMessages {
		history = append(history, protocol.FromMessage(historyMessage))
	}

	events := []protocol.IRCMessage{protocol.FromMessage(msg)}

	for _, inst := range instances {
		response, err := s.api.SendEvents(
			ctx,
			inst.ModelID,
			buildSystemPrompt(channel, inst),
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
			Channel: msg.Channel,
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

func (s *Session) instancesForChannel(ctx context.Context, ch domain.ChannelName) ([]domain.ModelInstance, error) {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return nil, err
	}

	filtered := make([]domain.ModelInstance, 0, len(instances))
	for _, inst := range instances {
		for _, instanceChannel := range inst.Channels {
			if instanceChannel == ch {
				filtered = append(filtered, inst)
				break
			}
		}
	}

	return filtered, nil
}

func buildSystemPrompt(ch domain.Channel, inst domain.ModelInstance) string {
	prompt := fmt.Sprintf(
		"You are %s, a participant in an IRC-style chat on %s. Reply only when you have something useful to add.",
		inst.Nick,
		ch.Name,
	)

	if ch.Title == "" {
		return prompt
	}

	return fmt.Sprintf("%s The channel title is %q.", prompt, ch.Title)
}
