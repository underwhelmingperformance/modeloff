package modelmanager

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

const (
	// maxReflectionToolTurns bounds one run's exploration, the way
	// [modelclient] bounds an acting turn's tool loop. A run past the
	// bound proposes from what it has read so far.
	maxReflectionToolTurns = 5

	// maxReflectionRecallEvents caps one tool result. Everything a
	// result carries is repeated in every later request of the run, so
	// the cap is what keeps a run's prompt from growing per call.
	maxReflectionRecallEvents = 40
)

// reflectionRecallStore is the read-only capability a reflecting
// instance's tools hold. Both reads take the instance the run belongs to
// and match on it, so a tool call reaches no other instance's stream, and
// neither read can change anything.
type reflectionRecallStore interface {
	ReflectionEventsBefore(
		ctx context.Context,
		instanceID domain.InstanceID,
		window protocol.WindowTarget,
		before domain.ReflectionSequence,
		limit int,
	) ([]store.ReflectionEvent, error)
	ReflectionEventsBySequence(
		ctx context.Context,
		instanceID domain.InstanceID,
		sequences []domain.ReflectionSequence,
	) ([]store.ReflectionEvent, error)
}

// reflectionTools is what one run offers the instance for reading its own
// past. It answers in the event shape the request already carries, under
// the same per-run tokens, so a tool result reads like more of the
// transcript and introduces no second vocabulary.
type reflectionTools struct {
	store      reflectionRecallStore
	aliases    *reflectionAliases
	instanceID domain.InstanceID

	// checkpoint is where the request's own event range begins. It is
	// the default starting point for reading back, so an instance that
	// asks for history without saying where gets the events immediately
	// preceding the ones it has just read.
	checkpoint domain.ReflectionSequence
}

func newReflectionTools(
	stored reflectionRecallStore,
	snapshot store.PendingReflectionSnapshot,
	aliases *reflectionAliases,
) *reflectionTools {
	return &reflectionTools{
		store:      stored,
		aliases:    aliases,
		instanceID: snapshot.Persona.Lineage.InstanceID,
		checkpoint: snapshot.Range.Checkpoint,
	}
}

func (t *reflectionTools) definitions() []api.ToolDefinition {
	return []api.ToolDefinition{
		{
			Name:        "recall_history",
			Description: "Read further back through what you saw, older than the events in this request. Use it to tell a one-off from a pattern, and to see what surrounded a remark. Set window to a channel name or to a participant token for your direct conversation with them, or leave it empty to read every window at once. Set before to a sequence number to page further back, or 0 to start where this request's events start.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"window": map[string]any{
						"type":        "string",
						"description": "A channel name, or a participant token for your direct conversation with them. Empty reads every window.",
					},
					"before": map[string]any{
						"type":        "integer",
						"description": "Read events older than this sequence number. 0 starts where this request's events start.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of events to return (1-40).",
					},
				},
				"required":             []string{"window", "before", "limit"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "recall_events",
			Description: "Read the exact events named by sequence number. The experiences under active_experiences cite their sources this way, so this is how you see what a belief you already hold was built on.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sequences": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "integer"},
						"description": "The sequence numbers to read, at most 40 of them.",
					},
				},
				"required":             []string{"sequences"},
				"additionalProperties": false,
			},
		},
	}
}

// execute answers one batch of tool calls. A refusal is reported to the
// instance as a tool result and does not end the run: a bad argument is
// something the instance can correct on its next turn.
func (t *reflectionTools) execute(
	ctx context.Context,
	calls []api.PendingToolCall,
) []api.ToolResult {
	results := make([]api.ToolResult, 0, len(calls))
	for _, call := range calls {
		events, err := t.call(ctx, call)
		payload := modelclient.ToolResultPayload{
			OK: true, Data: events,
			Summary: fmt.Sprintf("read %d events", len(events)),
		}
		if err != nil {
			payload = modelclient.ToolResultPayload{OK: false, Error: err.Error()}
		}
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			encoded = []byte(`{"ok":false,"error":"tool result could not be encoded"}`)
		}
		results = append(results, api.ToolResult{
			ToolCallID: call.ID, Content: string(encoded),
		})
	}

	return results
}

func (t *reflectionTools) call(
	ctx context.Context,
	call api.PendingToolCall,
) ([]api.ReflectionInputEvent, error) {
	switch call.Name {
	case "recall_history":
		return t.recallHistory(ctx, call.Args)
	case "recall_events":
		return t.recallEvents(ctx, call.Args)
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

func (t *reflectionTools) recallHistory(
	ctx context.Context,
	rawArgs json.RawMessage,
) ([]api.ReflectionInputEvent, error) {
	var args struct {
		Window string                    `json:"window"`
		Before domain.ReflectionSequence `json:"before"`
		Limit  int                       `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, err
	}

	before := args.Before
	if before <= 0 {
		before = t.checkpoint + 1
	}
	limit := min(max(args.Limit, 1), maxReflectionRecallEvents)

	events, err := t.store.ReflectionEventsBefore(
		ctx, t.instanceID, t.windowTarget(args.Window), before, limit,
	)
	if err != nil {
		return nil, err
	}

	return t.render(events), nil
}

func (t *reflectionTools) recallEvents(
	ctx context.Context,
	rawArgs json.RawMessage,
) ([]api.ReflectionInputEvent, error) {
	var args struct {
		Sequences []domain.ReflectionSequence `json:"sequences"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, err
	}
	if len(args.Sequences) > maxReflectionRecallEvents {
		return nil, fmt.Errorf(
			"recall_events takes at most %d sequences", maxReflectionRecallEvents,
		)
	}

	events, err := t.store.ReflectionEventsBySequence(
		ctx, t.instanceID, args.Sequences,
	)
	if err != nil {
		return nil, err
	}

	return t.render(events), nil
}

// windowTarget reads back a window the way the request named it: a
// channel by name, and a direct conversation by the counterpart's
// participant token. An empty name reads every window, and so does a
// token that named nobody in this run.
func (t *reflectionTools) windowTarget(name string) protocol.WindowTarget {
	if name == "" {
		return nil
	}
	if peer, known := t.aliases.instanceID(name); known {
		return protocol.DirectWindowTarget(peer)
	}
	if domain.InferChannelKind(domain.ChannelName(name)) == domain.KindChannel {
		return protocol.ChannelWindowTarget(domain.ChannelName(name))
	}

	return nil
}

func (t *reflectionTools) render(
	events []store.ReflectionEvent,
) []api.ReflectionInputEvent {
	rendered := make([]api.ReflectionInputEvent, 0, len(events))
	for _, event := range events {
		rendered = append(rendered, reflectionInputEvent(event, t.aliases))
	}

	return rendered
}
