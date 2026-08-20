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
	"slices"
	"sync"
	"sync/atomic"
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

	// ClientCaps reports the capabilities granted to a registered
	// subscription. [UserClient.Caps] delegates to it, so the
	// command-visibility filter reads the modes the session holds.
	ClientCaps(id protocol.ClientID) command.CapabilityHolder
}

// Store is the persistence surface a [UserClient] needs. It is the
// subset of the session store's surface used by autojoin,
// mark-read and quit bookkeeping.
type Store interface {
	// Autojoin list. The channels this client rejoins on the next
	// connection. It is client state: the client rewrites it as its
	// own membership changes, and the session neither reads nor
	// writes it, the same arrangement `dm_windows` has.
	ListAutojoinChannels(ctx context.Context) ([]domain.ChannelName, error)
	SetAutojoinChannels(ctx context.Context, channels []domain.ChannelName) error

	// ClearSessionActive drops the marker that says a connection is
	// open. [UserClient.Quit] writes it, so the next start reads a
	// QUIT-terminated run as a clean one. The session sets the marker
	// during its connect handshake and reads it to classify the
	// previous run; ending it belongs to the client whose connection
	// it describes.
	ClearSessionActive(ctx context.Context) error

	// SaveInstance writes this client's connection record. It is
	// written at attach, under the empty [domain.InstanceID] the
	// user-client registers with, and from then on the session
	// maintains it exactly as it maintains a model's: a join stamps
	// the channel onto it, a NICK rewrites the nick, and the QUIT
	// teardown deletes it.
	SaveInstance(ctx context.Context, inst *domain.Instance) error

	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)

	// DMEventsBefore reads the DM thread between `self` and `peer`,
	// both directions together. Marking a DM read needs it: the two
	// directions are logged under their recipients, so the newest
	// event in the conversation is not always under the window's own
	// key.
	DMEventsBefore(ctx context.Context, self, peer domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error)

	SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error

	// SetDMLastRead is SetLastRead's DM counterpart: the cursor for a
	// DM thread lives in its own table, keyed by the counterpart's
	// InstanceID, since last_read.channel references channels(name)
	// and a DM window is never a row there.
	SetDMLastRead(ctx context.Context, peer domain.InstanceID, eventID int64) error

	// ListDMWindows returns the counterparts of the DM windows the
	// user has open. AddDMWindow and RemoveDMWindow are its writes.
	// The set is client-owned: the session never reads or writes it,
	// so the user-client is the only actor that touches these three.
	ListDMWindows(ctx context.Context) ([]domain.InstanceID, error)
	AddDMWindow(ctx context.Context, peer domain.InstanceID) error
	RemoveDMWindow(ctx context.Context, peer domain.InstanceID) error
}

// ReplyLog is the write handle the user-client uses to persist its
// own point-to-point replies (query responses and command errors)
// to the per-issuer reply log, keyed by the issuer's identity. It
// mirrors the model-client's direct, store-backed write: the
// user-client records its replies without routing them through the
// session.
type ReplyLog interface {
	Record(ctx context.Context, issuer domain.InstanceID, reply domain.IssuerReply) error
}

// appendInstanceReply is the store-side reply-log write the concrete
// `*store.SQLiteStore` provides. [NewStoreReplyLog] adapts it to
// [ReplyLog], discarding the returned row id.
type appendInstanceReply interface {
	AppendInstanceReply(ctx context.Context, id domain.InstanceID, reply domain.IssuerReply) (int64, error)
}

// NewStoreReplyLog adapts a store's instance-reply write to the
// [ReplyLog] the user-client holds.
func NewStoreReplyLog(store appendInstanceReply) ReplyLog {
	return storeReplyLog{store: store}
}

type storeReplyLog struct {
	store appendInstanceReply
}

func (l storeReplyLog) Record(ctx context.Context, issuer domain.InstanceID, reply domain.IssuerReply) error {
	_, err := l.store.AppendInstanceReply(ctx, issuer, reply)
	return err
}

