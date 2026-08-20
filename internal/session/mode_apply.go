package session

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// applyChannelModeChangesAs is the entry for [protocol.ChannelMode].
// It loads the channel window, checks the actor's channel-op status
// once for the whole batch, validates every change's shape up front,
// then applies them in order. Up-front validation rejects the whole
// batch on a malformed entry so a `MODE` with a typo never half-
// applies. A runtime failure (e.g. unknown nick on `+o`) stops the
// loop and returns the error; already-applied changes remain,
// matching typical ircd behaviour.
func (s *Session) applyChannelModeChangesAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, changes []protocol.ChannelModeChange) error {
	return s.inSpan(ctx, "session.apply_channel_mode_changes", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor.Nick())),
		attribute.Int("mode.change_count", len(changes)),
	}, func(ctx context.Context, _ trace.Span) error {
		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		ch = window.Name()

		if err := s.requireChannelOp(actor, window, "MODE", ch); err != nil {
			return err
		}

		for _, change := range changes {
			if err := validateChannelModeChange(change, s.now()); err != nil {
				return err
			}
		}

		for _, change := range changes {
			if change.Flag.MemberMode() {
				if err := s.setMemberModeAs(ctx, window, ch, actor, change); err != nil {
					return err
				}
				continue
			}

			if err := s.setChannelAttributeAs(ctx, window, ch, actor, change); err != nil {
				return err
			}
		}

		return nil
	})
}

// validateChannelModeChange checks one change against what its flag
// takes alongside it ([domain.ModeArgumentFor]): a member mode needs
// a nick target; a count mode needs a positive integer on add; a
// text mode needs a non-empty string on add; a boolean mode needs
// nothing. The remove form of a parametric mode needs no parameter,
// because it clears the setting whatever the setting was. A flag
// this build does not know is rejected.
func validateChannelModeChange(change protocol.ChannelModeChange, now time.Time) error {
	switch domain.ModeArgumentFor(change.Flag) {
	case domain.ModeArgNick:
		if change.Target == "" {
			return domain.MissingModeParamError{Flag: change.Flag, At: now}
		}

	case domain.ModeArgCount:
		if change.Add {
			n, err := strconv.Atoi(change.Param)
			if err != nil || n <= 0 {
				return domain.MissingModeParamError{Flag: change.Flag, At: now}
			}
		}

	case domain.ModeArgText:
		if change.Add && change.Param == "" {
			return domain.MissingModeParamError{Flag: change.Flag, At: now}
		}

	case domain.ModeArgNone:
		// A boolean flag has nothing to check.

	case domain.ModeArgUnknown:
		return domain.UnknownModeFlagError{Flag: change.Flag, At: now}
	}

	return nil
}

// setMemberModeAs applies a member-mode change (`+o`/`+v` add or
// remove) to `change.Target`'s entry in `window.Members`, persists
// the window, and emits a [domain.ChannelModeChange] to channel peers.
// The change touches only the named privilege, so a member holding
// both `@` and `+` keeps whichever one the change does not name (RFC
// 2811 §4.1). Called from [applyChannelModeChangesAs] after up-front
// validation, so the shape invariants are already enforced.
//
// `change.Target` resolves through [Session.resolveConnectedNick],
// the same registry INVITE, KICK, WHOIS and KILL resolve a nick
// through, so a grant reaches whoever currently holds the nick and
// not a row nothing is attached under.
func (s *Session) setMemberModeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) error {
	return s.inSpan(ctx, "session.set_member_mode", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(change.Target)),
		attribute.String("mode.flag", string(change.Flag)),
		attribute.Bool("mode.add", change.Add),
	}, func(ctx context.Context, _ trace.Span) error {
		target, err := s.resolveConnectedNick(change.Target)
		if err != nil {
			return err
		}

		window.Members.ApplyMode(target, change.Flag, change.Add)

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.persistAndEmit(ctx, ch, domain.ChannelModeChange{
			Target:       ch,
			Nick:         target.Nick(),
			InstanceID:   target.ID(),
			Flag:         change.Flag,
			Add:          change.Add,
			By:           actor.Nick(),
			ByInstanceID: actor.ID(),
			At:           s.now(),
			Instance:     target,
		})

		return nil
	})
}

// setChannelAttributeAs applies an attribute-mode change to the
// channel's `Modes` field, persists the window, and emits a
// [domain.ChannelModeChange] to peers. Called from
// [applyChannelModeChangesAs] after validation, so a parametric
// mode already has a valid `change.Param`: a positive integer for
// `+l` and `+f`, a non-empty key for `+k`.
func (s *Session) setChannelAttributeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) error {
	return s.inSpan(ctx, "session.set_channel_attribute", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String("mode.flag", string(change.Flag)),
		attribute.Bool("mode.add", change.Add),
	}, func(ctx context.Context, _ trace.Span) error {
		window.Modes.ApplyChannelMode(change.Flag, change.Add, change.Param)

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.persistAndEmit(ctx, ch, domain.ChannelModeChange{
			Target:       ch,
			Flag:         change.Flag,
			Add:          change.Add,
			Param:        attributeEmitParam(change),
			By:           actor.Nick(),
			ByInstanceID: actor.ID(),
			At:           s.now(),
		})

		return nil
	})
}

// attributeEmitParam returns the parameter to include on the
// broadcast [domain.ChannelModeChange] for an attribute change.
// Only the add form of a parametric mode has one: a boolean mode
// never takes a parameter, and the remove form of a parametric mode
// clears the setting without naming it.
func attributeEmitParam(change protocol.ChannelModeChange) string {
	if !change.Add {
		return ""
	}

	switch domain.ModeArgumentFor(change.Flag) {
	case domain.ModeArgCount, domain.ModeArgText:
		return change.Param
	}

	return ""
}
