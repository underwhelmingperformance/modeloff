# `modeloff`

An old school IRC-style interface but for one user talking to multiple agents.

## Services

There is no network server. The session in `internal/session` is an in-process
IRC-like server; the only external service is the OpenRouter API.

## The flow

1. Start the application. A cool IRC-style connection sequence appears.
   1. If there's no OpenRouter API key configured, the user is prompted to use
      `/config` to set it up. The app won't work until this is done. The
      prompt is enforced two ways: the welcome checklist while no channel is
      open, and a persistent status-bar item once one is, so it stays
      visible for as long as no key is configured.
   2. A step failing during the sequence is fatal only for the connect step:
      every later step depends on the store-backed handshake it performs. A
      failure loading the model catalogue or joining the autojoin channels is
      noted on its own line, and the sequence still completes and hands off
      to the chat screen.
2. Any channels from last time are loaded and shown in the sidebar. The
   channel that was open last time is opened again.
   1. If there are no channels, a welcome message is shown.
3. The user can `/join` a channel (`#`-prefix like IRC), or switch windows
   with the usual IRC-client keybindings — `alt+1`…`alt+9` for a direct
   switch, `alt+a` for the next window with activity, `ctrl+n`/`ctrl+p` for
   next and previous — or the mouse. `ctrl+u` (kill to line start) and
   `ctrl+d` (delete character) belong to the input editor.
   1. If the channel doesn't exist, it is created with the default channel
      modes — a validated `/config` setting, defaulting to `+nt`.
   2. The user can have multiple channels open at once.
4. The user can `/part` a channel.
   1. A channel exists only while it has occupants; the last occupant
      parting destroys it, along with its topic, modes and invitation list
      (RFC 2811 §2). The sidebar entry is client state and survives a
      part. So is the autojoin list, and the client drops the channel
      from it, because parting says the channel should not come back on
      the next connection. If durable channels are ever wanted, the
      mechanism is an explicit permanent-channel mode.
5. The user can `/list` all channels, subject to the channel-visibility
   rule described below.
6. The user can `/invite` models to add them to the channel, and `/kick`
   them to remove them.
   1. The user can specify a model by name or ID, and the app will look it up
      using the OpenRouter API. If no name or ID is given, the user is prompted
      to select from a list of models or existing instances.
   2. When an existing instance is invited, a memory system is used so that it
      remembers previous conversations. This applies only to instances that
      still exist: QUIT and KILL end a client and free its nick, and a quit
      instance cannot be re-invited. Bringing one back would need an account
      registry distinct from the connection record (the services model),
      introduced for human and model users alike.
7. The user can `/msg` to DM a model, which is shown similarly to a channel
   except no `#` prefix.
   1. A model can start one too. A message from a model the user has no
      window for opens the window and badges it, without taking the
      focus off whatever the user is reading.
   2. `/close` (`/wc`, `/unquery`) closes the window in view. The open
      windows are remembered across restarts and reopened on the next
      one; closing a window leaves the conversation itself in the log,
      and the next message opens a window on it again.
8. From then on, it's a channel. Event delivery follows the echo gate and
   membership filter described below. Models can reply or not. Runaway
   conversations are bounded by server-side flood control applied uniformly
   to every client (RFC 1459 §8.10's penalty algorithm, with channel mode
   `+f` for a per-channel limit); the throttle is surfaced to the throttled
   client, and a turn is never silently dropped. Any spend budget beyond
   that is app policy expressed through IRC primitives such as `+m` or
   KILL, not a hidden gate.
9. A channel can have a `/topic`, which is shown in the UI. This is optional
   but it will be sent to the model as part of its prompt.
10. A small, cheap model (configurable, defaulting to `openai/gpt-5.4-mini`) is
    used to give each invited model a nickname.
11. `/whois` can be used on a nickname to show metadata and the target's
    channels, subject to the same channel-visibility rule `/list`
    answers under.
12. On a random (perturbed a bit) configurable (via `/config`) schedule, the
    model instances are poked to see if they want to say anything, so that
    channels don't go dead. The poke is the server's PING (RFC 2812 §3.7.2):
    an unsolicited prod the model may answer with a PRIVMSG or with nothing.
13. Models can be given a persona when they're instantiated.
14. The user can rename themselves via `/nick`. By default we use their username
    on the system.

## Model interaction

1. The model interaction itself should be via a typed protocol where the
   model can _explicitly_ choose not to reply, and they should be encouraged
   to take that option. Remember that models _have_ to reply with something when
   we call the API.
2. In a way the protocol should follow the IRC protocol. For example the model
   will be told when there's a message, when there's a join/part event etc.
3. There should be a per-instance (keyed by the immutable instance ID, so
   memories survive a `/nick` rename) memory system so that the model can
   remember what's happened to it. This should be exposed as a tool so
   that it can decide when to read and write memories.

## Server-client protocol

The session in `internal/session` is an in-process IRC-like server. The
chat-screen (one per running TUI) and each model instance are uniform clients
on the same bus. The contract is the [`internal/protocol`][protocol-pkg]
package; the dispatcher does not branch on which kind of client it is talking
to, and capability parity is enforced at the type level.

[protocol-pkg]: ./internal/protocol/protocol.go

### Contract

`protocol.Command` is a closed sum, sealed via an unexported `isCommand()`
method on each member. `protocol.Event` is an alias for `domain.ProtocolEvent`,
also sealed via an unexported method declared on each event type in the
`domain` package. Adding a new command type makes every dispatcher arm fail
to build until it is handled — the migration path is mechanical.

Clients implement the small `Client` interface — `Identity()`, `Send(ctx,
Command) (Response, error)`, `Events() <-chan Delivery`, `Caps()
command.CapabilityHolder`; each `Delivery` wraps an `Event` with the originating
handler's span context for OTel trace continuity. Both the dispatcher's operator
gate and `Caps()` read the live `serverClient` mode set keyed by `Identity`, so
an `Oper` elevation is honoured without the client object changing, and the
commands a client is offered are decided from the same place as the commands
the dispatcher will run for it. Neither client kind holds a capability constant
of its own: `protocol.LiveCaps` binds an identity to `Session.ClientCaps` and
both kinds return one. A `Send` returns a `Response` whose `Err` field carries
any typed command failure (e.g. `domain.NotOperatorError`,
`domain.UnknownNickError`); callers branch on it via `errors.As`. The
`Response.Events` slot carries the dispatcher's synchronous numeric-reply
payloads: the persisted `domain.Message` for `PrivMsg`/`Action`,
`domain.Invited` for `Invite`, the `domain.Whois` snapshot for `Whois`,
the `domain.ListReply` stream terminated by `domain.ListEnd` for `List`, and a
`domain.SystemNotice` for each warning `ADDMODEL`'s preparation reported.
Broadcast side effects flow asynchronously over `Client.Events()` to peers.

