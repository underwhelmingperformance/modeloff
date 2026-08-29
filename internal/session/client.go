package session

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// serverClient is the session-side concrete implementation of
// [protocol.Client]. One instance exists per subscription. The
// user-client's subscription survives QUIT, while its connection
// generation does not. The struct keeps a back-reference
// to its owning session so `Send` can route through [Session.Handle].
//
// The mode set is guarded by `modesMu`: `HasMode` and `Has` take
// the read lock, `setMode` takes the write lock. The `instance`
// pointer is set at construction and is the canonical actor handle
// the dispatcher reads via [Session.resolveClientActor] — no
// per-command store lookup.
//
// No producer ever waits on a consumer. Producers — the command
// loop, the poke scheduler, a model-client's own emissions — hand a
// delivery to `enqueue` and return; when the client is behind, the
// delivery lands in `outbox` and `pump` is what waits on `events`.
// That is what keeps a client that has stopped reading from
// stalling the server.
type serverClient struct {
	sess *Session
	id   protocol.ClientID

	// owner is the client this subscription was allocated for.
	// [Session.ensureSubscription] compares against it so the
	// envelope is never handed to a second client: `events` has one
	// reader, and two would take deliveries from each other.
	owner protocol.Client

	instance *domain.Instance
	events   chan protocol.Delivery

	connectionMu         sync.RWMutex
	connectionActive     bool
	connectionGeneration uint64

	// done closes exactly once when the subscription is reaped,
	// from any source: client-initiated via [serverClient.Unsubscribe],
	// session-initiated via QUIT / KILL through [Session.reapClient],
	// or shutdown. `unsubOnce` serialises the close so the channel
	// is never closed twice and never written to. Consumers that
	// long-poll on `Events` select on `done` to exit cleanly.
	done      chan struct{}
	unsubOnce sync.Once

	// outbox is this subscription's send queue, guarded by `outMu`.
	// A backlog survives a consumer that has fallen behind, up to
	// `sendQAllowance`; past that the server disconnects the client,
	// which is where the buffering stops. What the queue still holds
	// is released when the subscription is reaped or the session
	// shuts down.
	//
	// `outWake` carries a single coalesced wake-up: producers offer
	// one after appending and the pump drains the queue dry on each
	// wake, so a burst of appends costs at most one signal.
	outMu       sync.Mutex
	outbox      []queuedDelivery
	outWake     chan struct{}
	outClosed   bool
	outSealed   bool
	terminating bool
	outDraining bool
	outDrained  chan struct{}
	drainOnce   sync.Once

	// replayMu orders projected row writes and attach-time snapshots.
	// It stays separate from outMu so PART and KICK can advance a
	// membership epoch while a database read is in progress.
	replayMu sync.Mutex

	// turnGeneration revokes guards that could otherwise admit a new
	// journal after instance deletion. replayMu guards it and orders
	// journal admission against that deletion.
	turnGeneration uint64

	// replayBlocked keeps attach-time traffic in outbox until the
	// client has loaded its channel scrollback. replayRanges records
	// the projected rows included for each window, so Activate can
	// remove only queued deliveries that the snapshot covered.
	replayBlocked bool
	replayCapable bool
	replayRanges  map[domain.ChannelName]replayRange
	dmLiveFrom    map[domain.ChannelName]int64
	windowEpochs  map[domain.ChannelName]uint64

	// disconnecting latches the first terminal delivery failure so
	// send-Q overflow and projected-history failure can start only one
	// server-side disconnect between them.
	disconnecting atomic.Bool

	// pumpDone closes when this subscription's pump goroutine
	// exits, so [Session.reapClient] and [Session.Shutdown] can join
	// it before returning.
	pumpDone chan struct{}

	// penalty is this connection's RFC 1459 §8.10 message timer.
	// [Session.throttleCommand] charges every command the client
	// sends to it, whatever kind of actor is behind the connection.
	penalty penaltyTimer

	modesMu sync.RWMutex
	modes   map[domain.Mode]struct{}

	// echo grants IRCv3 echo-message: the client's own chat traffic
	// is delivered back to it (see [Session.fanOutProtocol]). Set once
	// at subscribe time from [protocol.SubscribeOptions.EchoMessage].
	echo bool
}

// newServerClient constructs a subscription with the given identity
// and actor instance, and starts its outbound pump. Modes start
// empty — the user-client is promoted via [Session.New]'s bootstrap
// call to `setUserModeAs`; future model elevation flows through
// [protocol.Oper] via the dispatcher.
//
// `stop` ends the pump for a subscription that is never reaped: the
// user-client lives for the session, so its pump exits on the
// session's shutdown gate.
type queuedDelivery struct {
	delivery      protocol.Delivery
	scrollbackIDs map[domain.ChannelName]int64
	eventID       int64
	terminal      bool
}

type replayRange struct {
	first int64
	last  int64
}

type windowGuard struct {
	client           *serverClient
	clientGeneration uint64
	turnGeneration   uint64
	target           protocol.WindowTarget
	window           domain.ChannelName
	epoch            uint64
	dmPeer           *serverClient
	dmPeerGeneration uint64
	dmTurnGeneration uint64
}

type invitationGuard struct {
	client           *serverClient
	clientGeneration uint64
	turnGeneration   uint64
	channel          domain.ChannelName
	generation       invitationGeneration
	windowEpoch      uint64
}

