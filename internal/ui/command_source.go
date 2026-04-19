package ui

import "github.com/laney/modeloff/internal/command"

// CommandSource is implemented by models that contribute slash
// commands. K parameterises the grammar on the completion context
// type used in the consuming screen.
type CommandSource[K command.KindProvider] interface {
	Commands() command.Set[K]
}