### Two kinds of actor

The user-client and the model-clients are uniform on the protocol — same
`Send → Handle`, same event types, same dispatcher — but they are deliberately
different kinds of actor in their lifecycle and the capabilities granted at
attach.

Persistence is a property of identity and connection state, not of actor kind.
A client that has not quit is still connected: the fiction the app maintains is
that the server kept running while the user was away, so such a client returns
with its context intact. The persisted event log (`store.EventsBefore` /
`store.DMEventsBefore`) is the server's memory of channel activity; on
(re)attach a model-client restores its context from it through the
history-replay capability. A client that quit is gone, whatever kind it is:
QUIT ends the client and frees its nick. The user-client does not request
history replay, so it sees live traffic forward and nothing from before it
connected — the chat-screen's scrollback is populated purely from live events.

`Session.quitAs` is that one teardown, and it runs the same whoever sent
the QUIT. The event is logged and broadcast while the actor is still on
its channels, which is the order PART follows and what carries the QUIT
to the peers who share them and to the departing client itself.
Membership goes afterwards, one channel at a time through the shared
`removeMember`. The instance record goes last, and deleting it is what
frees the nick, for every client alike. What differs for the client
whose lifetime is the session's is only what sits under the
connection: `Session.releaseClient` has no model-client to release and
`Session.reapClient` has no connection to close, so both refuse for it
and its subscription outlives its own QUIT. The subscription is the
only thing that survives.

A channel the departure empties is destroyed like a last PART (RFC
2811 §2), and everything the channel held goes with it: its topic,
its modes and its invitation list, and its event log too, which the
next store open removes as orphaned rows once no `channels` row
answers to the name. Autojoin then recreates the channel on the next
connection, empty, with the configured default modes and no history.
A QUIT is therefore how a channel nobody else is in is forgotten, and
`/part` is the same act said deliberately.

Differences between the two kinds are expressed as server-side capabilities
granted at attach (`SubscribeOptions.InitialModes`) and read live off the
issuing `serverClient`, not as a branch on which kind of client it is.

### Two client kinds

- The user-client lives in the `internal/userclient` package. It is
  constructed in the repo-root `main.go` (or a test fixture), holds the
  user's `*domain.Instance`, and attaches to the session via the
  public `Session.Subscribe(c, opts)` API with `+o` requested
  through `protocol.SubscribeOptions.InitialModes`. Its
  `Identity()` is the sentinel `protocol.UserClientID` (the empty
  `ClientID`); its lifetime equals the session's. The chat-screen
  holds a `*userclient.UserClient` directly and reads identity,
  channel membership, and the protocol bus through it; user-actor
  convenience methods (`Join`, `Part`, `SendMessage`, `SendAction`,
  `SetTopic`, `ChangeNick`, `Quit`, `JoinAutojoinChannels`,
  `MarkRead`) construct the appropriate `protocol.X` command (or the
  equivalent store-side work) and dispatch through `Send`. The
  user-client's `Poke` is the exception: poke is not a user action.
  The automatic schedule is session-owned (`Session.StartPoking`
  drives a perturbed, configurable cadence that nudges only channels
  gone quiet since the last cycle, per point 12 above); `Poke` merely
  relays the optional manual `/poke` to `Session.PokeNow`.
- A model-client lives in the `internal/modelclient` package. It
  owns the dispatch goroutine, the per-channel history ring buffer
  used for prompt assembly, the memory-tool registry, and a getter
  for the live OpenRouter `api.Client`. `ModelClient.Attach`
  registers with the session via the public `Session.Subscribe(c,
  opts)` API, storing the resulting `protocol.Subscription`
  internally, and returns only an error. Ending the connection is
  two phases, because a model can end its own: `Release` cancels the
  loop's context and unsubscribes, and is safe from any goroutine —
  including the dispatch goroutine a `quit` tool call runs on —
  while `Wait` joins that goroutine and belongs to whoever owns the
  client's lifetime. `Detach` is the two together, and carries
  `Wait`'s restriction. A client released mid-session — by QUIT,
  KILL, a send-queue disconnect or a failed ADDMODEL — leaves the
  manager's registry at once, since its identity is free from that
  point, and moves to a draining set: its turn may still be inside
  an upstream call, and `Manager.DetachAll(ctx)` joins the draining
  set along with the attached one. That is the only join, and
  `main.go` runs it on the way out under the configured
  `DrainTimeout`; see Shutdown below. The
  dispatch goroutine watches the subscription's
  `Events` and `Done` channels and runs an LLM turn when
  `dispatchTrigger` says so (a message or join in a window the
  instance shares, a part by another member, an invite addressed to
  it, a poke). The window a turn runs in is the one
  `domain.Message.RoutingKey` names, so a DM turn runs under the
  counterpart: that is the window the tools address and the window
  `dispatchWindowFor` builds for the prompt, which is how a `/me` in
  a DM reaches the person the model is talking to.
  A burst is taken as a batch: after a delivery arrives
  the loop drains what is already queued and gives one turn every
  trigger that arrived for the same window, so a model that was busy
  catches up in a single prompt. Each turn
  re-reads the api client through the getter so a manager-driven
  `SetAPIKey` rebuild propagates without reattach; a nil client means
  no API key is configured, and the turn ends with a
  `ModelUnavailableError` and no upstream call. A turn lost to a
  transient upstream failure is dispatched once more, after
  `dispatchRetryDelay` with `dispatchRetryJitter` either side of it.
  `api.Retryable` is what counts as transient: HTTP 429, any 5xx, or
  an expired deadline.
  The wait runs on a goroutine of its own and the turn returns to the
  loop's select, so the queue keeps draining while it runs; a second
  failure stays failed. A turn raised for the same window during the
  delay supersedes the pending one, which is what keeps a message from
  reaching the model twice: `fileBatch` files every delivery into the
  ring as it arrives, so the failed turn's traffic is already the
  superseding turn's transcript, and re-asking about it afterwards
  would have the model answer a line it had just read. The loop's
  `redispatchSet` is where the two arms agree, and it is loop-owned,
  so it needs no lock. A panic in
  the loop ends the connection through `Session.Disconnect`, so a dead
  dispatch goroutine leaves a QUIT in the channel and no orphaned
  subscription behind it.