// guardedCommandContextKey carries a window guard from
// [windowGuard.Send] to [Session.Handle]. [protocol.Client.Send]
// takes a command and nothing else, so the context is what crosses
// that boundary. [Session.commandWindowGuard] is the only read:
// from there the guard is an argument to the dispatch it authorises,
// so no work the dispatch goes on to start inherits it.
type guardedCommandContextKey struct{}

// commandWindowGuard returns the window authority attached to one
// command, or nil for a command sent outside a model turn.
func commandWindowGuard(ctx context.Context) protocol.WindowGuard {
	guard, _ := ctx.Value(guardedCommandContextKey{}).(protocol.WindowGuard)

	return guard
}

func (g invitationGuard) Valid(ctx context.Context) bool {
	unlock := lockTurnClients(g.client)
	defer unlock()

	return g.validLocked(ctx)
}

func (g invitationGuard) RunWithAuthority(ctx context.Context, operation func() error) error {
	unlock := lockTurnClients(g.client)
	defer unlock()

	if !g.validLocked(ctx) {
		return protocol.ErrWindowAuthorityChanged
	}

	return operation()
}

func (g invitationGuard) Send(
	ctx context.Context,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	return sendGuardedCommand(ctx, g.client, g, client, cmd)
}

func (g invitationGuard) validLocked(ctx context.Context) bool {
	if !g.client.connectionValid(g.clientGeneration) ||
		g.client.turnGeneration != g.turnGeneration {
		return false
	}

	windowEpoch := g.client.windowEpoch(g.channel)
	_, authority, _ := g.client.sess.invitationGuardState(
		ctx, g, windowEpoch,
	)

	return authority != invitationAuthorityNone
}

func (g invitationGuard) Context(ctx context.Context) (protocol.WindowContext, error) {
	unlock := lockTurnClients(g.client)
	defer unlock()

	if !g.client.connectionValid(g.clientGeneration) ||
		g.client.turnGeneration != g.turnGeneration {
		return nil, fmt.Errorf("guard invitation for %q: %w", g.channel, protocol.ErrSubscriptionClosed)
	}

	windowEpoch := g.client.windowEpoch(g.channel)
	window, authority, _ := g.client.sess.invitationGuardState(
		ctx, g, windowEpoch,
	)
	if !g.client.connectionValid(g.clientGeneration) {
		return nil, fmt.Errorf("guard invitation for %q: %w", g.channel, protocol.ErrSubscriptionClosed)
	}
	if authority == invitationAuthorityNone {
		return nil, domain.NotOnChannelError{
			Channel: g.channel,
			Command: "INVITE",
			At:      g.client.sess.now(),
		}
	}
	if authority == invitationAuthorityMember {
		return projectedChannelWindowContext(window), nil
	}

	return channelWindowContext{name: window.Name()}, nil
}

func (g windowGuard) Valid(ctx context.Context) bool {
	unlock := lockTurnClients(g.client, g.dmPeer)
	defer unlock()

	return g.validLocked(ctx)
}

func (g windowGuard) RunWithAuthority(ctx context.Context, operation func() error) error {
	unlock := lockTurnClients(g.client, g.dmPeer)
	defer unlock()

	if !g.validLocked(ctx) {
		return protocol.ErrWindowAuthorityChanged
	}

	return operation()
}

func (g windowGuard) Send(
	ctx context.Context,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	return sendGuardedCommand(ctx, g.client, g, client, cmd)
}

// sendGuardedCommand dispatches one command under `guard`, which
// [Session.Handle] rechecks on the command loop immediately before
// the handler runs. The command goes out through the client so its
// own reply handling still runs; the guard rides the context because
// [protocol.Client.Send] carries a command and nothing else.
func sendGuardedCommand(
	ctx context.Context,
	owner *serverClient,
	guard protocol.WindowGuard,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	if client == nil || client.Identity() != owner.Identity() {
		return protocol.Response{}, protocol.ErrWindowAuthorityChanged
	}

	ctx = context.WithValue(ctx, guardedCommandContextKey{}, guard)

	return client.Send(ctx, cmd)
}

func (g windowGuard) validLocked(ctx context.Context) bool {
	if !g.client.connectionValid(g.clientGeneration) ||
		g.client.turnGeneration != g.turnGeneration {
		return false
	}
	if _, direct := protocol.DirectWindowPeer(g.target); direct {
		return g.dmPeer == g.client ||
			(g.dmPeer.connectionValid(g.dmPeerGeneration) &&
				g.dmPeer.turnGeneration == g.dmTurnGeneration)
	}

	_, valid := g.channelWindowLocked(ctx)

	return valid
}

func (g windowGuard) channelWindow(ctx context.Context) (*domain.ChannelWindow, bool) {
	unlock := lockTurnClients(g.client)
	defer unlock()

	return g.channelWindowLocked(ctx)
}

func (g windowGuard) channelWindowLocked(ctx context.Context) (*domain.ChannelWindow, bool) {
	if !g.client.connectionValid(g.clientGeneration) ||
		g.client.turnGeneration != g.turnGeneration ||
		g.client.windowEpoch(g.window) != g.epoch {
		return nil, false
	}

	channel, err := g.client.sess.loadChannelWindow(ctx, g.window)
	if err != nil || !channel.Members.HasInstance(g.client.instance) ||
		!g.client.instance.InChannel(channel.Name()) ||
		g.client.windowEpoch(channel.Name()) != g.epoch {
		return nil, false
	}

	return channel, true
}

