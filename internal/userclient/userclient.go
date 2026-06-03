// Package userclient holds the user-client implementation of
// [protocol.Client]. A [UserClient] represents the human user
// participating in the session: it owns the user's
// `*domain.Instance`, attaches to the session via
// [session.Session.Subscribe], holds the resulting
// [protocol.Subscription], and exposes user-actor convenience
// methods the chat-screen calls into when the user types a
// slash-command or chat line.
//
// The user-client's lifetime equals the session's. It is
// constructed in `cmd/modeloff` (or in a test fixture) and
// attached straight away with `+o` (operator). There is no
// detach: the session-shutdown path is the only way the
// subscription is released, via [session.Session.Shutdown]
// closing the registration gate.
package userclient

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// Session is the dependency surface a [UserClient] needs from the
// session. The concrete `*session.Session` satisfies it implicitly.
type Session interface {
	// Subscribe registers the client with the session and returns
	// the per-client delivery handle.
	Subscribe(c protocol.Client, opts protocol.SubscribeOptions) (protocol.Subscription, error)

	// Handle is the wire dispatcher's entry point.
	Handle(ctx context.Context, c protocol.Client, cmd protocol.Command) (protocol.Response, error)

	// GetWindow retrieves an addressable window by name.
	GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error)

	// PokeNow asks the session to run an immediate poke pass over
	// every channel. The session owns the poke schedule and the bus
	// emission; the user-client only relays a manual request.
	PokeNow(ctx context.Context) error
}

// Store is the persistence surface a [UserClient] needs. It is the
// subset of the session store's surface used by autojoin,
// mark-read and quit bookkeeping.
type Store interface {
	ListAutojoinChannels(ctx context.Context) ([]domain.ChannelName, error)
	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)
	SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error
}

// UserClient is the [protocol.Client] backing the human user. It
// holds the canonical `*domain.Instance` for the user and a
// subscription on the owning [Session]; the chat-screen reads
// identity through it and sends wire commands through it.
type UserClient struct {
	instance *domain.Instance
	sess     Session
	store    Store

	mu  sync.Mutex
	sub protocol.Subscription
}

// New returns an unattached `UserClient` for `nick`. Call
// [UserClient.Attach] to register it with the session before any
// command flows through it.
func New(nick domain.Nick, sess Session, store Store) *UserClient {
	return &UserClient{
		instance: domain.NewUserInstance(nick),
		sess:     sess,
		store:    store,
	}
}

// Instance returns the canonical user `*domain.Instance`. Identity
// checks against this pointer are how callers recognise user-origin
// events; the handle is stable for the process lifetime, with
// in-place nick renames via [domain.Instance.SetNick].
func (uc *UserClient) Instance() *domain.Instance { return uc.instance }

// Nick is shorthand for `uc.Instance().Nick()`.
func (uc *UserClient) Nick() domain.Nick { return uc.instance.Nick() }

// Identity reports the sentinel [protocol.UserClientID].
func (uc *UserClient) Identity() protocol.ClientID { return protocol.UserClientID }

// Send routes `cmd` through the session's dispatcher with this
// client as the issuing actor.
func (uc *UserClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return uc.sess.Handle(ctx, uc, cmd)
}

// Events returns the per-subscription delivery stream, or nil if
// the client has not been attached.
func (uc *UserClient) Events() <-chan protocol.Delivery {
	uc.mu.Lock()
	defer uc.mu.Unlock()

	if uc.sub == nil {
		return nil
	}

	return uc.sub.Events()
}

// Caps exposes the user-client's capabilities for the chatcmd
// grammar's `caps:` filter. The operator bit is held for the
// session's lifetime, so the visibility filter sees the full
// command and tool set.
func (uc *UserClient) Caps() command.CapabilityHolder { return userCaps{} }

// Attach registers the user-client with its session, requesting
// `+o` (operator) as its initial mode. The session writes the
// granting [domain.ModeChange] as the first event on the
// subscription's bus so consumers see the elevation before any
// other traffic.
//
// Attach is idempotent: a repeat call on an already-attached
// client returns nil.
func (uc *UserClient) Attach(ctx context.Context) error {
	uc.mu.Lock()
	defer uc.mu.Unlock()

	if uc.sub != nil {
		return nil
	}

	sub, err := uc.sess.Subscribe(uc, protocol.SubscribeOptions{
		Instance:     uc.instance,
		InitialModes: []domain.Mode{domain.ModeOperator},
	})
	if err != nil {
		return fmt.Errorf("attach user client: %w", err)
	}

	uc.sub = sub
	_ = ctx

	return nil
}

// Subscription returns the registered subscription handle, or nil
// if the client has not been attached.
func (uc *UserClient) Subscription() protocol.Subscription {
	uc.mu.Lock()
	defer uc.mu.Unlock()

	return uc.sub
}

// Join issues a wire JOIN as the user-actor.
func (uc *UserClient) Join(ctx context.Context, ch domain.ChannelName) error {
	resp, err := uc.Send(ctx, protocol.Join{Channel: ch})
	return firstErr(err, resp.Err)
}

// Part issues a wire PART as the user-actor.
func (uc *UserClient) Part(ctx context.Context, ch domain.ChannelName, reason string) error {
	resp, err := uc.Send(ctx, protocol.Part{Channel: ch, Reason: reason})
	return firstErr(err, resp.Err)
}