The LLM-side state — the api client and its rebuild factory, the
persona pool, the small-model id used for nick generation, the
catalogue cache, and the per-instance model-client registry —
lives in the `internal/modelmanager` package. A `*Manager`
satisfies `session.ModelClientFactory`: the session's `addModelAs`
(the `protocol.AddModel` handler) asks the manager to construct a
model-client when a new instance is added to a channel, `KILL` /
`QUIT` ask it to detach, and `ADDMODEL` asks it for persona
arbitration and a unique nick via `PrepareInstance`. The chatcmd and chat-
screen layers route persona / api-key / model-directory commands
through the manager too; nothing LLM-shaped flows through the
session router.

### Dispatcher

`Session.Handle(client, cmd)` is the single entry point for any model
action. The dispatcher's exhaustive switch over `protocol.Command` resolves
the issuing actor via `resolveClientActor`, runs an operator-mode check
where required, then delegates to a per-command implementation in the
`session` package. The actor surface (`joinAs`, `partAs`, `sendMessageAs`,
…) is unexported: outside the package, the only way to reach it is
through `Send → Handle`.

`AddModel`, `Quit`, `Kill`, and `Oper` are full dispatcher handlers
(`handleAddModel`, `handleQuit`, `handleKill`, `handleOper`); there are no
legacy public `Session.AddModel` / `Session.QuitAs` methods. `AddModel` and
`Kill` are operator-gated; a non-operator client receives
`domain.NotOperatorError`.

`KILL` names a nick, and no nick is exempt, the issuing operator's own
included (RFC 2812 §3.7.1). A client that kills itself gets the
ordinary teardown: the QUIT carries `"Killed by <oper> (<reason>)"`,
membership goes, and a channel the departure empties is destroyed. The
only thing the server leaves undone is closing a connection it does
not have. The chat-screen recognises its own QUIT on the bus and exits
on it, which is the same ending `/quit` reaches by a different route.

Two cases leave the killed client with no channel the QUIT can arrive
through: a client on no channels at all, and one whose channels all
carry `+a`, where §4.2.1 withholds the QUIT and puts a masked `PART`
there instead. `quitAs` asks `namedChannels` the question
`maskActorEvent` asks per recipient, and delivers the QUIT point to
point when the answer is empty. That is the fallback `changeNickAs`
makes for NICK, and for the same reason: a client is told what
happened to its own connection.

### Command loop and live channel state

The session is a single writer. `Session.Handle` does not run a
command on the caller's goroutine: each handler hands its
state-touching work to the session's command loop through
`onWriter`, and that one goroutine runs commands serially in
arrival order. A command therefore sees the full effect of every
command before it and none of any command after it, which is what
makes a handler's read-modify-write of a channel atomic. The call
stays synchronous for the caller — `Handle` still returns that
command's own `Response`.

Two rules follow, and both are load-bearing for anything added to
the `session` package:

- Code running on the loop must not call `Handle` or a client's
  `Send`. The loop is busy running the current command, so a nested
  submission would never be taken up.
- Blocking work stays off the loop. `ADDMODEL` runs
  `ModelClientFactory.PrepareInstance` (an LLM round-trip) and
  `ModelClientFactory.Attach` (which loads the new client's history
  from the store) off it, and `QUIT` / `KILL` run
  `ModelClientFactory.Detach` after leaving it — the model-client it
  releases may be one whose dispatch goroutine is queued behind the
  loop for a command of its own. Everything that touches session
  state runs on it.

`ADDMODEL` is the one command that takes the loop twice, because it
is the sequence a client goes through on a real server: register
(claim the nick, record the instance — on the loop), connect (attach
the model-client — off it), then join (on the loop). Attaching
between the two is what lets the new client receive its own JOIN and
the `RPL_NAMREPLY` / `RPL_TOPIC` that follow.

No emitter ever blocks on a consumer. Each subscription owns an
outbound queue and a pump goroutine: a producer — the command loop,
the poke scheduler, a model-client's own emissions — hands the
delivery over and returns, and only the pump waits on the client's
channel. Without that, a model that is mid-turn and therefore not
reading, while waiting on the loop for a command of its own, would
deadlock the whole server. A backlog therefore survives a consumer
that has fallen behind, up to `sendQAllowance`; what the queue still
holds is released when the subscription is reaped or the session
shuts down, since in both cases there is nobody left to read it.

Past that allowance the server stops holding anything for that
client and disconnects it instead (RFC 1459 §8.10). The disconnect
goes through the ordinary QUIT teardown — `Session.Disconnect` runs
the QUIT on the loop, releases the model-client and reaps the
subscription — so the channel sees the client leave with a "Max
SendQ exceeded" message. Dropping deliveries would leave every
reader's transcript with a hole in it and nothing to say one was
there. The rule is one property of the subscription, so it reaches
every client the server can close, whatever kind of actor is behind
it.

The exception is the one client whose lifetime is the session's.
The allowance buys a bounded queue by spending a disconnect, and
that is a price only a closable connection can pay: this client is
the process hosting the server, so there is no connection under it
to close and `Session.Disconnect` is undefined for it. Its
subscription therefore keeps the unbounded queue every subscription
had before the allowance existed, and that consequence is accepted
deliberately — the alternatives are a hole in the one transcript a
person is reading, or running the session's own shutdown underneath
a process that is still running. If a hopelessly-behind UI is worth
noticing, the mechanism is a metric, not a QUIT.

Channel state — member lists, topics, modes, invitation sets —
lives in memory in the session and is the source of truth. Records
enter it on demand from the store and are written through to the
store on every mutation; a read hands the caller its own copy, so
readers on other goroutines (the fan-out's `+a` check, the send
gates, a model assembling its prompt) never touch the record the
next command will read, and never pay for a row fetch. Enumerating
every channel (`LIST`, the poke scheduler) still reads the store,
since live state holds only the channels this session has touched.
The two agree while the write-through succeeds; when it fails they
part company, and the persistence-failure counter is what reports
it.

The nick space is claimed on the loop for the same reason: `NICK`
checks and takes a nick as one step, and `ADDMODEL` re-checks the
nick it was given, because that nick was chosen off the loop and a
rename may have taken it in the meantime.

### Shutdown

Teardown runs in the order the layers sit in, and one deadline covers
all of it. `main.go` cancels the application context, which wakes
every dispatch goroutine and stops the command loop; then
`Manager.DetachAll(ctx)` releases every model-client and joins its
dispatch goroutine, the attached ones and the draining ones alike;
then `Session.Shutdown(ctx)` closes the registration gate and joins
every subscription's outbound pump. Only then does the deferred
`Store.Close` run, so on the clean path nothing is still reading or
writing the database underneath it.