func (g windowGuard) Context(ctx context.Context) (protocol.WindowContext, error) {
	if !g.client.connectionValid(g.clientGeneration) {
		return nil, fmt.Errorf("guard window %q: %w", g.window, protocol.ErrSubscriptionClosed)
	}

	if _, direct := protocol.DirectWindowPeer(g.target); direct {
		if !g.Valid(ctx) {
			if !g.client.connectionValid(g.clientGeneration) {
				return nil, fmt.Errorf("guard window %q: %w", g.window, protocol.ErrSubscriptionClosed)
			}
			return nil, domain.NotOnChannelError{
				Channel: g.window,
				Command: "WINDOW",
				At:      g.client.sess.now(),
			}
		}

		return directWindowContext{peer: g.dmPeer.instance.ID()}, nil
	}

	window, valid := g.channelWindow(ctx)
	if !valid {
		if !g.client.connectionValid(g.clientGeneration) {
			return nil, fmt.Errorf("guard window %q: %w", g.window, protocol.ErrSubscriptionClosed)
		}
		return nil, domain.NotOnChannelError{
			Channel: g.window,
			Command: "WINDOW",
			At:      g.client.sess.now(),
		}
	}

	if !g.client.connectionValid(g.clientGeneration) {
		return nil, fmt.Errorf("guard window %q: %w", g.window, protocol.ErrSubscriptionClosed)
	}

	return projectedChannelWindowContext(window), nil
}

func projectedChannelWindowContext(window *domain.ChannelWindow) channelWindowContext {
	members := window.Members
	if window.Modes.Anonymous {
		members = domain.AnonymousMembers()
	}

	state := protocol.ChannelState{Modes: window.Modes}
	for member := range members.All() {
		state.Members = append(state.Members, protocol.ChannelMemberState{
			Nick: member.Nick, Modes: member.Modes,
		})
	}

	context := channelWindowContext{name: window.Name(), state: &state}
	if window.Topic != "" {
		setter := window.TopicSetBy
		if window.Modes.Anonymous && setter != "" {
			setter = domain.AnonymousNick
		}

		context.topic = &domain.TopicInfo{
			Target:     window.Name(),
			Topic:      window.Topic,
			TopicSetBy: setter,
			TopicSetAt: window.TopicSetAt,
			At:         window.TopicSetAt,
		}
	}

	return context
}

type channelWindowContext struct {
	name  domain.ChannelName
	state *protocol.ChannelState
	topic *domain.TopicInfo
}

func (c channelWindowContext) Target() protocol.WindowTarget {
	return protocol.ChannelWindowTarget(c.name)
}
func (c channelWindowContext) ChannelState() (protocol.ChannelState, bool) {
	if c.state == nil {
		return protocol.ChannelState{}, false
	}

	state := *c.state
	state.Members = slices.Clone(state.Members)

	return state, true
}
func (c channelWindowContext) Topic() (domain.TopicInfo, bool) {
	if c.topic == nil {
		return domain.TopicInfo{}, false
	}

	return *c.topic, true
}

type directWindowContext struct {
	peer domain.InstanceID
}

func (c directWindowContext) Target() protocol.WindowTarget {
	return protocol.DirectWindowTarget(c.peer)
}
func (directWindowContext) ChannelState() (protocol.ChannelState, bool) {
	return protocol.ChannelState{}, false
}
func (directWindowContext) Topic() (domain.TopicInfo, bool) {
	return domain.TopicInfo{}, false
}

func newServerClient(
	sess *Session,
	owner protocol.Client,
	inst *domain.Instance,
	opts protocol.SubscribeOptions,
	stop <-chan struct{},
) *serverClient {
	c := &serverClient{
		sess:                 sess,
		id:                   owner.Identity(),
		owner:                owner,
		instance:             inst,
		events:               make(chan protocol.Delivery, eventBufSize),
		connectionActive:     true,
		connectionGeneration: 1,
		done:                 make(chan struct{}),
		outWake:              make(chan struct{}, 1),
		outDrained:           make(chan struct{}),
		pumpDone:             make(chan struct{}),
		modes:                make(map[domain.Mode]struct{}),
		echo:                 opts.EchoMessage,
		replayBlocked:        opts.ReplayHistory,
		replayCapable:        opts.ReplayHistory,
		replayRanges:         make(map[domain.ChannelName]replayRange),
		dmLiveFrom:           make(map[domain.ChannelName]int64),
		windowEpochs:         make(map[domain.ChannelName]uint64),
	}

	go c.pump(stop)

	return c
}

func (c *serverClient) connection() (uint64, bool) {
	c.connectionMu.RLock()
	defer c.connectionMu.RUnlock()

	return c.connectionGeneration, c.connectionActive
}

func (c *serverClient) connectionValid(generation uint64) bool {
	if c.sess.lookupClientHandle(c.id) != c {
		return false
	}

	current, active := c.connection()

	return active && current == generation
}

func (c *serverClient) deactivateConnection() {
	c.connectionMu.Lock()
	defer c.connectionMu.Unlock()

	if !c.connectionActive {
		return
	}

	c.connectionActive = false
	c.connectionGeneration++
}

func (c *serverClient) activateConnection() {
	c.connectionMu.Lock()
	defer c.connectionMu.Unlock()

	if c.connectionActive {
		return
	}

	c.connectionActive = true
	c.connectionGeneration++
}

// sendQAllowance is how many deliveries the server will hold for one
// subscription before it stops holding any. It is generous: a model
// mid-turn is not reading, and a busy channel can queue a long burst
// behind a single LLM round-trip without anything being wrong.
//
// A client past it is one the server can no longer serve, and RFC
// 1459 §8.10's answer is to close the connection, not to skip
// deliveries. That keeps the invariant every reader depends on —
// what a client receives is a prefix of what the server sent, never
// a version of it with holes.
const sendQAllowance = 1024

