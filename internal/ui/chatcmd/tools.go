package chatcmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/session"
)

// ToolCommand is implemented by slash-command leaves that are also
// executable as model tools.
type ToolCommand interface {
	RunTool(context.Context, session.ToolContext) session.ToolResultPayload
}

// BuildToolRegistry derives tool specs from the command grammar.
func BuildToolRegistry() (*session.ToolRegistry, error) {
	set := command.Build(&Grammar{})
	nodes := set.ToolNodes()
	specs := make([]session.ToolSpec, 0, len(nodes))

	for _, node := range nodes {
		current := node

		specs = append(specs, session.ToolSpec{
			Definition: api.ToolDefinition{
				Name:        current.ToolName(),
				Description: current.ToolDescription(current.NewZero()),
				Parameters:  current.ToolParameters(),
			},
			Execute: func(ctx context.Context, tc session.ToolContext, rawArgs json.RawMessage) (session.ToolResultPayload, error) {
				value, err := current.ToolValue(rawArgs)
				if err != nil {
					return session.ToolResultPayload{}, err
				}

				tool, ok := value.(ToolCommand)
				if !ok {
					return session.ToolResultPayload{}, fmt.Errorf("command /%s does not implement ToolCommand", current.Path())
				}

				return tool.RunTool(ctx, tc), nil
			},
		})
	}

	return session.NewToolRegistry(specs...), nil
}
