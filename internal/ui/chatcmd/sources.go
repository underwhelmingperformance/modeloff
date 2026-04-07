package chatcmd

import (
	"iter"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

// ChannelsSource suggests known channels.
func ChannelsSource(channels iter.Seq[domain.Channel]) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		if channels == nil {
			return nil
		}

		var suggestions []command.Suggestion

		for ch := range channels {
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
func ActiveMembersSource(members iter.Seq[domain.Nick], userNick domain.Nick) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		if members == nil {
			return nil
		}

		var suggestions []command.Suggestion

		for nick := range members {
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
}

// InstancesSource suggests known instance nicks.
func InstancesSource(instances iter.Seq[domain.Instance]) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		if instances == nil {
			return nil
		}

		var suggestions []command.Suggestion

		for inst := range instances {
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
// given active channel.
func ReusableInstancesSource(instances iter.Seq[domain.Instance], active domain.ChannelName) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		if instances == nil {
			return nil
		}

		var suggestions []command.Suggestion

		for inst := range instances {
			if inst.Channels != nil {
				if _, ok := inst.Channels.Get(active); ok {
					continue
				}
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

// LiveModelsSource suggests live model identifiers.
func LiveModelsSource(models []ModelOption) command.SuggestionSource {
	return func(_ command.InvocationState) []command.Suggestion {
		suggestions := make([]command.Suggestion, 0, len(models))

		for _, model := range models {
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
