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

// validateChannelModeChange enforces the per-flag shape rules:
// member modes take a nick target and no param; parametric
// attribute modes (`+l`, `+k`) take a param on add only; boolean
// attribute modes take neither. Unknown flags reject.
func validateChannelModeChange(change protocol.ChannelModeChange, now time.Time) error {
	switch change.Flag {
	case domain.ModeOperator, domain.ModeChannelVoice:
		if change.Target == "" {
			return domain.MissingModeParamError{Flag: change.Flag, At: now}
		}

	case domain.ModeUserLimit:
		if change.Add {
			n, err := strconv.Atoi(change.Param)
			if err != nil || n <= 0 {
				return domain.MissingModeParamError{Flag: change.Flag, At: now}
			}
		}

	case domain.ModeKey:
		if change.Add && change.Param == "" {
			return domain.MissingModeParamError{Flag: change.Flag, At: now}
		}

	case domain.ModeAnonymous, domain.ModeInviteOnly, domain.ModeModerated,
		domain.ModeNoExternal, domain.ModePrivate, domain.ModeQuiet,
		domain.ModeSecret, domain.ModeTopicLock:
		// boolean attribute mode, no param required

	default:
		return domain.UnknownModeFlagError{Flag: change.Flag, At: now}
	}

	return nil
}

// setMemberModeAs applies a member-mode change (`+o`/`+v` add or
// remove) to `change.Target`'s entry in `window.Members`, persists
// the window, and emits a [domain.ChannelModeChange] to channel peers.
// Called from [applyChannelModeChangesAs] after up-front
// validation, so the shape invariants are already enforced.
func (s *Session) setMemberModeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) error {
	return s.inSpan(ctx, "session.set_member_mode", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(change.Target)),
		attribute.String("mode.flag", string(change.Flag)),
		attribute.Bool("mode.add", change.Add),
	}, func(ctx context.Context, _ trace.Span) error {
		target, err := s.ResolveNick(ctx, change.Target)
		if err != nil {
			return err
		}

		nickMode := domain.NickModeFor(change.Flag, change.Add)
		window.Members.SetMode(target, nickMode)

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.persistAndEmit(ctx, ch, domain.ChannelModeChange{
			Target:     ch,
			Nick:       target.Nick(),
			InstanceID: target.ID(),
			Flag:       change.Flag,
			Add:        change.Add,
			By:         actor.Nick(),
			At:         s.now(),
			Instance:   target,
		})

		return nil
	})
}

// setChannelAttributeAs applies an attribute-mode change to the
// channel's `Modes` field, persists the window, and emits a
// [domain.ChannelModeChange] to peers. Called from
// [applyChannelModeChangesAs] after validation; parametric `+l` /
// `+k` carry their value in `change.Param` (already a positive
// int or non-empty key, respectively).
func (s *Session) setChannelAttributeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) error {
	return s.inSpan(ctx, "session.set_channel_attribute", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String("mode.flag", string(change.Flag)),
		attribute.Bool("mode.add", change.Add),
	}, func(ctx context.Context, _ trace.Span) error {
		applyAttribute(&window.Modes, change)

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}

		s.persistAndEmit(ctx, ch, domain.ChannelModeChange{
			Target: ch,
			Flag:   change.Flag,
			Add:    change.Add,
			Param:  attributeEmitParam(change),
			By:     actor.Nick(),
			At:     s.now(),
		})

		return nil
	})
}

// applyAttribute mutates `modes` according to `change`. Boolean
// flags toggle directly; parametric flags clear on `-` and set on
// `+`. The caller guarantees shape via [validateChannelModeChange].
func applyAttribute(modes *domain.ChannelModes, change protocol.ChannelModeChange) {
	switch change.Flag {
	case domain.ModeAnonymous:
		modes.Anonymous = change.Add
	case domain.ModeInviteOnly:
		modes.InviteOnly = change.Add
	case domain.ModeModerated:
		modes.Moderated = change.Add
	case domain.ModeNoExternal:
		modes.NoExternal = change.Add
	case domain.ModePrivate:
		modes.Private = change.Add
	case domain.ModeQuiet:
		modes.Quiet = change.Add
	case domain.ModeSecret:
		modes.Secret = change.Add
	case domain.ModeTopicLock:
		modes.TopicLock = change.Add
	case domain.ModeUserLimit:
		if change.Add {
			n, _ := strconv.Atoi(change.Param)
			modes.UserLimit = n
		} else {
			modes.UserLimit = 0
		}
	case domain.ModeKey:
		if change.Add {
			modes.Key = change.Param
		} else {
			modes.Key = ""
		}
	}
}

// attributeEmitParam returns the parameter to include on the
// broadcast [domain.ChannelModeChange] for an attribute change. Boolean
// modes and remove-form parametric modes emit no parameter.
func attributeEmitParam(change protocol.ChannelModeChange) string {
	if !change.Add {
		return ""
	}

	switch change.Flag {
	case domain.ModeUserLimit, domain.ModeKey:
		return change.Param
	}

	return ""
}