The deadline is the `/config drain-timeout` setting, and it bounds
the drain end to end. Releasing a client cancels the context its turn
runs under, but an upstream call already handed to the network
answers when it answers. `DetachAll` therefore waits
per client and gives up at the deadline, returning a
`modelmanager.DrainTimeoutError` naming the clients still dispatching.
Those goroutines are abandoned to the exiting process, which is the
price of a shutdown that finishes; the warning is what tells the
operator it happened. A configured value that is not positive would
mean no drain at all, so `main` falls back to
`config.DefaultDrainTimeout` and says so.

Spending the deadline in the drain leaves `Shutdown` none, so on that
path the outbound pumps are abandoned with the turns. `Shutdown`
closes the registration gate before it consults the deadline, though,
and closing that gate is what stops the command loop. An abandoned
turn waking after `Store.Close` therefore has every wire command it
issues refused with `session.ErrSessionClosed`, without a database
round trip; the one thing it can still reach the database through is a
memory tool, which answers the model with the failure. Nothing on that
path reaches the operator's log.

### Message targets

`PrivMsg` and `Action` carry a `protocol.MsgTarget`: the closed sum of
what RFC 2812 §3.3.1's `<msgtarget>` may name. A client says what it
is addressing and the server works out which conversation that is.
No client resolves a target itself.

- `ChannelTarget` names a channel. The send gates canonicalise the
  spelling against the channel record, so `#Dev` is logged and
  broadcast under `#dev`.
- `NickTarget` names another client by nick, matched under the
  server's casemapping. This is what a person types after `/msg` and
  what a model passes to the `msg` tool; `protocol.ParseMsgTarget`
  reads a raw token into a channel or a nick exactly as a server reads
  the wire parameter.
- `ClientTarget` names another client by `InstanceID`, for a client
  that already holds the conversation open. A nick is display state
  its holder may change, so a window pinned to a client addresses it
  by identity, the same choice `domain.Invitations` makes.
  `protocol.TargetForWindow` builds the target from a window key, and
  is how the user-client sends from a DM window and how a model's
  `/me` addresses the window its turn is running in.

`Session.resolveMsgTarget` runs the resolution on the command loop
against the registry of connected clients, so addressing a message
costs the loop no store round-trip. A target naming no connected
client is refused with `domain.UnknownNickError` (RFC 2812 numeric 401
ERR_NOSUCHNICK) and nothing is logged: a message the server cannot
place has to come back as a refusal, since a client that is told
nothing assumes it arrived.

`Session.resolveConnectedNick` answers the same question for
`INVITE`, `KICK`, `WHOIS`, `KILL` and a member-mode `MODE` change
(`+o`/`+v`'s target): which connected client currently holds this
nick. It reads the same registry `resolveMsgTarget` reads, so all
five agree with message addressing on who a nick reaches, and the
user resolves like any other client through the sentinel empty
`protocol.ClientID` its subscription is registered under. A
subscription is what makes a client addressable: the connection
record says a client exists, and the subscription says the server can
reach it. A nick outside the RFC 2812 §2.3.1 grammar is refused with
`domain.ErroneousNicknameError` (432) before any lookup runs; a nick
naming no connected client is refused with `domain.UnknownNickError`
(401), including a nick an instances row still holds when the client
behind it never attached: the server has no subscription to deliver
a kick, a whois answer, a mode grant or a kill's disconnect to
there.

DMs have no wire-level "open" command. A direct message is just a
`PrivMsg` naming the counterpart. The conversation key is the
counterpart's `InstanceID`, and each direction is logged under its
recipient: a line to `botty` under botty's id, botty's answer under
the user's (empty) id. `store.DMEventsBefore` reads the union, and
`domain.Message.RoutingKey` is how either party turns a single event
into the conversation it belongs to.

Opening and closing a DM window is therefore the client's own
business, and the session sees none of it. `/query` opens one; a DM
arriving from a counterpart the user has no window for opens one too,
the autocreate-on-message rule every IRC client has for an
unsolicited private message; `/close` closes one. None of the three
touches the conversation, which is in the event log either way.

### Casemapping and naming

Two names are the same name when they fold to the same thing (RFC
2812 §2.2). This server uses the `ascii` casemapping: `A`-`Z` fold
to `a`-`z` and nothing else. `rfc1459`'s extra fold of `[]\~` onto
`{|}^` would take those characters away from nicks that may legally
use them, and Unicode folding would make the answer depend on the Go
version's case tables. The `ascii` fold is also exactly SQLite's
NOCASE collation, so a case-insensitive lookup is an index seek
(`idx_instances_nick_nocase`, `idx_channels_name_nocase`) and the
store never has to read a row to decide whether it matches.

`domain.EqualNick` and `domain.KeyForChannel` apply the fold to nicks
and channel names; `command.Set` and `command.Node`'s name resolution
apply the same fold, exported as `domain.CaseFold`, to command and
subcommand names in the chat grammar. Those are the only three places
the fold is applied. The session's live channel state is keyed by
`domain.ChannelKey`, and `store.GetWindow` / `store.ResolveNick`
match under NOCASE, so `#Dev` and `#dev` are one channel and `Botty`
and `botty` are one client wherever the question is asked. Neither
store index is UNIQUE: uniqueness is the command loop's to enforce,
where a nick is claimed and a channel is created, and a UNIQUE index
would refuse to be created against a database written before the
casemapping existed, which may already hold both spellings.

Case is preserved, never folded away. A channel keeps the spelling
it was created with and a client keeps the spelling of the nick it
took; that spelling is what goes on the wire. Every `*As` method
that loads a channel record adopts `ChannelWindow.Name()` as the
channel's name from that point on, so the event log, the actor's
channel set and the broadcast events all carry one spelling however
the client asked. `Session.joinAs` returns that name, which is what
the JOIN reply carries back.

Names are checked against the RFC grammars before they are taken.
`domain.ValidateNick` implements RFC 2812 §2.3.1 up to
`domain.NickMaxLen`, and reserves `domain.AnonymousNick` because a
client holding it could impersonate the `+a` mask;
`domain.ValidateChannelName` implements §1.3 up to
`domain.ChannelNameMaxLen`, admitting both prefixes in
`domain.ChannelPrefixes`. Both return a rejection reason, which the
session wraps in
`domain.ErroneousNicknameError` (numeric 432) or
`domain.ErroneousChannelNameError` (479) with its own timestamp. 432
is the separate answer from `domain.NickInUseError` (433): 432 says
the nick could never be taken by anyone, 433 says it is taken now
and may be freed. `Session.requireNickAvailable` runs both checks in
that order and is the one path to claiming a nick, for `NICK` and
for `ADDMODEL`'s registration alike. `domain.ValidatePersona` follows
the same shape for a persona description, bound by
`domain.PersonaMaxLen` (400 characters) and refused with
`domain.ErroneousPersonaError` at `ADDMODEL` time. `domain.ValidateTopic`
bounds a channel topic by `domain.TopicMaxLen` (390 characters) and is
refused with `domain.ErroneousTopicError` at `TOPIC` time: RFC 1459
and RFC 2812 place no length limit on a topic, but TOPICLEN is the
convention modern ircds advertise via ISUPPORT, and this server
enforces it because a channel's topic is repeated into every dispatch
turn's prompt for that channel.

### Flood control

Flood control is the inbound counterpart to the send-queue bound, and
is separate from it. It has two halves, and both apply to every
client the same way.

Per connection, the RFC 1459 §8.10 penalty algorithm paces commands.
Each subscription has a message timer; `Session.Handle` charges two
seconds to it per command and holds a command back for as long as the
timer runs more than ten seconds ahead of now. A client may therefore
send five or six commands at once and one every two seconds after
that. The rule reads nothing about what kind of actor is sending: it
is a property of the connection, and a person typing never reaches
the threshold. Typing at human speed gains half a second per line;
autojoin restoring channels on connect is one multi-target JOIN per
chunk of `protocol.MaxJoinTargets` channels (RFC 2812 §3.2.1,
"JOIN #a,#b,#c"), and each chunk is one charge.
`protocol.MaxJoinTargets` is this server's TARGMAX (RFC 2812
ISUPPORT): a JOIN naming more channels than that is refused whole,
with `domain.TooManyJoinTargetsError`, before any channel in it is
joined, so one command's channel list cannot grow the writer loop's
per-command work without bound. `userclient.JoinAutojoinChannels`
chunks the autojoin list to that cap, and
`TestSession_autojoin_spends_the_user_allowance` pins the cost at
zero delay, both joining and on the first thing typed afterwards,
for the channel counts autojoin sees in practice. Two models
answering each other back to back are the case that does gain two
seconds a turn.