// UserClient is the [protocol.Client] backing the human user. It
// holds the canonical `*domain.Instance` for the user and a
// subscription on the owning [Session]; the chat-screen reads
// identity through it and sends wire commands through it.
type UserClient struct {
	instance *domain.Instance
	sess     Session
	store    Store
	replyLog ReplyLog

	mu  sync.Mutex
	sub protocol.Subscription

	// restoring is set while [UserClient.JoinAutojoinChannels] is
	// replaying the autojoin list, so the JOINs it issues do not
	// rewrite the list they are reading. See
	// [UserClient.saveAutojoinList].
	restoring atomic.Bool
}

// New returns an unattached `UserClient` for `nick`. Call
// [UserClient.Attach] to register it with the session before any
// command flows through it.
func New(nick domain.Nick, sess Session, store Store, replyLog ReplyLog) *UserClient {
	return &UserClient{
		instance: domain.NewUserInstance(nick),
		sess:     sess,
		store:    store,
		replyLog: replyLog,
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
// client as the issuing actor, and keeps this client's own state in
// step with the commands it issues.
//
// Two pieces of state ride here. Every channel a JOIN reaches stamps
// its read cursor at the channel's head, and every command that can
// change which channels this client is in rewrites the autojoin
// list. Every way the user joins arrives here: [UserClient.Join] and
// so [UserClient.JoinAutojoinChannels], the chat-screen's window
// switch, and the `/join` slash command, which builds its own
// [protocol.Join] and dispatches it through this method. This is
// the one place that covers them all. Doing it per call site would
// leave whichever one was forgotten showing a channel as unread the
// moment you arrive in it.
func (uc *UserClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	resp, err := uc.sess.Handle(ctx, uc, cmd)
	if err != nil {
		return resp, err
	}

	if _, ok := cmd.(protocol.Join); ok {
		uc.markJoinedChannelsRead(ctx, resp.Events)
	}

	uc.saveAutojoinList(ctx, cmd)

	return resp, nil
}

// saveAutojoinList rewrites the autojoin list after a command that
// can change which channels this client is in. `Handle` has already
// returned, so the command loop has finished with the command and
// the client's channel set is the settled answer to what it is in.
//
// JOIN, PART and KICK are those commands. A KICK naming somebody
// else costs one write that changes nothing, which is cheaper than
// resolving the target here to find out. QUIT is deliberately not
// among them: it ends the connection without saying the channels
// should not come back, and this list is what brings them back.
//
// Best-effort, like the read cursor: the command itself succeeded,
// and a list that missed one update is repaired by the next one.
//
// [UserClient.JoinAutojoinChannels] suppresses the write for as long
// as it is replaying the list. It joins in chunks, so a write between
// two chunks would leave the stored list holding only the channels
// joined so far, and a process that died there would come back to a
// list missing the tail it never reached.
func (uc *UserClient) saveAutojoinList(ctx context.Context, cmd protocol.Command) {
	switch cmd.(type) {
	case protocol.Join, protocol.Part, protocol.Kick:
	default:
		return
	}

	if uc.restoring.Load() {
		return
	}

	uc.writeAutojoinList(ctx)
}

// writeAutojoinList persists the client's current channel set as the
// autojoin list. A failed write is logged and nothing else: the
// command that prompted it has already succeeded.
func (uc *UserClient) writeAutojoinList(ctx context.Context) {
	if err := uc.store.SetAutojoinChannels(ctx, uc.autojoinChannels()); err != nil {
		slog.Default().ErrorContext(ctx, "save autojoin list",
			"component", "userclient",
			"error", err,
		)
	}
}

// autojoinChannels is this client's current channel set restricted
// to real `#`-channels. The order is the order it joined them in,
// which the store does not preserve: it reads the list back sorted
// by name, and the next restore joins them in that order instead.
// The status window is the chat-screen's own view of the server and
// is never a membership; a DM window holds no shared state to rejoin
// and would reach `joinAs` under its `InstanceID`-shaped name.
func (uc *UserClient) autojoinChannels() []domain.ChannelName {
	channels := uc.instance.Channels()
	if channels == nil {
		return nil
	}

	var out []domain.ChannelName

	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		if domain.InferChannelKind(pair.Key) != domain.KindChannel {
			continue
		}

		out = append(out, pair.Key)
	}

	return out
}

// markJoinedChannelsRead stamps the read cursor for every channel a
// JOIN reached. [Session.handleJoin] knows exactly which channels it
// joined and puts a [domain.JoinedChannel] in `Response.Events` for
// each one; this stamps exactly that set and nothing else. A gate
// refusal, and a failure the dispatcher never turned into a typed
// event, both leave a channel simply absent from `events`, so
// neither ever gets a stamp. The derivation is fail-closed: it
// withholds a stamp by default, and grants one only for a channel
// `events` confirms joined.
func (uc *UserClient) markJoinedChannelsRead(ctx context.Context, events []protocol.Event) {
	for _, ev := range events {
		joined, ok := ev.(domain.JoinedChannel)
		if !ok {
			continue
		}

		uc.markJoinedChannelRead(ctx, joined.Channel)
	}
}

// markJoinedChannelRead stamps the read cursor for a channel this
// client has just joined. The name is normalised the way the
// dispatcher normalises it, so the cursor is keyed to the same
// channel the events were logged under.
//
// Best-effort: the join itself succeeded, and failing to move a
// cursor is not worth turning that into an error the user sees. The
// cost of a miss is a badge that over-counts until the channel is
// next read.
func (uc *UserClient) markJoinedChannelRead(ctx context.Context, ch domain.ChannelName) {
	name := domain.NormaliseChannelName(ch)

	if err := uc.MarkRead(ctx, name); err != nil {
		slog.Default().ErrorContext(ctx, "mark joined channel read",
			"component", "userclient",
			"channel", name,
			"error", err,
		)
	}
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

// Caps reports the capabilities the session has granted this
// client's subscription, for the chatcmd grammar's `caps:` filter.
// The answer is the session's, read afresh on each question: the
// `+o` [UserClient.Attach] requests is what puts the operator-gated
// commands in the palette, and a client that has not attached yet
// holds nothing.
func (uc *UserClient) Caps() command.CapabilityHolder {
	return protocol.LiveCaps(uc.sess, uc.Identity())
}

// Attach registers the user-client with its session, requesting
// `+o` (operator) as its initial mode. The session writes the
// granting [domain.UserModeChange] as the first event on the
// subscription's bus so consumers see the elevation before any
// other traffic.
//
// The connection record is written first. A model's is written by
// `ADDMODEL`'s registration step before its client connects, and
// this is the same step for a client that registers itself: from
// here on the session is the only writer of that row, and channel
// records loaded from the store resolve this client's member
// entries through it. The write is an upsert, so a database from
// before this row existed needs no migration and a repeat run
// overwrites the row it left.
//
// That write is also what registers this client's `*domain.Instance`
// as the canonical handle for its id, so Attach must run before
// anything loads a channel record: a member entry naming this client
// resolves through the registry, and one resolved before the write
// would either miss or bind to a different handle. `main.go` attaches
// as its first act on the session for that reason.
//
// Attach is idempotent: a repeat call on an already-attached
// client returns nil.
func (uc *UserClient) Attach(ctx context.Context) error {
	uc.mu.Lock()
	defer uc.mu.Unlock()

	if uc.sub != nil {
		return nil
	}

	if err := uc.store.SaveInstance(ctx, uc.instance); err != nil {
		return fmt.Errorf("register user client: %w", err)
	}

	sub, err := uc.sess.Subscribe(uc, protocol.SubscribeOptions{
		Instance:     uc.instance,
		InitialModes: []domain.Mode{domain.ModeOperator},
		EchoMessage:  true,
	})
	if err != nil {
		return fmt.Errorf("attach user client: %w", err)
	}

	uc.sub = sub

	return nil
}

// Subscription returns the registered subscription handle, or nil
// if the client has not been attached.
func (uc *UserClient) Subscription() protocol.Subscription {
	uc.mu.Lock()
	defer uc.mu.Unlock()

	return uc.sub
}

// Join issues a wire JOIN as the user-actor. The read cursor moves
// with it — see [UserClient.Send], which stamps it for every JOIN
// this client issues, however it was raised.
func (uc *UserClient) Join(ctx context.Context, ch domain.ChannelName) error {
	resp, err := uc.Send(ctx, protocol.Join{Channels: []domain.ChannelName{ch}})

	return firstErr(err, resp.Err)
}

// Part issues a wire PART as the user-actor.
func (uc *UserClient) Part(ctx context.Context, ch domain.ChannelName, reason string) error {
	resp, err := uc.Send(ctx, protocol.Part{Channel: ch, Reason: reason})
	return firstErr(err, resp.Err)
}

// SendMessage issues a wire PRIVMSG as the user-actor and returns
// the persisted [domain.Message] echoed in `Response.Events`. `ch` is
// the window the user is typing in, which
// [protocol.TargetForWindow] turns into the target the server
// resolves: a channel window addresses its channel, a DM window
// addresses its counterpart.
func (uc *UserClient) SendMessage(ctx context.Context, ch domain.ChannelName, body string) (domain.Message, error) {
	resp, err := uc.Send(ctx, protocol.PrivMsg{Target: protocol.TargetForWindow(ch), Body: body})
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

// SendAction issues a wire ACTION (`/me`) as the user-actor. It
// addresses `ch` exactly as [UserClient.SendMessage] does.
func (uc *UserClient) SendAction(ctx context.Context, ch domain.ChannelName, body string) (domain.Message, error) {
	resp, err := uc.Send(ctx, protocol.Action{Target: protocol.TargetForWindow(ch), Body: body})
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

// Quit issues a wire QUIT as the user-actor and records that this
// connection ended cleanly.
//
// The session-active marker says a connection is open. Clearing it
// once the QUIT has gone through is what makes the next start read
// this exit as a clean one, so the memberships the QUIT has just
// dropped are not reconciled a second time. A run that ends without
// a QUIT leaves the marker in place, which is the state the next
// connect classifies as unclean.
func (uc *UserClient) Quit(ctx context.Context, reason string) error {
	resp, err := uc.Send(ctx, protocol.Quit{Reason: reason})
	if err := firstErr(err, resp.Err); err != nil {
		return err
	}

	return uc.Disconnected(ctx)
}

// Disconnected records that this client's connection has ended. It
// is the bookkeeping half of [UserClient.Quit], and is called on its
// own when the server ended the connection without being asked. That
// is a KILL naming this client, which runs the same teardown a QUIT
// does and leaves the same nothing behind to reconcile.
func (uc *UserClient) Disconnected(ctx context.Context) error {
	if err := uc.store.ClearSessionActive(ctx); err != nil {
		return fmt.Errorf("clear session active: %w", err)
	}

	return nil
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
// store and issues it as multi-target JOINs (RFC 2812 §3.2.1),
// chunked to [protocol.MaxJoinTargets] channels per command so no
// single JOIN is refused for naming too many. Each chunk still
// charges the connection's flood-control penalty only once, so
// restoring dozens of channels on connect costs a handful of
// two-second charges, not one per channel.
// Best-effort: [Session.handleJoin] processes every channel in a
// chunk independently, so a gate refusal on one is logged without
// withholding the rest. Returns a non-nil error only if the
// autojoin list itself cannot be loaded.
//
// The list is rewritten once, after the last chunk, and the ordinary
// per-command write is suppressed until then. Writing between chunks
// would leave the stored list holding only what had been joined so
// far, and a process that ended there would come back to a list
// missing the tail it never reached.
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

	uc.restoring.Store(true)
	defer func() {
		uc.restoring.Store(false)
		uc.writeAutojoinList(ctx)
	}()

	channelNames := make([]string, len(channels))
	for i, ch := range channels {
		channelNames[i] = string(ch)
	}

	var failed int
	for chunk := range slices.Chunk(channels, protocol.MaxJoinTargets) {
		resp, sendErr := uc.Send(ctx, protocol.Join{Channels: chunk})
		if sendErr != nil {
			failed += len(chunk)
			slog.Default().ErrorContext(ctx, "autojoin channels",
				"component", "userclient",
				"channels", chunk,
				"error", sendErr,
			)
			continue
		}

		joined := 0
		for _, ev := range resp.Events {
			if _, ok := ev.(domain.JoinedChannel); ok {
				joined++
				continue
			}

			slog.Default().ErrorContext(ctx, "autojoin channel",
				"component", "userclient",
				"error", ev,
			)
		}

		failed += len(chunk) - joined
	}

	span.SetAttributes(
		attribute.Int(observability.AttrAutojoinCount, len(channels)),
		attribute.Int(observability.AttrAutojoinFailed, failed),
		attribute.StringSlice(observability.AttrAutojoinChannels, channelNames),
	)

	return nil
}

// MarkRead records the user's last-read position in `ch` at the id
// of the most recent event in the window. No-op when the window has
// no events.
func (uc *UserClient) MarkRead(ctx context.Context, ch domain.ChannelName) error {
	events, err := uc.latestEvent(ctx, ch)
	if err != nil {
		return fmt.Errorf("get latest event: %w", err)
	}

	if len(events) == 0 {
		return nil
	}

	if domain.InferChannelKind(ch) == domain.KindDM {
		return uc.store.SetDMLastRead(ctx, domain.InstanceID(ch), events[0].ID)
	}

	return uc.store.SetLastRead(ctx, ch, events[0].ID)
}

// latestEvent reads the most recent event of a window, as a slice
// that is empty when the window has none.
//
// A DM reads the whole thread. Each direction is logged under its
// recipient, so a cursor taken from the window's own key would stop
// at the last line the user sent and leave everything the counterpart
// said since counted as unread.
func (uc *UserClient) latestEvent(ctx context.Context, ch domain.ChannelName) ([]domain.StoredEvent, error) {
	if domain.InferChannelKind(ch) == domain.KindDM {
		return uc.store.DMEventsBefore(ctx, domain.InstanceID(uc.Identity()), domain.InstanceID(ch), nil, 1)
	}

	return uc.store.EventsBefore(ctx, ch, nil, 1)
}

// DMWindows returns the counterparts of the DM windows the user had
// open when the process last ran. A channel is rejoined on connect
// and announces itself with a JOIN, but a DM is not a membership the
// server holds: nothing on the wire would bring the window back, so
// the client keeps the list itself and the chat-screen reopens it at
// bootstrap.
func (uc *UserClient) DMWindows(ctx context.Context) ([]domain.InstanceID, error) {
	return uc.store.ListDMWindows(ctx)
}

// OpenDMWindow records that the user has a DM window open with
// `peer`, so a later run reopens it. Idempotent.
func (uc *UserClient) OpenDMWindow(ctx context.Context, peer domain.InstanceID) error {
	return uc.store.AddDMWindow(ctx, peer)
}

// CloseDMWindow drops `peer` from the set of open DM windows. The
// conversation itself survives: the event log keeps both directions
// of it, and messaging the counterpart again reopens the window with
// its history intact. Idempotent.
func (uc *UserClient) CloseDMWindow(ctx context.Context, peer domain.InstanceID) error {
	return uc.store.RemoveDMWindow(ctx, peer)
}

// RecordReply persists one of the user's own point-to-point replies
// to the per-issuer reply log, keyed by the user-client's identity
// (the empty [domain.InstanceID] by convention). The user is
// transient and never restores, so this is the durable record a
// future restore or inspector reads; the user does not see it again
// this session. Best-effort: a failed write is logged, since the
// reply was already rendered live.
func (uc *UserClient) RecordReply(ctx context.Context, reply domain.IssuerReply) {
	id := domain.InstanceID(uc.Identity())
	if err := uc.replyLog.Record(ctx, id, reply); err != nil {
		slog.Default().ErrorContext(ctx, "record user reply",
			"component", "userclient",
			"error", err,
		)
	}
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
