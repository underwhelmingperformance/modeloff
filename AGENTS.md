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
Command) (Response, error)`, `Events() <-chan Event`, `HasMode(UserMode) bool`.
A `Send` returns a `Response` whose `Err` field carries any typed command
failure (e.g. `domain.NotOperatorError`, `domain.UnknownNickError`); callers
branch on it via `errors.As`. The `Response.Events` slot is reserved for
synchronous numeric-reply payloads but is currently unused — see Out of scope.
Broadcast side effects flow asynchronously over `Client.Events()` to peers.

### Two client kinds

- The user-client is a singleton with the same lifetime as the session.
  It is constructed in `Session.New`, granted operator mode at
  construction, and exposed via `Session.User()`. Its `Identity()` is
  the sentinel `protocol.UserClientID` (the empty `ClientID`).
- A model-client lives in the `internal/modelclient` package. It owns
  the dispatch goroutine, the per-channel history ring buffer used
  for prompt assembly, the OpenRouter `api.Client` it calls, and the
  memory-tool registry. `ModelClient.Attach` registers with the
  session via the public `Session.Subscribe(c, opts)` API and
  returns a `protocol.Subscription`; `Detach` reverses both. The
  dispatch goroutine watches the subscription's `Events` and `Done`
  channels and runs an LLM turn when `dispatchTrigger` says so (a
  message in a window the instance shares, a join/part/invite that
  addresses it, a poke).

Model-client lifecycle is mediated by a `session.ModelClientFactory`
the binary supplies at `session.New` time. `attachInstanceToChannel`
asks the factory to attach when an instance joins a channel; the
`KILL` and `QUIT` handlers ask the factory to detach so the
dispatch goroutine joins deterministically. The factory's owning
session arrives as a parameter on each `Attach` call so the
factory can be constructed before the session it serves and the
two can be assembled in one expression.

### Dispatcher

`Session.Handle(client, cmd)` is the single entry point for any model
action. The dispatcher's exhaustive switch over `protocol.Command` resolves
the issuing actor via `resolveClientActor`, runs an operator-mode check
where required, then delegates to a per-command implementation in the
`session` package. The actor surface (`joinAs`, `partAs`, `sendMessageAs`,
…) is unexported: outside the package, the only way to reach it is
through `Send → Handle`.

`AddModel`, `Quit`, and `Kill` currently return `errNotYetImplemented`
from the dispatcher. The chatcmd entry points for `AddModel` and `Quit`
still call into legacy public methods on the session
(`Session.AddModel`, `Session.QuitAs`) and retire alongside the dispatcher
fills.

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
delivered to every member of the target window except the sender.
Other event types — JOIN, PART, MODE, TOPIC, NICK, etc. — are
delivered to every member-subscriber including the originator.

Each model-client carries a membership filter via `serverClient.canReceive`:
it only sees events for windows it is in — channel: target-channel
membership; DM: counterpart match; actor-scoped (`Quit`, `NickChange`):
any-channel-in-common with the actor. The user-client receives every
protocol event; the chat-screen renders the entire session and needs
the full feed.

### Operator capability

User-mode `+o` is granted to the user-client at construction. The
operator-gated commands today are `protocol.Kill` and `protocol.AddModel`;
non-operator clients receive `domain.NotOperatorError` from the
dispatcher (RFC 2812 numeric 481, ERR_NOPRIVILEGES). There is no wire
`OPER` command:
modeloff is a single-user app, and operator mode is set at construction
rather than acquired through a credential exchange. A future revision
that introduces credentialed operator promotion would extend the command
sum without changing the dispatcher's mode-check shape.

### Slash commands and tool schemas

`/`-commands and model-callable tools share a single source of truth.
The `internal/ui/chatcmd` grammar declares each command as a Go struct
with `arg:`/`help:`/`tool:` tags; the `internal/command` package walks
the grammar at registration time and derives the OpenAI tool schema
(name, description, JSON-schema parameters) by reflection. When a
chatcmd struct implements `ToCommand(Context) (protocol.Command, error)`,
the same wire command flows whether the user typed `/foo` or a model
called the `foo` tool. Some `/`-commands (`/help`, `/clear`, `/list`,
`/whois`) are UI-side or session-side without a wire counterpart, and
do not implement `ToCommand`.

The three memory tools (`write_memory`, `delete_memory`, `search_memory`)
are in-process operations rather than wire commands, and stay
hand-rolled in `internal/session/tools.go`.

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

### Out of scope, design accommodates

- Synchronous numeric-reply payloads (e.g. `RPL_LIST` / `RPL_LISTEND`
  for `protocol.List`, `RPL_WHOISUSER` for `protocol.Whois`) populating
  `Response.Events`. Today the dispatcher's `List`/`Whois` handlers
  discard these and the chatcmd-side path emits the events directly;
  the dispatcher fill collapses both into a single `Response.Events`
  flow.
- Opt-in user-side log replay (mirroring how models read pre-join
  history) is a future toggle, not a current behaviour.
- `KILL` implementation will need the dispatcher's handler plus
  permanent removal from the subscriber set.
- `AddModel` and `Quit` dispatcher fills retire the legacy
  chatcmd-direct paths.
- Bootstrap-time replay of recent events into newly-allocated
  subscriptions, replacing the per-dispatch store read, is tracked
  separately — and `joined_at` scoping for that replay if it becomes
  the desired shape.

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