A held-back command is delayed, never dropped, which is what an ircd
does when it reads a flooding connection more slowly. The wait
happens on the sending client's own goroutine, before the command
reaches the command loop, so the loop is never held up by it. A
model-client waiting there is not draining its events channel, which
is the state it is already in mid-turn: peers sending to it still do
not block, and a backlog past `sendQAllowance` disconnects it through
the ordinary teardown. The wait also ends early on the client's
context, its subscription being reaped, or the loop stopping, so a
throttled client never delays its own teardown or shutdown.

A client crossing into a throttle is told: it receives a server
notice on its own delivery stream, once as the episode opens and not
once per held-back command. The episode ends only when the timer has
drained the whole way back to now, which takes as long as the client
spent building it up, so a client alternating between bursts and
short pauses is warned once and not once per burst.

The notice is filed nowhere. The instance reply log holds an issuer's
own lookup results, which a model replays as things it asked for and
still knows; a throttle was true for a few seconds, and a model
reading it back on every later turn would be told to slow down long
after it already had, at the cost of prompt space. What a client
carries forward from a throttle is the delay it experienced.

The send gates run in a fixed order, and the first of them is not a
mode at all: a channel target the server does not know is refused
with `domain.NoSuchChannelError`, RFC 2812 §3.3.1's 401
ERR_NOSUCHNICK for a PRIVMSG addressed to nowhere. It is 401 and not
403 ERR_NOSUCHCHANNEL, which §3.2.2 reserves for the commands that
act on a channel the client is in. Refusing at the gate is what keeps
the event log from carrying a row under a name no channel answers
to.

Per channel, mode `+f <messages>` caps how many messages the channel
relays in one flood window. It is parametric like `+l`, it is checked
last among the send gates so a message an earlier gate refuses does
not spend the budget, and going over it refuses the message with
ERR_CANNOTSENDTOCHAN (RFC 2812 numeric 404), the same shape `+m`,
`+n` and `+q` refuse with. The window is fixed rather than sliding,
so a channel can relay twice its limit across a boundary; the
per-connection pacing already bounds the rate such a burst can
arrive at. Both flood timers measure elapsed time on the monotonic
clock, never `Session.now`, which stamps domain events and is
commonly frozen. `domain.ParseChannelModes`, which reads
the `/config` default-channel-modes string, rejects `+f` along with
the other parametric modes: that grammar reads bare flags and has
nowhere to put a parameter, so a creation default for a flood limit
means extending it to the parameter form `ChannelModes.IRCString`
already renders.

`+f` needed no new arm in any mode parser. Each mode letter is
described once, in `domain`'s `channelModeSpecs`: what it takes
alongside it (`domain.ModeArgumentFor`), how to read its current
setting for rendering, and how to write a change into a
`domain.ChannelModes` (`ChannelModes.ApplyChannelMode`). The `MODE`
validator, the `/mode` argument parser, the broadcast renderer,
`IRCString` and `ParseChannelModes` all decide from that one
description, so a new mode is one entry there.

Together with `+m` and KILL, those are the primitives an operator or
a future policy layer composes a spend budget out of. Nothing in the
session decides a budget, and `dispatchTrigger` holds no hidden gate.

### Channel visibility

`Session.channelVisibleTo` is the one answer to "may this client be
told that this channel exists". A channel carrying neither `+s` nor
`+p` is visible to anyone; either flag hides it, and the two
exemptions are its own members and a server operator (RFC 2811
§4.2.5–§4.2.6, RFC 2812 §3.6.2). `LIST`, `WHOIS`'s channel list and
the `NAMES` reply all answer under it, so a channel cannot be hidden
from one and read straight back out of another. `+p` hides the
channel outright, exactly as `+s` does: RFC 2811 drew a finer
distinction, listing a `+p` channel with its topic suppressed, but
that leaks the one thing the mode exists to hide, and modern ircds
treat the two the same.

Membership is read from the issuing client's own channel set, which
is the client's own answer to a question about itself and costs no
channel load. `LIST` asks it once per directory row, so consulting
each channel's member list instead would turn one enumeration into
one lookup per channel.

That predicate is also what a whole-session view is made of. The
user-client already holds `+o`, so its `/list` and `/whois` see
everything without any bypass of the delivery filter, and no
separate inspector layer is needed.

### Anonymous channels

Mode `+a` (RFC 2811 §4.2.1) means no member may learn who anyone
else is. Three things follow, and all three happen at fan-out, where
the per-recipient channel intersection is already known:

- chat traffic is attributed to `domain.AnonymousNick` before
  delivery, while the stored event keeps the real origin for audit;