// hasSessionLifetime reports whether this subscription's lifetime is
// the session's own. There is exactly one such subscription, and its
// identity is what says so: it belongs to the process hosting the
// server, so there is no connection under it for the server to
// close. Every bound whose remedy is a disconnect is therefore
// undefined for it — see [serverClient.queue] and
// [Session.Disconnect].
func (c *serverClient) hasSessionLifetime() bool {
	return c.id == protocol.UserClientID
}

// enqueue accepts `delivery` for this subscription and returns. It
// never blocks: a producer's progress does not depend on the
// consumer's.
//
// While the client is keeping up — nothing queued and room in its
// channel — the delivery goes straight across, so a client that
// reads promptly sees an event as soon as the command that raised
// it returns. The queue takes over the moment either is untrue, and
// from then on the pump owns delivery until it drains. Ordering
// holds across the switch because the direct path is taken only
// when the queue is empty, and the pump keeps a delivery queued
// until its send completes.
//
// A queue past `sendQAllowance` ends the connection. The rule is one
// property of the subscription, so it reaches every client the
// server can close, whatever kind of actor is behind it.
//
// A terminal drain rejects later deliveries but preserves the accepted
// prefix until the pump has sent it. An immediate unsubscribe drops
// what remains because the client has already gone.
func (c *serverClient) enqueue(ctx context.Context, delivery protocol.Delivery) {
	c.enqueueProjected(ctx, delivery, nil, 0)
}

func (c *serverClient) enqueueProjected(
	ctx context.Context,
	delivery protocol.Delivery,
	scrollbackIDs map[domain.ChannelName]int64,
	eventID int64,
) {
	if c.queue(queuedDelivery{
		delivery:      delivery,
		scrollbackIDs: scrollbackIDs,
		eventID:       eventID,
	}) {
		return
	}

	c.sess.disconnectOverflowed(ctx, c)
}

// queue appends `delivery` to the send queue, reporting false when
// the queue is already at its allowance and the subscription must be
// disconnected. A draining or reaped subscription reports true: later
// traffic is outside its accepted prefix and needs no second close.
//
// A subscription with the session's lifetime is exempt, and the
// consequence is deliberate: its queue grows without bound, keeping
// the contract every queue had before the allowance existed. The
// allowance buys a bounded queue by spending a disconnect, and that
// is a price only a closable connection can pay. Skipping deliveries
// for it instead would put a hole in the one transcript a person is
// reading, and tearing it down would run the session's shutdown
// under a process that is still running.
func (c *serverClient) queue(queued queuedDelivery) bool {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	return c.queueLocked(queued)
}

func (c *serverClient) queueLocked(queued queuedDelivery) bool {
	if c.outClosed || c.outDraining {
		return true
	}
	if c.outSealed && !queued.terminal {
		return true
	}

	c.noteLiveDM(queued)

	if !c.terminating && !c.replayBlocked && len(c.outbox) == 0 {
		select {
		case c.events <- queued.delivery:
			return true
		default:
		}
	}

	// A closing client's terminal events extend the prefix that was
	// accepted when teardown began. Ordinary traffic remains bounded
	// while the durable teardown is in progress.
	if len(c.outbox) >= sendQAllowance && !c.hasSessionLifetime() && !queued.terminal {
		c.outSealed = true

		return false
	}

	c.outbox = append(c.outbox, queued)

	select {
	case c.outWake <- struct{}{}:
	default:
	}

	return true
}

func (c *serverClient) noteLiveDM(queued queuedDelivery) {
	if !c.replayCapable || queued.eventID == 0 {
		return
	}

	message, ok := queued.delivery.Event.(domain.Message)
	if !ok || domain.InferChannelKind(message.Target) != domain.KindDM {
		return
	}

	window, ok := message.RoutingKey(c.instance.ID())
	if !ok {
		return
	}

	from, exists := c.dmLiveFrom[window]
	if !exists || queued.eventID < from {
		c.dmLiveFrom[window] = queued.eventID
	}
}

func (c *serverClient) claimServerDisconnect() bool {
	return c.disconnecting.CompareAndSwap(false, true)
}

// pump is the subscription's outbound goroutine. It moves queued
// deliveries into `events` in order, blocking there for as long as
// the consumer needs, and exits when the subscription is reaped or
// the session shuts down.
//
// The head stays on the queue until the send completes, so a
// producer appending mid-send always lands behind it and the
// consumer sees the server's order.
func (c *serverClient) pump(stop <-chan struct{}) {
	defer close(c.pumpDone)
	defer c.closeOutbound()

	for {
		delivery, ok := c.peek()
		if !ok {
			select {
			case <-c.outWake:
				continue
			case <-c.done:
				return
			case <-stop:
				return
			}
		}

		select {
		case c.events <- delivery:
			c.advance()
		case <-c.done:
			return
		case <-stop:
			return
		}
	}
}

// peek returns the delivery at the head of the queue, or false when
// the queue is empty.
func (c *serverClient) peek() (protocol.Delivery, bool) {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if c.replayBlocked || len(c.outbox) == 0 {
		return protocol.Delivery{}, false
	}

	return c.outbox[0].delivery, true
}

