# `modeloff`

An old school IRC-style interface but for one user talking to multiple agents.

## Services

There is no server component. This uses the OpenRouter API.

## The flow

1. Start the application. A cool IRC-style connection sequence appears.
   1. If there's no OpenRouter API key configured, the user is prompted to use
      `/config` to set it up. The app won't work until this is done.
2. Any channels from last time are loaded and shown in the sidebar. The
   channel that was open last time is opened again.
   1. If there are no channels, a welcome message is shown.
3. The user can `/join` a channel (`#`-prefix like IRC), or use shortcuts
   like ctrl-d,ctrl-u,ctrl-o to navigate in the sidebar (or the mouse).
   1. If the channel doesn't exist, it is created.
   2. The user can have multiple channels open at once.
4. The user can `/part` a channel.
   1. Parting a channel doesn't delete it.
5. The user can `/list` all channels.
6. The user can `/invite` models to add them to the channel, and `/kick`
   them to remove them.
   1. The user can specify a model by name or ID, and the app will look it up
      using the OpenRouter API. If no name or ID is given, the user is prompted
      to select from a list of models or existing instances.
   2. When an existing instance is invited, a memory system is used so that it
      remembers previous conversations.
7. The user can `/msg` to DM a model, which is shown similarly to a channel
   except no `#` prefix.
8. From then on, it's a channel. All events are broadcast to all models.
   They can reply or not. Measures should be taken to prevent infinite
   conversations which would become costly very quickly.
9. A channel can have a `/topic`, which is shown in the UI. This is optional
   but it will be sent to the model as part of its prompt.
10. A small model such as Haiku should be used to give each invited model a
    nickname.
11. `/whois` can be used on a nickname to show metadata, and common channels.
12. On a random (perturbed a bit) configurable (via `/config`) schedule, the
    model instances are poked to see if they want to say anything, so that
    channels don't go dead.
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
3. There should be a per-instance (keyed by nick) memory system so that the
   model can remember what's happened to it. This should be exposed as a tool so
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
Command) (Response, error)`, `Events() <-chan Event`, `Caps()
command.CapabilityHolder`. Operator gating reads the live `serverClient` mode
set keyed by `Identity`, so an `Oper` elevation is honoured without the client
object changing. A `Send` returns a `Response` whose `Err` field carries any
typed command failure (e.g. `domain.NotOperatorError`,
`domain.UnknownNickError`); callers branch on it via `errors.As`. The
`Response.Events` slot carries the dispatcher's synchronous numeric-reply
payloads: the persisted `domain.Message` for `PrivMsg`/`Action`,
`domain.ModelInvited` for `Invite`, the `domain.Whois` snapshot for `Whois`,
and the `domain.ListReply` stream terminated by `domain.ListEnd` for `List`.
Broadcast side effects flow asynchronously over `Client.Events()` to peers.

### Two kinds of actor

The user-client and the model-clients are uniform on the protocol — same
`Send → Handle`, same event types, same dispatcher — but they are deliberately
different kinds of actor in their lifecycle and the capabilities granted at
attach.

A model is a persistent inhabitant. The fiction the app maintains is that the
server kept running while the user was away, so a model returns with its context
intact. The persisted event log (`store.EventsBefore` / `store.DMEventsBefore`)
is the server's memory of channel activity; on (re)attach a model restores its
context from it. The user is a transient client: by IRC convention it sees live
traffic forward and nothing from before it connected, so it gets no history
replay — the chat-screen's scrollback is populated purely from live events.

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
  opts)` API and returns a `protocol.Subscription`; `Detach`
  reverses both. The dispatch goroutine watches the subscription's
  `Events` and `Done` channels and runs an LLM turn when
  `dispatchTrigger` says so (a message in a window the instance
  shares, a join/part/invite that addresses it, a poke). Each turn
  re-reads the api client through the getter so a manager-driven
  `SetAPIKey` rebuild propagates without reattach.

The LLM-side state — the api client and its rebuild factory, the
persona pool, the small-model id used for nick generation, the
catalogue cache, and the per-instance model-client registry —
lives in the `internal/modelmanager` package. A `*Manager`
satisfies `session.ModelClientFactory`: the session's
`attachInstanceToChannel` asks the manager to construct a
model-client when an instance joins a channel, `KILL` / `QUIT`
ask it to detach, and `ADDMODEL` asks it for persona arbitration
and a unique nick via `PrepareInstance`. The chatcmd and chat-
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

DMs have no wire-level "open" command. A direct message is just a
`PrivMsg` whose target is the counterpart's `InstanceID`; either party
can send and the events log carries the conversation under that key.
The chat-screen's `/query` is a UI affordance only — the session
never sees it.

### Event bus

The session exposes one event channel per subscription: each
subscription's `Client.Events()` returns the per-client protocol bus.
The session fans out wire-shaped events (PRIVMSG, JOIN, PART, TOPIC,
MODE, NICK, INVITE, KICK, QUIT) plus session-emitted events
(`FocusChannelEvent`, `TopicInfo`, `NamesReplyEvent`, `PokeEvent`,
`DispatchStartedEvent`, `DispatchDoneEvent`, `CommandError`,
`UsageHint`, `PersonasList`, `Help`, `Whois`, `ListReply`, `ListEnd`,
`SystemNotice`). Every value the session emits implements
`domain.ProtocolEvent`, sealed via `isProtocolEvent()`.