- a `QUIT` is delivered to the anonymous channel as a `PART` from
  the mask, so a member sees somebody leave the channel and cannot
  tell they left the server;
- `ModelDispatchStarted` / `ModelDispatchDone` lose their instance
  handle, which is the only thing in them that names anybody, so the
  thinking indicator says that something is happening without saying
  who is about to speak.

The last two are per recipient, not per event: a client that also
shares an ordinary channel with the actor already knows who it is
from there, and receives the `QUIT` and the named dispatch events
scoped to those channels.

`domain.AnonymousNick` is reserved by `domain.ValidateNick`, so no
client can take the mask and speak as it.

### Event bus

The session exposes one event channel per subscription: each
subscription's `Client.Events()` returns the per-client protocol bus.
The session fans out the broadcast events — the wire-shaped PRIVMSG,
JOIN, PART, TOPIC, MODE, NICK, KICK and QUIT, plus the session-emitted
`PokeEvent`, `ModelDispatchStarted` and `ModelDispatchDone`. The
point-to-point events — INVITE, `TopicInfo`, `NamesReplyEvent`/
`NamesEnd`, `Whois`, `ListReply`/`ListEnd` and `SystemNotice` — are
the numerics (341, 332/333, 353/366, 311–319, 322/323, and free-form
notices), addressed to one client by construction, and are delivered
to that client only. Every value the session emits implements
`domain.ProtocolEvent`, sealed via `isProtocolEvent()`.

Chat-screen-local control signals — `domain.ErrorEvent` wrapping a
backend error from a UI-issued command, and the `Help`, `UsageHint`,
`PersonasList` and `CommandError` events the chat-screen builds and
renders itself — flow as bare `tea.Msg` returns from the chat-screen's
own `tea.Cmd`s and reach the Update loop directly. The session never
puts them on the bus (`serverClient.canReceive` returns false for
them); it is not the courier for them.

`ErrorEvent.Target` and `CommandError.Target` name the window
`ChatScreen.handleErrorEvent` renders a command failure into, a
choice the chat-screen makes entirely on its own side. It carries no
bus-addressing meaning the way `Whois.Target` does: the session never
reads it, since an `ErrorEvent` never reaches the session at all.

`ChatScreen.fallbackTarget` gives every reply the chat-screen renders
the same answer, `Whois`, `Invited` and `SystemNotice` alongside the
failure: a reply naming a window the user has since closed renders in
the window they are looking at, and no closed window is reopened to
hold it. `&modeloff` is exempt and resolves to itself whether or not a
window is open for it, because it lives as long as the session and is
the one window `appendToScrollback` can create from the name alone. A
reply arriving while nothing is focused takes `logAndShow`'s answer,
`&modeloff` with the focus moved there.

### Echo gate and membership filter

`fanOutProtocol` skips the originator for `domain.Message` events
(PRIVMSG and `/me` actions): the base protocol provides no echo of a
client's own chat traffic, which is delivered to every member of the
target window except the sender. IRCv3 echo-message is the extension
that adds the echo. A subscription granted it (today only the
user-client, via `protocol.SubscribeOptions.EchoMessage`) receives a
direct echo of its own chat traffic back over the bus through
`Session.echoToOriginator`; a model holds no such capability and
keeps the no-self-echo rule.
Other event types — JOIN, PART, MODE, TOPIC, NICK, etc. — are
delivered to every member-subscriber including the originator. A
`PART` is broadcast while the departing actor is still a member, then
membership is dropped (RFC 2812 §3.2.2 order), so the actor receives
its own `PART`. `KICK` follows the same order for the same reason:
RFC 2812 §3.2.8 has the kicked client told it was kicked, and the
membership filter is what carries the event to it.

Every client — the user-client included — carries the same membership
filter via `serverClient.canReceive`: it sees events only for windows
it is in — channel: target-channel membership; DM: counterpart match;
actor-scoped (`Quit`, `NickChange`): any-channel-in-common with the
actor. The user-client is a member of whatever it has joined, so the
chat-screen renders exactly those windows. Server handshake numerics
(`Welcome`, `Reconnected`) and command replies reach the user-client
point-to-point — via `deliverToClient` or the issuing command's
`Response.Events` — not through this broadcast filter. A whole-session
view is the operator exemption inside `Session.channelVisibleTo`,
never an always-on bypass of this filter.

`NICK` is the one actor-scoped event with a point-to-point fallback.
RFC 2812 §3.1.2 has a client always told its own rename succeeded,
and a client on no channels has no channel for the broadcast to
reach it through, so `changeNickAs` delivers the `NickChange`
directly in that case.

### Operator capability

User-mode `+o` is requested via
`protocol.SubscribeOptions.InitialModes` when the user-client
attaches; the session writes the granting `domain.UserModeChange` as
the first event on the subscription's bus. The operator-gated
commands today are `protocol.Kill` and `protocol.AddModel`;
non-operator clients receive `domain.NotOperatorError` from the
dispatcher (RFC 2812 numeric 481, ERR_NOPRIVILEGES). A wire `OPER`
command (`protocol.Oper`, RFC 2812 §3.1.4) exists and is dispatched
by `handleOper`; its `OperAuthenticator` defaults to rejecting every
attempt, so credentialed operator promotion is a ready extension
point rather than a live capability. The user-client's `+o` is still
granted at attach via `InitialModes`, not acquired through `OPER`.

The `+o` override reaches privileges among a channel's members and
stops there. `Session.requireChannelOp` waives channel-op status for
an operator, so the user can `/kick` or `/mode` on a channel it
joined without picking up `@`. It does not make a non-member a
member: `TOPIC` requires being on the channel whatever the modes say
(RFC 2812 §3.2.4), and so does `INVITE`, since an invitation is a
member vouching for someone. `+i` controls who may be in the channel
at all, which is not one of the privileges `+o` waives, so an
operator's own `JOIN` is refused by it like anyone else's.

`ADDMODEL` is the one join that passes `+i` without an invitation.
It is the server placing a client on a channel at an operator's
request, the thing ircds that grew one call SAJOIN, and
`session.joinKind` is what distinguishes it from a client joining
itself. The command is operator-gated at the dispatcher, so the
authority the operator already exercised is what admits the new
client, and the channel's invitation list is left holding only
invitations somebody actually issued.

### Invitations

`domain.Invitations` is keyed by `domain.InstanceID`. This is a
deliberate departure from RFC 2812 §3.2.7, which describes the list
in terms of nicks because a nick is all the wire carries. A nick is
display state a client may change at will: keyed by nick, a client
invited as `botty` and then renamed loses the invitation it was
granted, and a second client taking the freed nick inherits it.
Keying by the immutable id makes an invitation belong to the client
it was issued to for as long as that client exists.

