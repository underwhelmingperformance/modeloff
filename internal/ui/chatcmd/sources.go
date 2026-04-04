package chatcmd

import (
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

// ChannelsSource suggests known channels, reading the current list
// from the provided accessor at call time.
func ChannelsSource(channels func() []domain.Channel) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		chs := channels()
		suggestions := make([]command.Suggestion, 0, len(chs))

		for _, ch := range chs {
			suggestions = append(suggestions, command.Suggestion{
				Value:  string(ch.Name),
				Label:  string(ch.Name),
				Detail: channelDetail(ch),
			})
		}

		return suggestions
	}
}

// ActiveMembersSource suggests members of the active channel,
// excluding the user's own nick.
func ActiveMembersSource(members func() []domain.Nick, userNick func() domain.Nick) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		nicks := members()
		self := userNick()
		suggestions := make([]command.Suggestion, 0, len(nicks))

		for _, nick := range nicks {
			if nick == self {
				continue
			}

			suggestions = append(suggestions, command.Suggestion{
				Value: string(nick),
				Label: string(nick),
			})
		}

		return suggestions
	}
}

// InstancesSource suggests known instance nicks.
func InstancesSource(instances func() []domain.ModelInstance) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		insts := instances()
		suggestions := make([]command.Suggestion, 0, len(insts))

		for _, inst := range insts {
			suggestions = append(suggestions, command.Suggestion{
				Value:  string(inst.Nick),
				Label:  string(inst.Nick),
				Detail: string(inst.ModelID),
			})
		}

		return suggestions
	}
}

// ReusableInstancesSource suggests instance nicks not already in the
// active channel.
func ReusableInstancesSource(instances func() []domain.ModelInstance, activeChannel func() domain.ChannelName) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		insts := instances()
		active := activeChannel()
		suggestions := make([]command.Suggestion, 0, len(insts))

		for _, inst := range insts {
			if inst.Channels.Has(active) {
				continue
			}

			suggestions = append(suggestions, command.Suggestion{
				Value:  string(inst.Nick),
				Label:  string(inst.Nick),
				Detail: string(inst.ModelID),
			})
		}

		return suggestions
	}
}

// ModelOption describes a live model for completion suggestions.
type ModelOption struct {
	ID          domain.ModelID
	Name        string
	Description string
}

// LiveModelsSource suggests live model identifiers from the API.
func LiveModelsSource(models func() []ModelOption) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		ms := models()
		suggestions := make([]command.Suggestion, 0, len(ms))

		for _, model := range ms {
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

		return suggestions
	}
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