Chat-screen-local control signals (e.g. `domain.ErrorEvent` wrapping
a backend error from a UI-issued command) flow as bare `tea.Msg`
returns from the chat-screen's own `tea.Cmd`s and reach the Update
loop directly. The session is not the courier for them.

### Echo gate and membership filter

`fanOutProtocol` skips the originator for `domain.Message` events
(PRIVMSG and `/me` actions), per RFC 2812 §3.3.1: chat traffic is
delivered to every member of the target window except the sender. A
subscription granted IRCv3 echo-message (today only the user-client,
via `protocol.SubscribeOptions.EchoMessage`) then receives a direct
echo of its own chat traffic back over the bus through
`Session.echoToOriginator`; a model holds no such capability and
keeps the no-self-echo rule.
Other event types — JOIN, PART, MODE, TOPIC, NICK, etc. — are
delivered to every member-subscriber including the originator. A
`PART` is broadcast while the departing actor is still a member, then
membership is dropped (RFC 2812 §3.2.2 order), so the actor receives
its own `PART`.

Every client — the user-client included — carries the same membership
filter via `serverClient.canReceive`: it sees events only for windows
it is in — channel: target-channel membership; DM: counterpart match;
actor-scoped (`Quit`, `NickChange`): any-channel-in-common with the
actor. The user-client is a member of whatever it has joined, so the
chat-screen renders exactly those windows. Server handshake numerics
(`Welcome`, `Reconnected`) and command replies reach the user-client
point-to-point — via `deliverToClient` or the issuing command's
`Response.Events` — not through this broadcast filter. A whole-session
"god's-eye" view, if wanted, is an explicit request-driven inspector
layered on top (see Out of scope), never an always-on bypass.

### Operator capability

User-mode `+o` is requested via
`protocol.SubscribeOptions.InitialModes` when the user-client
attaches; the session writes the granting `domain.ModeChange` as
the first event on the subscription's bus. The operator-gated
commands today are `protocol.Kill` and `protocol.AddModel`;
non-operator clients receive `domain.NotOperatorError` from the
dispatcher (RFC 2812 numeric 481, ERR_NOPRIVILEGES). A wire `OPER`
command (`protocol.Oper`, RFC 2812 §3.1.4) exists and is dispatched
by `handleOper`; its `OperAuthenticator` defaults to rejecting every
attempt, so credentialed operator promotion is a ready extension
point rather than a live capability. The user-client's `+o` is still
granted at attach via `InitialModes`, not acquired through `OPER`.

### Slash commands and tool schemas

`/`-commands and model-callable tools share a single source of truth.
The `internal/ui/chatcmd` grammar declares each command as a Go struct
with `arg:`/`help:`/`tool:` tags; the `internal/command` package walks
the grammar at registration time and derives the OpenAI tool schema
(name, description, JSON-schema parameters) by reflection. When a
chatcmd struct implements `ToCommand(Context) (protocol.Command, error)`,
the same wire command flows whether the user typed `/foo` or a model
called the `foo` tool. `/list` and `/whois` implement `ToCommand`
(returning `protocol.List` / `protocol.Whois`); only the purely
UI-side `/help` and `/clear` have no wire counterpart and do not
implement `ToCommand`.

The three memory tools (`write_memory`, `delete_memory`, `search_memory`)
are in-process operations rather than wire commands, and stay
hand-rolled in `internal/modelclient/memory_tools.go`.

### Persistence

The channel-keyed event log (`store.AppendEvent` /
`store.EventsBefore` / `store.DMEventsBefore`) is the server-side
record of channel history. Each model dispatch turn reads from it via
`dispatchHistoryFor` to assemble the LLM prompt. The chat-screen does
not read this log on focus changes — the in-memory scrollback buffer
captures only events the user has seen this session, mirroring IRC's
"you don't see what happened before you joined" rule. Models see up
to 500 most-recent events from the channel log on each dispatch turn;
today this includes events that pre-date the instance's join.

A model's own point-to-point replies (`WHOIS`, `LIST`) are not channel
activity, so they live in a private per-instance reply log
(`store.AppendInstanceReply` / `store.InstanceRepliesBefore`), not the
shared channel log. The dispatcher records a model issuer's reply
there; a user issuer's is live-only, since the user is transient. Each
dispatch turn merges the instance's own replies chronologically into
its prompt transcript, so a model re-experiences its lookups across
turns and reattach — as if its quit never happened — while another
model in the channel never sees them.

### Out of scope, design accommodates

- The remaining tool-surface protocol-routing cleanup: the model tool
  path still resolves nicks client-side (`ResolveNick`) where the
  dispatcher already resolves them server-side, reads the current topic
  through `GetWindow`, and the chat-screen holds a concrete
  `*session.Session` for its own command-reply renders. Routing those
  through the protocol — and dropping the concrete session from the
  chat-screen — is a follow-up.
- Bootstrap-time, `joined_at`-scoped replay of recent events into a
  newly-allocated subscription, replacing the per-dispatch store read
  and the model-client's eager seed. Replay is for model-clients only;
  the user-client sees live traffic forward by design.
- A request-driven "god's-eye" inspector letting the user peek into a
  window or actor's vantage it is not a member of — the supported
  route to a whole-session view, layered on top of the membership
  filter.
- Credentialed operator promotion through `OPER`, backed by a real
  `OperAuthenticator`.

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