An invitation is single-use and is consumed by any successful join,
`+i` or not: a client that walked in while the channel was open is
not owed a second entry after `+i` goes up. Like every other piece
of channel state it dies with the channel (RFC 2811 §2).

The user's empty `InstanceID` sentinel is an ordinary key here, like
any other client's: `inviteAs` resolves the target through
`Session.resolveConnectedNick`, which answers for the user the same
way it answers for a model, so `/invite` can target the user and the
user's own `+i` `JOIN` consumes the invitation `checkJoinGates` reads
under `actor.ID()`.

### Slash commands and tool schemas

`/`-commands and model-callable tools share a single source of truth.
The `internal/ui/chatcmd` grammar declares each command as a Go struct
with `arg:`/`help:`/`tool:` tags; the `internal/command` package walks
the grammar at registration time and derives the OpenAI tool schema
(name, description, JSON-schema parameters) by reflection. When a
chatcmd struct implements `ToCommand(Context) (protocol.Command, error)`,
the same wire command flows whether the user typed `/foo` or a model
called the `foo` tool. Most commands implement `ToCommand` and so
have a wire counterpart: `/join`, `/part`, `/list`, `/add-model`,
`/invite`, `/kill`, `/kick`, `/msg`, `/nick`, `/mode`, `/topic`,
`/me`, `/whois`, and `/quit` (for example `ListCommand` returns
`protocol.List` and `WhoisCommand` returns `protocol.Whois`). The
remaining commands are purely UI-side, have no wire counterpart, and
do not implement `ToCommand`: `/config`, `/query`, `/personas`,
`/regenerate-personas`, `/help`, `/clear`, `/poke`, and the tool-only
`pass`.

`/close` (aliases `/wc`, `/unquery`) is the one command whose wire
counterpart depends on the window it is run in, so it has no
`ToCommand` of its own. In a channel window it dispatches
`PartCommand`'s PART, because a channel window exists only while the
user is in the channel, which is what `/wc` does in irssi and
`/close` in WeeChat. In a DM window it closes client state and never
reaches the wire. PART takes a channel and correctly refuses anything
else, which is what a query window needs in its place. In
`&modeloff` it refuses: that window is the client's own view of the
server and lives as long as the session.

The three memory tools (`write_memory`, `delete_memory`, `search_memory`)
are in-process operations rather than wire commands, and stay
hand-rolled in `internal/modelclient/memory_tools.go`.

### Persistence

Every connected client has a row in `instances`, its connection
record, and the empty `InstanceID` the user-client registers under is
an ordinary key there. `UserClient.Attach` writes the row before it
subscribes, which is the registration step `ADDMODEL` performs for a
model; from then on the session is the only writer of it, exactly as
it is for a model's. A join stamps the channel onto it, a `NICK`
rewrites the nick, and the QUIT teardown deletes it, which is what
frees the nick. The write is an upsert on a table that already
existed, so a database from before the row needs no migration: the
first attach creates it.

A row is not a connection. `Session.Subscribe` allocates the
subscription envelope to the client that asks for the identity, and
refuses a second client asking for the same one with
`session.ErrIdentityInUse`: the envelope's events channel has one
reader, and two goroutines receiving from it would take deliveries
from each other. `Manager.Start` asks the same question the other way
round, skipping any instance row the session already holds a client
for, so the boot-time attach of stored model instances leaves a
client that registered and connected before it alone. That is what
keeps `main.go`'s order (attach the user-client, then start the
manager) from putting two readers on one bus.

This is the connection record, not an account registry. Nothing about
it survives the QUIT that deletes it, and re-inviting a client that
quit would still need the services model described in point 6.2 of
the flow.

That row is what lets a channel record name the client in its member
list. `store.resolveChannelMembers` resolves every member id through
the registry, the empty one included, so a member entry keeps
resolving to the same `*domain.Instance` the client holds and the
pointer comparisons downstream keep matching. `Session.ResolveNick`
and `Session.ResolveInstanceByID` therefore answer for every client
the same way, out of the store, with no short-circuit for one of
them. `Session.Instances` returns every row, this client's included;
a caller that wants everybody but itself excludes its own identity,
which is a question only that caller can answer, and the chat-screen
does it for its completion sources.

A run that ends without a QUIT never runs the teardown, so the
persisted member lists still name the client. `Connect` classifies
that run as unclean from the `session_active` marker and
`cleanupUncleanShutdown` reconciles it: on the command loop, every
channel record naming this client loses the membership, and a channel
the departure empties is destroyed, which is what the departure would
have done in order (RFC 2811 §2). It reads the channel records rather
than the client's own channel set, because the client registered a
moment ago with an empty one.

The channel-keyed event log (`store.AppendEvent` /
`store.EventsBefore` / `store.DMEventsBefore`) is the server-side
record of channel history. A model-client loads it once at attach:
per channel, a single bounded read of the most-recent events
(`modelHistorySize`), kept to those at or after the instance's join
time — a channel with no known join time loads nothing (fail-closed).
From there the per-channel ring is appended live and read locally
each dispatch turn; the store is not re-read per turn. The chat-screen
does not read this log on focus changes — the in-memory scrollback
buffer captures only events the user has seen this session, up to
`components.ScrollbackLimit` per window, mirroring IRC's "you don't
see what happened before you joined" rule.

A model's DM windows have no attach-time list to load from, so each
is loaded the first time the client sees it, from
`store.DMEventsBefore` between the instance and the counterpart. The
load happens under the history lock as part of taking the turn's
snapshot, which is what makes it one step with the read it precedes:
a turn is never prompted from a window whose load has not run. Every
buffer, DM and channel alike, is keyed by the window
`domain.Message.RoutingKey` places an event in, so a DM is one buffer
holding both directions of the conversation and a model reads back
what it said itself.

The unread badge is answered by `Session.UnreadCount` against the
user's read cursor. A DM counts through `store.CountDMEventsFrom`,
over both directions of the thread, since counting the window's own
key would count only the lines the user sent. The cursors live in
two tables: `last_read` keys a channel's cursor and references
`channels(name)`, and `dm_last_read` keys a DM's cursor by the
counterpart's `InstanceID` with a cascade to `instances`, so a
deleted counterpart drops its cursor with it. `UserClient.MarkRead`
and `Session.UnreadCount` pick the table by the window key's kind.

