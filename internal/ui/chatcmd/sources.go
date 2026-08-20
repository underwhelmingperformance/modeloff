package chatcmd

import (
	"iter"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

// CompletionContext provides live accessors for suggestion data.
// Collection fields are iterator factories so that sources only
// materialise the data they need, and always see the latest state.
//
// `Instances` iterates every known model instance across the whole
// session — used by commands whose target is any model (/invite,
// /msg, /whois, /add-model reuse). `ChannelMembers` iterates only
// members of the currently-active channel — used by commands whose
// target must already be present in the active channel (/kick,
// inline @nick mentions).
type CompletionContext struct {
	Channels        func() iter.Seq[domain.Window]
	Instances       func() iter.Seq[*domain.Instance]
	ChannelMembers  func() iter.Seq[*domain.Instance]
	ActiveMembers   func() iter.Seq[domain.Nick]
	ActiveChannel   func() domain.ChannelName
	UserNick        func() domain.Nick
	LiveModels      func() iter.Seq[ModelOption]
	LiveModelsState func() command.SuggestionState
	Personas        func() iter.Seq[domain.Persona]
	Kind            func() domain.ChannelKind

	// Directory iterates every channel the session knows of, joined
	// or not, the same set `/list` answers with. `channelsSource`
	// merges it with `Channels` (already-open windows) so `/join`
	// and `/msg` can offer an unjoined channel as a completion
	// target: the case completion is actually useful for, since a
	// channel the user has already joined is one they typically
	// don't need to type again. Optional: a nil Directory limits
	// channel completion to windows already open.
	Directory func() iter.Seq[domain.ChannelDirectoryEntry]
}

// ChannelKind implements command.KindProvider.
func (ctx CompletionContext) ChannelKind() domain.ChannelKind {
	return ctx.Kind()
}

// channelsSource suggests known channels: every window already open,
// plus, when ctx.Directory is set, every other channel the session's
// directory knows of, so an unjoined channel is offered too. A
// channel present in both is suggested once, from Channels.
func channelsSource(ctx CompletionContext, _ command.InvocationState[CompletionContext]) command.SuggestionResult {
	var suggestions []command.Suggestion

	open := make(map[domain.ChannelName]bool)

	for w := range ctx.Channels() {
		open[w.Name()] = true

		suggestions = append(suggestions, command.Suggestion{
			Value:  string(w.Name()),
			Label:  string(w.Name()),
			Detail: channelDetail(w),
		})
	}

	if ctx.Directory != nil {
		for entry := range ctx.Directory() {
			if open[entry.Channel] {
				continue
			}

			suggestions = append(suggestions, command.Suggestion{
				Value:  string(entry.Channel),
				Label:  string(entry.Channel),
				Detail: entry.Topic,
			})
		}
	}

	return command.SuggestionResult{Suggestions: suggestions}
}

// activeMembersSource suggests members of the active channel,
// excluding the user's own nick.
func activeMembersSource(ctx CompletionContext, _ command.InvocationState[CompletionContext]) command.SuggestionResult {
	userNick := ctx.UserNick()

	var suggestions []command.Suggestion

	for nick := range ctx.ActiveMembers() {
		if nick == userNick {
			continue
		}

		suggestions = append(suggestions, command.Suggestion{
			Value: string(nick),
			Label: string(nick),
		})
	}

	return command.SuggestionResult{Suggestions: suggestions}
}

// instancesSource suggests known instance nicks.
func instancesSource(ctx CompletionContext, _ command.InvocationState[CompletionContext]) command.SuggestionResult {
	var suggestions []command.Suggestion

	for inst := range ctx.Instances() {
		suggestions = append(suggestions, command.Suggestion{
			Value:  string(inst.Nick()),
			Label:  string(inst.Nick()),
			Detail: string(inst.ModelID),
		})
	}

	return command.SuggestionResult{Suggestions: suggestions}
}

// personasSource suggests known persona identifiers.
func personasSource(ctx CompletionContext, _ command.InvocationState[CompletionContext]) command.SuggestionResult {
	var suggestions []command.Suggestion

	for p := range ctx.Personas() {
		suggestions = append(suggestions, command.Suggestion{
			Value:  p.ID,
			Label:  p.ID,
			Detail: p.Description,
		})
	}

	return command.SuggestionResult{Suggestions: suggestions}
}

// liveModelsSource suggests live model identifiers.
func liveModelsSource(ctx CompletionContext, _ command.InvocationState[CompletionContext]) command.SuggestionResult {
	if ctx.LiveModelsState != nil && ctx.LiveModelsState() == command.SuggestionStateError {
		return command.SuggestionResult{State: command.SuggestionStateError}
	}

	var suggestions []command.Suggestion

	for model := range ctx.LiveModels() {
		detail := model.Name
		if detail == "" {
			detail = model.Description
		}

		suggestions = append(suggestions, command.Suggestion{
			Value:  string(model.ID),
			Label:  string(model.ID),
			Detail: detail,
		})
	}

	return command.SuggestionResult{Suggestions: suggestions}
}

// ModelOption describes a live model for completion suggestions.
type ModelOption struct {
	ID          domain.ModelID
	Name        string
	Description string
}

func channelDetail(w domain.Window) string {
	if cw, ok := w.(*domain.ChannelWindow); ok && cw.Topic != "" {
		return cw.Topic
	}

	if w.Kind() == domain.KindDM {
		return "direct message"
	}

	return ""
}
