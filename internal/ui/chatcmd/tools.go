package chatcmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/modelclient"
)

// ToolCommand is implemented by slash-command leaves that are also
// executable as model tools.
type ToolCommand interface {
	RunTool(context.Context, modelclient.ToolContext) modelclient.ToolOutcome
}

// BuildToolRegistry derives tool specs from the command grammar.
func BuildToolRegistry() (*modelclient.ToolRegistry, error) {
	set, err := command.Build[CompletionContext](&Grammar{})
	if err != nil {
		return nil, err
	}
	nodes := set.ToolNodes()
	specs := make([]modelclient.ToolSpec, 0, len(nodes))

	for _, node := range nodes {
		current := node

		specs = append(specs, modelclient.ToolSpec{
			Definition: api.ToolDefinition{
				Name:        current.ToolName(),
				Description: current.ToolDescription(current.NewZero()),
				Parameters:  current.ToolParameters(),
			},
			RequiredCapabilities: current.RequiredCapabilities,
			RequiredKind:         current.RequiredKind,
			Execute: func(ctx context.Context, tc modelclient.ToolContext, rawArgs json.RawMessage) (modelclient.ToolResultPayload, error) {
				value, err := current.ToolValue(rawArgs)
				if err != nil {
					return commandToolResult(current.ToolName(), toolRefusal(err))
				}

				tool, ok := value.(ToolCommand)
				if !ok {
					return commandToolResult(current.ToolName(), toolExecutionFailure(
						fmt.Errorf("command /%s does not implement ToolCommand", current.Path()),
					))
				}

				return commandToolResult(current.ToolName(), tool.RunTool(ctx, tc))
			},
		})
	}

	return modelclient.NewToolRegistry(specs...), nil
}

func commandToolResult(name string, outcome modelclient.ToolOutcome) (modelclient.ToolResultPayload, error) {
	if outcome.ExecutionError != nil {
		return modelclient.ToolResultPayload{}, &modelclient.ToolExecutionError{
			Tool: name,
			Err:  outcome.ExecutionError,
		}
	}

	return outcome.Payload, nil
}
