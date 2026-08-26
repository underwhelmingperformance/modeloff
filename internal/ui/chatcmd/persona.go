package chatcmd

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

const personaUsage = "/persona <nick> " +
	"[<description...> | --reset | --rollback <revision>]"

// PersonaCommand represents `/persona <nick>` and everything an operator does
// to an instance's persona. The bare form shows the persona in force and the
// history behind it; a form carrying text writes a new revision, the way
// `/topic` shows a topic or sets one. `--reset` and `--rollback` put a
// revision that already exists back in force.
//
// Each of the three puts a different revision in force, so naming more than
// one is refused.
//
// Rollback is a pointer so that supplying a revision is distinct from
// supplying none. Revision ids start at one, so a zero read as an absent flag
// would make `--rollback 0` an inspection and let `--reset --rollback 0` past
// the check that refuses two of them at once.
//
// Description carries `passthrough:"partial"` so an unrecognised flag is
// refused before it can be read as text. Under `passthrough:"all"` a mistyped
// `--reste` would pass through and become part of the instance's
// personality; both modes keep recognised flags active until the passthrough
// takes its first value, so `--reset` and `--rollback` parse either way and
// the unknown-flag case is the whole difference.
//
// The grammar gives this command no `tool:` tag, so no tool is derived from
// it and no model can call it. That separation is what the persona
// arrangement rests on: an instance's only path into its own character is
// reflection, which cites evidence and lands a revision an operator can roll
// back.
type PersonaCommand struct {
	Nick        string                    `arg:"" optional:"" help:"Instance nick"`
	Description *CommandText              `arg:"" optional:"" passthrough:"partial" help:"Replacement persona description"`
	Reset       bool                      `optional:"" help:"Reset to revision zero"`
	Rollback    *domain.PersonaRevisionID `optional:"" help:"Roll back to an ancestor revision"`
}

// Sources implements command.Completer.
func (PersonaCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// Run implements Command.
func (c PersonaCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if strings.TrimSpace(c.Nick) == "" || c.actionCount() > 1 {
		return usageCmd("persona", personaUsage)
	}
	if c.Rollback != nil && *c.Rollback <= 0 {
		return usageCmd("persona", personaUsage)
	}

	nick := domain.Nick(c.Nick)
	return func() tea.Msg {
		var inspection domain.PersonaInspection
		var err error
		action := PersonaInspected

		switch {
		case c.Description != nil:
			action = PersonaDescribed
			inspection, err = rc.Manager.SetInstancePersona(
				ctx, nick, c.Description.String(),
			)
		case c.Reset:
			action = PersonaReset
			inspection, err = rc.Manager.ResetPersona(ctx, nick)
		case c.Rollback != nil:
			action = PersonaRolledBack
			inspection, err = rc.Manager.RollbackPersona(ctx, nick, *c.Rollback)
		default:
			inspection, err = rc.Manager.InspectPersona(ctx, nick)
		}
		if err != nil {
			return rc.errorResult("persona", err)
		}

		return PersonaResult{Action: action, Inspection: inspection}
	}
}

// actionCount reports how many of the three revision actions the command
// names. Each puts a different revision in force, so more than one is a
// contradiction.
func (c PersonaCommand) actionCount() int {
	actions := 0
	if c.Description != nil {
		actions++
	}
	if c.Reset {
		actions++
	}
	if c.Rollback != nil {
		actions++
	}

	return actions
}