// SendMessage issues a wire PRIVMSG as the user-actor and returns
// the persisted [domain.Message] echoed in `Response.Events`.
func (uc *UserClient) SendMessage(ctx context.Context, ch domain.ChannelName, body string) (domain.Message, error) {
	resp, err := uc.Send(ctx, protocol.PrivMsg{Target: ch, Body: body})
	if err != nil {
		return domain.Message{}, err
	}
	if resp.Err != nil {
		return domain.Message{}, resp.Err
	}

	for _, e := range resp.Events {
		if msg, ok := e.(domain.Message); ok {
			return msg, nil
		}
	}

	return domain.Message{}, nil
}

// SendAction issues a wire ACTION (`/me`) as the user-actor.
func (uc *UserClient) SendAction(ctx context.Context, ch domain.ChannelName, body string) (domain.Message, error) {
	resp, err := uc.Send(ctx, protocol.Action{Target: ch, Body: body})
	if err != nil {
		return domain.Message{}, err
	}
	if resp.Err != nil {
		return domain.Message{}, resp.Err
	}

	for _, e := range resp.Events {
		if msg, ok := e.(domain.Message); ok {
			return msg, nil
		}
	}

	return domain.Message{}, nil
}

// SetTopic issues a wire TOPIC as the user-actor.
func (uc *UserClient) SetTopic(ctx context.Context, ch domain.ChannelName, topic string) error {
	resp, err := uc.Send(ctx, protocol.Topic{Channel: ch, Body: topic})
	return firstErr(err, resp.Err)
}

// ChangeNick issues a wire NICK as the user-actor.
func (uc *UserClient) ChangeNick(ctx context.Context, newNick domain.Nick) error {
	resp, err := uc.Send(ctx, protocol.Nick{New: newNick})
	return firstErr(err, resp.Err)
}

// Quit issues a wire QUIT as the user-actor.
func (uc *UserClient) Quit(ctx context.Context, reason string) error {
	resp, err := uc.Send(ctx, protocol.Quit{Reason: reason})
	return firstErr(err, resp.Err)
}

// Channels returns the user's current channel set. Returns nil
// when the user has joined no channels.
func (uc *UserClient) Channels() *orderedmap.OrderedMap[domain.ChannelName, time.Time] {
	return uc.instance.Channels()
}

// JoinedAt returns the time the user joined the given channel, or
// the zero time when the user is not in the channel.
func (uc *UserClient) JoinedAt(ch domain.ChannelName) time.Time {
	channels := uc.Channels()
	if channels == nil {
		return time.Time{}
	}

	t, ok := channels.Get(ch)
	if !ok {
		return time.Time{}
	}

	return t
}

// JoinAutojoinChannels loads the autojoin channel list from the
// store and issues an ordinary JOIN for each entry. Best-effort:
// per-channel join failures are logged but do not abort the
// iteration. Returns a non-nil error only if the autojoin list
// itself cannot be loaded.
func (uc *UserClient) JoinAutojoinChannels(ctx context.Context) (retErr error) {
	tracer := otel.GetTracerProvider().Tracer("github.com/laney/modeloff/internal/userclient")
	ctx, span := tracer.Start(ctx, "userclient.autojoin",
		trace.WithAttributes(attribute.String(observability.AttrOperation, "userclient.autojoin")),
	)
	defer func() {
		if retErr != nil {
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.End()
	}()

	channels, err := uc.store.ListAutojoinChannels(ctx)
	if err != nil {
		return fmt.Errorf("list autojoin channels: %w", err)
	}

	channelNames := make([]string, len(channels))
	for i, ch := range channels {
		channelNames[i] = string(ch)
	}

	var failed int
	for _, ch := range channels {
		if err := uc.Join(ctx, ch); err != nil {
			failed++
			slog.Default().ErrorContext(ctx, "autojoin channel",
				"component", "userclient",
				"channel", ch,
				"error", err,
			)
		}
	}

	span.SetAttributes(
		attribute.Int(observability.AttrAutojoinCount, len(channels)),
		attribute.Int(observability.AttrAutojoinFailed, failed),
		attribute.StringSlice(observability.AttrAutojoinChannels, channelNames),
	)

	return nil
}

// MarkRead records the user's last-read position in `ch` at the
// id of the most recent event in the channel. No-op when the
// channel has no events.
func (uc *UserClient) MarkRead(ctx context.Context, ch domain.ChannelName) error {
	events, err := uc.store.EventsBefore(ctx, ch, nil, 1)
	if err != nil {
		return fmt.Errorf("get latest event: %w", err)
	}

	if len(events) == 0 {
		return nil
	}

	return uc.store.SetLastRead(ctx, ch, events[0].ID)
}

// Poke asks the session to run an immediate poke pass over every
// channel. The session owns the schedule and the bus emission.
// Models subscribed to a channel use the poke as a cue to take a
// dispatch turn even when there has been no recent traffic.
func (uc *UserClient) Poke(ctx context.Context) error {
	return uc.sess.PokeNow(ctx)
}

// firstErr returns the first non-nil error of `transport` and
// `cmd`. The transport error wins by convention: a non-nil
// dispatcher return indicates a wiring fault, while a non-nil
// `Response.Err` is a typed command refusal.
func firstErr(transport, cmd error) error {
	if transport != nil {
		return transport
	}
	return cmd
}

// userCaps is the capability holder returned by [UserClient.Caps].
// The user-client carries the operator bit for the session's
// lifetime, so the holder reports `true` for [protocol.CapOperator]
// unconditionally.
type userCaps struct{}

func (userCaps) Has(c command.Capability) bool {
	return c == protocol.CapOperator
}