// advance drops the delivered head. Emptying the queue releases the
// backing array so a burst does not pin its peak size for the life
// of the subscription.
//
// The queue can be empty here: a send completing at the moment the
// subscription is reaped leaves both arms of the pump's select
// ready, and `closeOutbound` may already have released the queue.
// The delivery landed either way, so there is nothing left to drop.
func (c *serverClient) advance() {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if len(c.outbox) == 0 {
		return
	}

	c.outbox = c.outbox[1:]
	if len(c.outbox) == 0 {
		c.outbox = nil
		c.markOutboundDrainedLocked()
	}
}

func (c *serverClient) beginTermination() {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if c.outClosed || c.outDraining {
		return
	}

	c.terminating = true
}

func (c *serverClient) abortTermination() {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if c.outClosed || c.outDraining {
		return
	}

	c.terminating = false
	select {
	case c.outWake <- struct{}{}:
	default:
	}
}

func (c *serverClient) sealOutbound() {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	c.outSealed = true
}

func (c *serverClient) drainOutbound() <-chan struct{} {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	c.terminating = false
	c.outDraining = true
	c.replayBlocked = false
	c.replayRanges = nil
	c.markOutboundDrainedLocked()

	select {
	case c.outWake <- struct{}{}:
	default:
	}

	return c.outDrained
}

func (c *serverClient) markOutboundDrainedLocked() {
	if c.outDraining && len(c.outbox) == 0 {
		c.drainOnce.Do(func() { close(c.outDrained) })
	}
}

// closeOutbound refuses further deliveries and releases whatever is
// still queued. Called as the subscription is reaped, once the
// client it addressed has gone.
func (c *serverClient) closeOutbound() {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	c.outClosed = true
	c.outbox = nil
	c.drainOnce.Do(func() { close(c.outDrained) })
}

func (c *serverClient) Identity() protocol.ClientID { return c.id }

// Nick reports the actor's current nick, which a NICK rename
// rewrites while the subscription stands.
func (c *serverClient) Nick() domain.Nick { return c.instance.Nick() }

func (c *serverClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}

func (c *serverClient) Events() <-chan protocol.Delivery { return c.events }

// Done returns a channel closed when the subscription is reaped.
// The user-client's `done` channel is allocated but never closed —
// the user-client lives for the session's lifetime.
func (c *serverClient) Done() <-chan struct{} { return c.done }

// Activate releases traffic queued during attach-time replay. A
// queued delivery already present in every window snapshot is
// removed. For an actor-scoped event whose targets straddled two
// snapshots, only the targets already replayed are removed.
func (c *serverClient) Activate() {
	c.outMu.Lock()
	if !c.replayBlocked {
		c.outMu.Unlock()
		return
	}

	kept := c.outbox[:0]
	for _, queued := range c.outbox {
		queued, keep := c.afterReplay(queued)
		if keep {
			kept = append(kept, queued)
		}
	}

	c.outbox = kept
	if len(c.outbox) == 0 {
		c.outbox = nil
	}
	c.replayBlocked = false
	c.replayRanges = nil
	c.outMu.Unlock()

	select {
	case c.outWake <- struct{}{}:
	default:
	}
}

func (c *serverClient) afterReplay(queued queuedDelivery) (queuedDelivery, bool) {
	switch event := queued.delivery.Event.(type) {
	case domain.Part:
		if sourceBelongsTo(event.Source, c.instance.ID()) {
			return queued, true
		}
	case domain.Kicked:
		if event.SubjectIsSelf {
			return queued, true
		}
	}

	if len(queued.scrollbackIDs) == 0 {
		return queued, true
	}

	switch queued.delivery.Event.(type) {
	case domain.Quit, domain.NickChange:
		remaining := make([]domain.ChannelName, 0, len(queued.delivery.Targets))
		for _, window := range queued.delivery.Targets {
			id, projected := queued.scrollbackIDs[window]
			replayed, loaded := c.replayRanges[window]
			if projected && loaded && replayed.contains(id) {
				continue
			}
			remaining = append(remaining, window)
		}

		if len(remaining) == 0 {
			return queuedDelivery{}, false
		}
		queued.delivery.Targets = remaining
		return queued, true
	default:
		for window, id := range queued.scrollbackIDs {
			replayed, loaded := c.replayRanges[window]
			if !loaded || !replayed.contains(id) {
				return queued, true
			}
		}

		return queuedDelivery{}, false
	}
}

func (r replayRange) contains(id int64) bool {
	return r.first <= id && id <= r.last
}

func (c *serverClient) bumpWindowEpoch(window domain.ChannelName) {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	c.windowEpochs[window]++
}

func (c *serverClient) Scrollback(
	ctx context.Context,
	target protocol.WindowTarget,
	limit int,
) ([]protocol.ScrollbackEntry, error) {
	clientGeneration, active := c.connection()
	if !active || !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read scrollback: %w", protocol.ErrSubscriptionClosed)
	}
	if limit <= 0 {
		return nil, nil
	}
	limit = min(limit, protocol.MaxScrollbackEntries)

	if channel, ok := protocol.ChannelWindowName(target); ok {
		return c.channelScrollback(ctx, channel, limit)
	}
	if peer, ok := protocol.DirectWindowPeer(target); ok {
		return c.dmScrollback(ctx, domain.ChannelName(peer), limit)
	}

	return nil, fmt.Errorf("read scrollback: invalid window target %T", target)
}

