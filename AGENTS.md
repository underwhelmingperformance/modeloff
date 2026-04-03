# `modeloff`

An old school IRC-style interface but for one user talking to multiple agents.

## Services

There is no server component. This uses the OpenRouter API.

## The flow

1. Start the application. A cool IRC-style connection sequence appears.
   1. If there's no OpenRouter API key configured, the user is prompted to use
      `/config` to set it up. The app won't work until this is done.
2. Any chats from last time are loaded and shown in the side bar. The window
   that was open last time is opened again.
   1. If there are no chats, a welcome message is shown.
3. The user can `/join` a chat room (`#`-prefix like IRC), or use shortcuts like
   ctrl-d,ctrl-u,ctrl-o to navigate in the sidebar (or the mouse).
   1. If the room doesn't exist, it is created.
   2. The user can have multiple rooms open at once.
4. The user can `/leave` a chat room.
   1. Leaving a room doesn't delete it.
5. The user can `/list` all chat rooms.
6. The user can `/invite` models to add them to the chat, and `/kick` them to
   remove them.
   1. The user can specify a model by name or ID, and the app will look it up
      using the OpenRouter API. If no name or ID is given, the user is prompted
      to select from a list of models or existing instances.
   2. When an existing instance is invited, a memory system is used so that it
      remembers previous conversations.
7. The user can `/msg` to DM a model, which is shown similarly to a channel
   except no `#` prefix.
8. From then on, it's a chat room. All events are broadcast to all models.
   They can reply or not. Measures should be taken to prevent infinite
   conversations which would become costly very quickly.
9. A channel can have a `/title`, which is shown in the UI. This is optional but
   it will be sent to the model as part of its prompt.
10. A small model such as Haiku should be used to give each invited model a
    nickname.
11. `/whois` can be used on a nickname to show metadata, and common channels.
12. On a random (perturbed a bit) configurable (via `/config`) schedule, the
    model instances are poked to see if they want to say anything, so that
    channels don't go dead.
13. Models can be given a persona when they're instansiated.
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
