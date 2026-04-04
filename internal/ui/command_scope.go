package ui

import "github.com/laney/modeloff/internal/command"

// CommandScoper is implemented by models that contribute slash commands.
type CommandScoper interface {
	CommandScope() command.Scope
}