func (c *serverClient) GuardWindow(
	ctx context.Context,
	target protocol.WindowTarget,
) (protocol.WindowGuard, error) {
	if target == nil {
		return nil, fmt.Errorf("guard window: %w: %T", protocol.ErrInvalidWindowTarget, target)
	}

	window := protocol.WindowKey(target)
	clientGeneration, active := c.connection()
	if !active || c.sess.lookupClientHandle(c.id) != c {
		return nil, fmt.Errorf("guard window %q: %w", window, protocol.ErrSubscriptionClosed)
	}

	if direct, ok := protocol.DirectWindowPeer(target); ok {
		peer := protocol.ClientID(direct)
		peerClient := c.sess.activeClientHandle(peer)
		if peerClient == nil {
			return nil, domain.UnknownNickError{Nick: domain.Nick(window), At: c.sess.now()}
		}
		peerGeneration, _ := peerClient.connection()
		c.replayMu.Lock()
		turnGeneration := c.turnGeneration
		c.replayMu.Unlock()
		peerClient.replayMu.Lock()
		dmTurnGeneration := peerClient.turnGeneration
		peerClient.replayMu.Unlock()
		if !peerClient.connectionValid(peerGeneration) {
			return nil, domain.UnknownNickError{Nick: domain.Nick(window), At: c.sess.now()}
		}

		return windowGuard{
			client: c, clientGeneration: clientGeneration, turnGeneration: turnGeneration,
			target: target, window: window, dmPeer: peerClient,
			dmPeerGeneration: peerGeneration, dmTurnGeneration: dmTurnGeneration,
		}, nil
	}

	channelTarget, ok := protocol.ChannelWindowName(target)
	if !ok {
		return nil, fmt.Errorf("guard window: %w: %T", protocol.ErrInvalidWindowTarget, target)
	}
	window = channelTarget

	channel, err := c.sess.loadChannelWindow(ctx, window)
	if !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("guard window %q: %w", window, protocol.ErrSubscriptionClosed)
	}
	if err != nil {
		return nil, err
	}
	window = channel.Name()
	if !channel.Members.HasInstance(c.instance) || !c.instance.InChannel(window) {
		return nil, domain.NotOnChannelError{Channel: window, Command: "DISPATCH", At: c.sess.now()}
	}

	c.outMu.Lock()
	epoch := c.windowEpochs[window]
	c.outMu.Unlock()
	c.replayMu.Lock()
	turnGeneration := c.turnGeneration
	c.replayMu.Unlock()

	channel, err = c.sess.loadChannelWindow(ctx, window)
	if !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("guard window %q: %w", window, protocol.ErrSubscriptionClosed)
	}
	if err != nil || !channel.Members.HasInstance(c.instance) || !c.instance.InChannel(window) {
		return nil, domain.NotOnChannelError{Channel: window, Command: "DISPATCH", At: c.sess.now()}
	}

	c.outMu.Lock()
	currentEpoch := c.windowEpochs[window]
	c.outMu.Unlock()
	if currentEpoch != epoch {
		return nil, domain.NotOnChannelError{Channel: window, Command: "DISPATCH", At: c.sess.now()}
	}

	return windowGuard{
		client: c, clientGeneration: clientGeneration, turnGeneration: turnGeneration,
		target: target, window: window, epoch: epoch,
	}, nil
}

func (c *serverClient) GuardInvitation(
	ctx context.Context,
	channel domain.ChannelName,
) (protocol.WindowGuard, error) {
	clientGeneration, active := c.connection()
	if !active || !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("guard invitation for %q: %w", channel, protocol.ErrSubscriptionClosed)
	}

	c.replayMu.Lock()
	defer c.replayMu.Unlock()

	window, generation, invited := c.sess.invitationState(ctx, channel, c.instance.ID())
	if !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("guard invitation for %q: %w", channel, protocol.ErrSubscriptionClosed)
	}
	if !invited {
		return nil, domain.NotOnChannelError{
			Channel: channel,
			Command: "INVITE",
			At:      c.sess.now(),
		}
	}
	turnGeneration := c.turnGeneration
	windowEpoch := c.windowEpoch(window.Name())

	return invitationGuard{
		client: c, clientGeneration: clientGeneration, turnGeneration: turnGeneration,
		channel: window.Name(), generation: generation, windowEpoch: windowEpoch,
	}, nil
}

func (c *serverClient) Replies(
	ctx context.Context,
	window protocol.WindowTarget,
	limit int,
) ([]protocol.ReplyEntry, error) {
	clientGeneration, active := c.connection()
	if !active || !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read private replies: %w", protocol.ErrSubscriptionClosed)
	}
	if limit <= 0 {
		return nil, nil
	}
	limit = min(limit, protocol.MaxScrollbackEntries)

	var guard protocol.WindowGuard
	var err error
	if window != nil {
		guard, err = c.GuardWindow(ctx, window)
		if err != nil {
			return nil, err
		}
	}

	stored, err := c.sess.store.InstanceRepliesForWindowBefore(
		ctx, c.instance.ID(), window, nil, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read private replies: %w", err)
	}

	if !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read private replies: %w", protocol.ErrSubscriptionClosed)
	}
	if guard != nil && !guard.Valid(ctx) {
		return nil, fmt.Errorf("read private replies: %w", protocol.ErrWindowAuthorityChanged)
	}

	replies := make([]protocol.ReplyEntry, 0, len(stored))
	for _, reply := range stored {
		replies = append(replies, protocol.ReplyEntry{Window: reply.Window, Event: reply.Event})
	}

	return replies, nil
}