Which DM windows the user has open is client state, and `dm_windows`
is where the client keeps it. A channel comes back on its own,
because it is on the autojoin list and the JOIN announces it. Nothing
on the wire reopens a DM window, so without a record the user returns
to a sidebar missing every conversation they had open. The user-client is
the only actor that touches the table (`UserClient.DMWindows` /
`OpenDMWindow` / `CloseDMWindow`); the session never reads or writes
it. The chat-screen records a window whenever it opens one and
forgets it on `/close`, and reopens the recorded set at bootstrap,
landing on one of them when that is the window the user left open.
The rows are keyed by the counterpart's `InstanceID` and cascade from
`instances`, like the DM cursors.

The autojoin list is client state on the same terms, and `autojoin` is
where the client keeps it. `UserClient.Send` rewrites it after each
JOIN, PART or KICK the client issues, from the channel set the command
loop has just settled, and writes it through
`userclient.Store.SetAutojoinChannels`; the session neither reads nor
writes the table. QUIT is deliberately not one of those commands:
ending a connection does not say the channels should stay behind, and
this list is what brings them back on the next one. A KICK naming
somebody else costs one write that changes nothing, which is cheaper
than resolving the target to find out.

The `session_active` marker is the other half of that record. The
session writes it during its connect handshake and reads it there to
classify the previous run; `UserClient.Quit` clears it once the QUIT
has gone through. A run that ended without a QUIT therefore leaves the
marker standing, and the next connect reconciles the memberships it
left behind through `cleanupUncleanShutdown`.

An issuer's own point-to-point replies (`WHOIS`, `LIST`, and the
`domain.SystemNotice` a refused `INVITE` or a fallen-short `ADDMODEL`
preparation answers with) are not channel activity, so they live in a
private per-instance reply log
(`store.AppendInstanceReply` / `store.InstanceRepliesBefore`), keyed
by the issuer's identity, not the shared channel log. A model files
the same set into its own in-memory replies ring as the reply comes
back, so it meets a refusal on the turn that caused it and not only
after a reattach reloads the log. Both actors
write their replies the same way: the dispatcher records every
issuer's reply there, the user-client included (under the empty id).
The two actors differ only in whether they restore — a model loads
its log once at attach, merging its own replies chronologically into
the prompt transcript so it re-experiences its lookups across turns
and reattach, as if its quit never happened; the user is transient
and never restores, so its entries are the durable record a future
restore or inspector would read, and the user sees nothing of them
again this session. Another model in the channel never sees a peer's
replies. The dispatcher also stamps the issuing window onto a reply it
owns: `handleWhois` sets `domain.Whois.Target` to the window the
`WHOIS` was issued from, so the issuer renders the reply where it
asked. Numerics and UI notices the chat-screen raises locally (help,
usage hints, system notices) stay out of the channel log entirely:
they render to the in-memory scrollback only, so the shared channel
log a model loads holds nothing but genuine channel activity.

### Out of scope, design accommodates

- The remaining tool-surface protocol-routing cleanup: the `topic`
  tool still reads the current topic through `GetWindow`, and the
  chat-screen's `/msg` and `/query` still resolve a nick client-side
  to materialise the DM window they open. Both reach the session
  through the narrow `SessionReader` the chat-screen holds; routing
  them through the protocol is a follow-up. Message targets do not go
  this way; see Message targets above.
- Bootstrap-time, `joined_at`-scoped replay of recent events into a
  newly-allocated subscription, replacing the per-dispatch store read
  and the model-client's eager seed. History replay is the IRCv3
  `chathistory` capability, granted at attach via `SubscribeOptions`;
  the user-client simply does not request it today.
- A `WHO` command, which would answer under the same
  `Session.channelVisibleTo` predicate LIST, WHOIS and NAMES already
  do.
- Credentialed operator promotion through `OPER`, backed by a real
  `OperAuthenticator`.
- A user-restore-history feature that, on reconnect, routes each
  persisted wire-command reply back to its source window.
  `Whois.Target` already carries that window; `ListReply` and
  `ListEnd` would need an issuing-window field added at that point,
  since `RPL_LIST` carries no addressable target on the wire today.

## External libraries

- [Bubble Tea] for the TUI framework.
- [lipgloss] for styling the TUI.
- [`openai-go`] for interacting with the OpenRouter API (it is OpenAI
  compatible).

For [OpenRouter-specific features][openrouter] such as listing models, call the
API directly.

[Bubble Tea]: https://github.com/charmbracelet/bubbletea
[`openai-go`]: https://github.com/openai/openai-go
[lipgloss]: https://github.com/charmbracelet/lipgloss
[openrouter]: https://openrouter.ai/openapi.yaml

## Bubble Tea coding standards

It's really important that we maintain a clean internal architecture. For an
application like this, that means _discipline_ around our backend and frontend.

We should always be strongly typed. Don't work with bare strings wherever
possible. There must be a principled layer where the backend and frontend
communicate and UI concerns should not leak into the backend.

For the TUI itself, follow the "tree of models" approach. There should be a main
router model which holds the top-level state and handles _very few_ other
concerns. It knows which screen is active and simply routes messages to it.
Those screens can themselves be routers, all the way down as far as needed.

The UI works _with_ the Bubble Tea framework and communicates with Tea messages
and commands. It does not work around this.

Components must ALWAYS render responsively in the available space. There are
NEVER hardcoded dimensions. For this to work, models need to know their size. So
our models have an interface of:

```go
type Model interface {
    // Init is called when the model is first created. It can return an initial
    // command to run.
    Init() tea.Cmd

    // Update is called when a message is sent to the model. It returns the
    // updated model and an optional command to run.
    Update(msg tea.Msg) (Model, tea.Cmd)

    // View returns the string representation of the model, which will be
    // rendered in the UI.
    View(width, height int) string
}
```

which is almost identical to the standard Bubble Tea interface, except the
`View` method takes the available width and height as parameters. This way, we
can ensure that all models render responsively. The very root model keeps track
of the application's overall size and passes it down to all child models.

With this, and with good use of `lipgloss` utilities like `Height`, `Width`, to
to calculate actual rendered dimensions, we can ensure that the UI renders
properly at _any_ size.

### Write components freely

The aim is a consistent UX, an application that feels like it was designed as
one whole system. For that to work, we need reusable components and models. So
create these freely. Models should be _small_. If it's getting big, it's time to
split now. Don't delay.

## Design system

Create a design system using a small number of Lipgloss styles. Use this
throughout, instead of hardcoding colours.

Use the ANSI colours so the user's terminal theme is respected.

## Testing

- Use DI and fakes or endpoint configurability so that we can test the OpenAI
  and OpenRouter endpoints.
- Test everything, including the UI. Use [`teatest`] for this.
- Prefer table tests where possible.

[`teatest`]: https://github.com/charmbracelet/x/tree/main/exp/teatest
