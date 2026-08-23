package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// ErrModelTurnClosed means the actor can no longer append to a turn
// because its window or instance was deleted.
var ErrModelTurnClosed = errors.New("model turn is closed")

// ModelTurnID links a journal entry to the turn it belongs to. It
// never crosses the actor protocol: a model reaches its own turn
// through a [ModelTurnRecorder] and never learns the id. An operator
// read starts from [ModelTurnRecord.ID], which the same value fills.
type ModelTurnID int64

// ModelTurn describes the actor and window whose model-visible turn
// is recorded in the journal.
type ModelTurn struct {
	InstanceID domain.InstanceID
	Window     protocol.WindowTarget
	ModelID    domain.ModelID
	StartedAt  time.Time
}

// ModelTurnRecord names one recorded turn to a reader that has not
// seen its entries. It is what an operator picks a turn from before
// asking [ModelTurnEntries] for the entries themselves.
type ModelTurnRecord struct {
	ID         ModelTurnID
	InstanceID domain.InstanceID
	Window     protocol.WindowTarget
	ModelID    domain.ModelID
	StartedAt  time.Time
}

// ModelTurnEntryKind identifies one step in a provider turn.
type ModelTurnEntryKind string

const (
	// ModelTurnInput contains one request sent to the provider.
	ModelTurnInput ModelTurnEntryKind = "input"
	// ModelTurnAssistant contains one provider response.
	ModelTurnAssistant ModelTurnEntryKind = "assistant"
	// ModelTurnToolResults contains the results of one tool batch.
	ModelTurnToolResults ModelTurnEntryKind = "tool_results"
	// ModelTurnOutcome contains the terminal state of the turn.
	ModelTurnOutcome ModelTurnEntryKind = "outcome"
)

// ModelTurnEntry is one verbatim JSON record in a provider turn.
// The writer owns the schema inside Data; the store preserves it
// without interpreting it.
//
// Seq is the writer's position for this entry within its turn,
// counting from zero at the input entry the turn opens with.
// [ModelTurnEntries] reads by it, so a writer that hands entries to
// the store out of order still reads them back in the order it
// produced them. A gap in the sequence means the writer produced an
// entry the store never accepted.
type ModelTurnEntry struct {
	Kind ModelTurnEntryKind
	Seq  int
	Data json.RawMessage
	At   time.Time
}

// ModelTurnRecorder appends evidence to one admitted turn without
// exposing its store row identity. The session implementation binds
// each append to the actor and window authority captured at admission.
type ModelTurnRecorder interface {
	AppendModelTurnEntry(ctx context.Context, entry ModelTurnEntry) error
}