func (c *serverClient) DirectoryChannels(
	ctx context.Context,
) ([]domain.ChannelDirectoryEntry, error) {
	clientGeneration, active := c.connection()
	if !active || !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read channel directory: %w", protocol.ErrSubscriptionClosed)
	}

	for {
		generation := c.sess.directoryGeneration.Load()
		entries, err := c.sess.directoryChannels(ctx, c.instance)
		if err != nil {
			return nil, err
		}
		if !c.connectionValid(clientGeneration) {
			return nil, fmt.Errorf("read channel directory: %w", protocol.ErrSubscriptionClosed)
		}
		if c.sess.directoryGeneration.Load() == generation {
			return entries, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (c *serverClient) channelScrollback(
	ctx context.Context,
	window domain.ChannelName,
	limit int,
) ([]protocol.ScrollbackEntry, error) {
	clientGeneration, active := c.connection()
	if !active || !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read scrollback for %q: %w", window, protocol.ErrSubscriptionClosed)
	}

	channel, err := c.sess.loadChannelWindow(ctx, window)
	if err != nil {
		return nil, err
	}

	window = channel.Name()
	if !channel.Members.HasInstance(c.instance) || !c.instance.InChannel(window) {
		return nil, domain.NotOnChannelError{
			Channel: window,
			Command: "SCROLLBACK",
			At:      c.sess.now(),
		}
	}

	c.replayMu.Lock()
	defer c.replayMu.Unlock()

	c.outMu.Lock()
	epoch := c.windowEpochs[window]
	var before *int64
	if c.replayBlocked {
		before = c.firstQueuedScrollbackID(window)
	}
	c.outMu.Unlock()

	stored, err := c.sess.store.ChannelScrollbackBefore(
		ctx, c.instance.ID(), window, before, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read scrollback for %s: %w", window, err)
	}

	// Membership may have changed while the database read was in
	// progress. Discard the result unless the actor can still read
	// the channel now.
	channel, err = c.sess.loadChannelWindow(ctx, window)
	if !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read scrollback for %q: %w", window, protocol.ErrSubscriptionClosed)
	}
	if err != nil || !channel.Members.HasInstance(c.instance) ||
		!c.instance.InChannel(window) {
		return nil, domain.NotOnChannelError{
			Channel: window,
			Command: "SCROLLBACK",
			At:      c.sess.now(),
		}
	}

	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.windowEpochs[window] != epoch {
		return nil, domain.NotOnChannelError{
			Channel: window,
			Command: "SCROLLBACK",
			At:      c.sess.now(),
		}
	}
	if c.replayBlocked && len(stored) > 0 {
		c.replayRanges[window] = replayRange{
			first: stored[0].ID,
			last:  stored[len(stored)-1].ID,
		}
	}

	return scrollbackEntries(
		stored,
		protocol.HistorySourceChannelScrollback,
		protocol.ChannelWindowTarget(window),
	), nil
}

// firstQueuedScrollbackID returns the first projected row for
// `window` that is already waiting in the live queue. The caller
// holds outMu, so no producer can insert an earlier boundary before
// the corresponding snapshot completes.
func (c *serverClient) firstQueuedScrollbackID(window domain.ChannelName) *int64 {
	for _, queued := range c.outbox {
		if id := queued.scrollbackIDs[window]; id != 0 {
			return &id
		}
	}

	return nil
}

func (c *serverClient) dmScrollback(
	ctx context.Context,
	window domain.ChannelName,
	limit int,
) ([]protocol.ScrollbackEntry, error) {
	c.sess.dmHistoryMu.Lock()
	defer c.sess.dmHistoryMu.Unlock()

	peer := protocol.ClientID(window)
	clientGeneration, active := c.connection()
	if !active || !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read scrollback for %q: %w", window, protocol.ErrSubscriptionClosed)
	}

	peerClient := c.sess.activeClientHandle(peer)
	if peerClient == nil {
		return nil, fmt.Errorf("read scrollback for %q: %w", window, protocol.ErrWindowAuthorityChanged)
	}
	peerGeneration, _ := peerClient.connection()

	c.outMu.Lock()
	before, hasLive := c.dmLiveFrom[window]
	c.outMu.Unlock()

	var cutoff *int64
	if hasLive {
		cutoff = &before
	}

	stored, err := c.sess.store.DMEventsBefore(
		ctx,
		c.instance.ID(),
		domain.InstanceID(peer),
		cutoff,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read scrollback for %q: %w", window, err)
	}

	if !c.connectionValid(clientGeneration) {
		return nil, fmt.Errorf("read scrollback for %q: %w", window, protocol.ErrSubscriptionClosed)
	}
	if !peerClient.connectionValid(peerGeneration) {
		return nil, fmt.Errorf("read scrollback for %q: %w", window, protocol.ErrWindowAuthorityChanged)
	}

	c.outMu.Lock()
	if current, ok := c.dmLiveFrom[window]; hasLive && ok && current == before {
		delete(c.dmLiveFrom, window)
	}
	c.outMu.Unlock()

	return scrollbackEntries(
		stored,
		protocol.HistorySourceEvent,
		protocol.DirectWindowTarget(peer),
	), nil
}

func scrollbackEntries(
	stored []domain.StoredEvent,
	kind protocol.HistorySourceKind,
	window protocol.WindowTarget,
) []protocol.ScrollbackEntry {
	entries := make([]protocol.ScrollbackEntry, 0, len(stored))
	for _, event := range stored {
		if event.Event == nil {
			continue
		}

		entries = append(entries, protocol.ScrollbackEntry{
			Event:   event.Event,
			History: historyRef(kind, event.ID, window),
		})
	}

	return entries
}

