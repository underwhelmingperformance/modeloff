package ui

import "github.com/laney/modeloff/internal/command"

// CommandSource is implemented by models that contribute slash commands.
type CommandSource interface {
	Commands() command.Set
}
