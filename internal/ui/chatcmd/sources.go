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
	Channels        func() iter.Seq[domain.Channel]
	Instances       func() iter.Seq[*domain.Instance]
	ChannelMembers  func() iter.Seq[*domain.Instance]
	ActiveMembers   func() iter.Seq[domain.Nick]
	ActiveChannel   func() domain.ChannelName
	UserNick        func() domain.Nick
	LiveModels      func() iter.Seq[ModelOption]
	LiveModelsState func() command.SuggestionState
	Personas        func() iter.Seq[domain.Persona]
	Kind            func() domain.ChannelKind
}

// ChannelKind implements command.KindProvider.
func (ctx CompletionContext) ChannelKind() domain.ChannelKind {
	return ctx.Kind()
}

// source wraps a typed source function so that chatcmd code never
// mentions the any type directly.
func source(fn func(CompletionContext, command.InvocationState) []command.Suggestion) command.SuggestionSource {
	return command.TypedSource(fn)
}

func resultSource(fn func(CompletionContext, command.InvocationState) command.SuggestionResult) command.SuggestionSource {
	return command.TypedResultSource(fn)
}

// channelsSource suggests known channels.
func channelsSource(ctx CompletionContext, _ command.InvocationState) []command.Suggestion {
	var suggestions []command.Suggestion

	for ch := range ctx.Channels() {
		suggestions = append(suggestions, command.Suggestion{
			Value:  string(ch.Name),
			Label:  string(ch.Name),
			Detail: channelDetail(ch),
		})
	}

	return suggestions
}

// activeMembersSource suggests members of the active channel,
// excluding the user's own nick.
func activeMembersSource(ctx CompletionContext, _ command.InvocationState) []command.Suggestion {
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

	return suggestions
}

// instancesSource suggests known instance nicks.
func instancesSource(ctx CompletionContext, _ command.InvocationState) []command.Suggestion {
	var suggestions []command.Suggestion

	for inst := range ctx.Instances() {
		suggestions = append(suggestions, command.Suggestion{
			Value:  string(inst.Nick()),
			Label:  string(inst.Nick()),
			Detail: string(inst.ModelID),
		})
	}

	return suggestions
}

// personasSource suggests known persona identifiers.
func personasSource(ctx CompletionContext, _ command.InvocationState) []command.Suggestion {
	var suggestions []command.Suggestion

	for p := range ctx.Personas() {
		suggestions = append(suggestions, command.Suggestion{
			Value:  p.ID,
			Label:  p.ID,
			Detail: p.Description,
		})
	}

	return suggestions
}

// liveModelsSource suggests live model identifiers.
func liveModelsSource(ctx CompletionContext, _ command.InvocationState) command.SuggestionResult {
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

func channelDetail(ch domain.Channel) string {
	if ch.Topic != "" {
		return ch.Topic
	}

	if ch.Kind == domain.KindDM {
		return "direct message"
	}

	return ""
}