func historyRef(
	kind protocol.HistorySourceKind,
	id int64,
	window protocol.WindowTarget,
) protocol.HistoryRef {
	if kind == protocol.HistorySourceChannelScrollback {
		channel, _ := protocol.ChannelWindowName(window)
		return protocol.ChannelHistoryRef(id, channel)
	}

	peer, _ := protocol.DirectWindowPeer(window)
	return protocol.DirectHistoryRef(id, peer)
}

// Unsubscribe removes the client from the session's subscriber
// registry and closes [Done]. The user-client never reaps — its
// lifetime equals the session. Idempotent across concurrent
// callers via `unsubOnce`.
func (c *serverClient) Unsubscribe() { c.sess.reapClient(c.id) }

func (c *serverClient) HasMode(m domain.Mode) bool {
	c.modesMu.RLock()
	defer c.modesMu.RUnlock()

	_, ok := c.modes[m]
	return ok
}

// setMode adds or clears a single mode flag under the write lock.
// Idempotent: a grant for an already-held mode (or a clear for an
// unheld mode) is a no-op. Returns true if the call mutated state —
// actor methods use it to decide whether to emit a
// [domain.UserModeChange].
func (c *serverClient) setMode(m domain.Mode, add bool) bool {
	c.modesMu.Lock()
	defer c.modesMu.Unlock()

	_, present := c.modes[m]

	if add {
		if present {
			return false
		}
		c.modes[m] = struct{}{}
		return true
	}

	if !present {
		return false
	}
	delete(c.modes, m)
	return true
}

// Caps returns the client as a [command.CapabilityHolder] bound to
// live state. Each call to [command.CapabilityHolder.Has] re-reads
// the current mode set, so a mode mutation is reflected on the
// next consultation by the suggestion filter or the tool registry.
func (c *serverClient) Caps() command.CapabilityHolder { return c }

// Has implements [command.CapabilityHolder]. Adding a new capability
// that maps to a mode requires both a [protocol] constant and a
// new case here.
func (c *serverClient) Has(capability command.Capability) bool {
	switch capability {
	case protocol.CapOperator:
		return c.HasMode(domain.ModeOperator)
	default:
		return false
	}
}

// canReceive reports whether this subscription should receive
// `ev`. Both kinds of client ride the same filter: a subscription
// receives only events whose target window it is a member of, or
// actor-scoped events (Quit, NickChange) where the recipient shares
// any channel with the actor — RFC 2812 §3.3.1's intersection rule.
// The user-client is a member of whatever it has joined, so the
// chat-screen renders exactly those windows; server handshake
// numerics and command replies reach it point-to-point (via
// [Session.deliverToClient] or the issuing command's
// `Response.Events`), not through this filter. `targets` is the
// per-recipient intersection that [Session.fanOutProtocol] computed
// for this fan-out; it is non-empty exactly when the actor and `c`
// share at least one channel, so the test for actor-scoped delivery
// is just a length check.
func (c *serverClient) canReceive(ev domain.ProtocolEvent, actorTargets []domain.ChannelName) bool {
	_, active := c.connection()
	if !active {
		return false
	}

	switch e := ev.(type) {
	case domain.Message:
		return modelTargetsThis(c, e.Target)
	case domain.Join:
		return c.instance.InChannel(e.Target)
	case domain.Part:
		return c.instance.InChannel(e.Target)
	case domain.TopicChange:
		return c.instance.InChannel(e.Target)
	case domain.TopicInfo:
		return c.instance.InChannel(e.Target)
	case domain.ChannelModeChange:
		return c.instance.InChannel(e.Target)
	case domain.Kicked:
		return c.instance.InChannel(e.Target)
	case domain.Quit, domain.NickChange, domain.ModelDispatchStarted, domain.ModelDispatchDone:
		_ = e
		return len(actorTargets) > 0
	case domain.PokeEvent:
		return c.instance.InChannel(e.Channel)
	case domain.NamesReplyEvent:
		return c.instance.InChannel(e.Channel)
	case domain.NamesEnd:
		return c.instance.InChannel(e.Channel)
	case domain.ModelUnavailableError:
		_ = e
		// Dispatch failures are operator diagnostics, rendered in the
		// operator's status window; an operator subscription receives
		// them across every window, channel and DM alike.
		return c.HasMode(domain.ModeOperator)
	case domain.SystemNotice:
		_ = e
		// A notice reaching this filter reports server-side work no
		// client issued, so it is addressed the same way a dispatch
		// failure is. A notice answering a command reaches its issuer
		// through [Session.deliverToClient] or the command's
		// `Response.Events`, neither of which consults this filter.
		return c.HasMode(domain.ModeOperator)
	}

	// Server handshake numerics (Welcome, Reconnected) and the
	// point-to-point command replies the session emits (Whois,
	// ListReply, ListEnd) reach the issuing client through
	// [Session.deliverToClient] or the command's `Response.Events`.
	// Help, UsageHint, PersonaTemplatesList and CommandError are
	// chat-screen-local control signals the session never puts on this
	// bus.
	return false
}

// modelTargetsThis reports whether a [domain.Message] target
// addresses this model — either its own DM (target equals the
// instance id) or a channel it is in. The sender side is gated by
// the echo helper, not here.
func modelTargetsThis(c *serverClient, target domain.ChannelName) bool {
	if domain.InferChannelKind(target) == domain.KindDM {
		return target == domain.ChannelName(c.id)
	}

	return c.instance.InChannel(target)
}
