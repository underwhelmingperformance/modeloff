package modelclient

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

const turnJournalWriteTimeout = 5 * time.Second

type modelTurnInput struct {
	Request modelTurnRequest `json:"request"`
}

func journalInput(request api.RenderedEventRequest) modelTurnInput {
	return modelTurnInput{Request: modelTurnRequest{
		Method:  request.Method,
		Path:    request.Path,
		Headers: request.Headers,
		Body:    string(request.Body),
	}}
}

type modelTurnRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

type modelTurnAssistant struct {
	ToolCalls []modelTurnToolCall `json:"tool_calls"`
	Text      string              `json:"text,omitempty"`
	Refusal   string              `json:"refusal,omitempty"`
	RequestID string              `json:"request_id"`
	Usage     api.Usage           `json:"usage"`
}

type modelTurnToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

type modelTurnToolResults struct {
	Results []api.ToolResult `json:"results"`
}

type modelTurnOutcome struct {
	ToolTurnCount int    `json:"tool_turn_count"`
	PassReason    string `json:"pass_reason,omitempty"`
	Error         string `json:"error,omitempty"`
}

// turnJournalWriter records one provider turn. It numbers the
// entries it produces, starting at zero for the input entry
// [beginTurnJournal] opens the turn with, so a reader recovers the
// producer's order without depending on the order the store inserted
// the rows in. A gap in the numbering is an entry the writer produced
// and the store never accepted.
//
// Every method hands its entry to the client's [journalQueue] and
// returns. A turn therefore never waits for the store, and a failed
// write reaches the operator's log rather than the turn's result.
type turnJournalWriter struct {
	queue *journalQueue
	turn  *journalTurn
	now   func() time.Time
	seq   int
}

// beginTurnJournal queues the admission of one turn and returns the
// writer for the rest of it. A nil journal means the client records
// nothing, and every later call on the nil writer does nothing.
func beginTurnJournal(
	queue *journalQueue,
	journal TurnJournal,
	guard protocol.WindowGuard,
	turn store.ModelTurn,
	input modelTurnInput,
	now func() time.Time,
) *turnJournalWriter {
	if journal == nil {
		return nil
	}

	writer := &turnJournalWriter{
		queue: queue, turn: &journalTurn{}, now: now,
	}
	writer.enqueue(&journalBegin{journal: journal, guard: guard, turn: turn},
		store.ModelTurnInput, input, turn.StartedAt)

	return writer
}

func (j *turnJournalWriter) assistant(result api.CompletionResult) {
	j.append(store.ModelTurnAssistant, modelTurnAssistant{
		ToolCalls: journalToolCalls(result.PendingToolCalls),
		Text:      result.AssistantText,
		Refusal:   result.Refusal,
		RequestID: result.RequestID,
		Usage:     result.Usage,
	})
}

func (j *turnJournalWriter) request(request api.RenderedEventRequest) {
	j.append(store.ModelTurnInput, journalInput(request))
}

func journalToolCalls(calls []api.PendingToolCall) []modelTurnToolCall {
	if len(calls) == 0 {
		return nil
	}

	journalCalls := make([]modelTurnToolCall, len(calls))
	for i, call := range calls {
		journalCalls[i] = modelTurnToolCall{
			ID: call.ID, Name: call.Name, Args: string(call.Args),
		}
	}

	return journalCalls
}

func (j *turnJournalWriter) toolResults(results []api.ToolResult) {
	j.append(store.ModelTurnToolResults, modelTurnToolResults{Results: results})
}

func (j *turnJournalWriter) outcome(outcome turnOutcome, err error) {
	record := modelTurnOutcome{
		ToolTurnCount: outcome.toolTurnCount,
		PassReason:    outcome.passReason,
	}
	if err != nil {
		record.Error = err.Error()
	}

	j.append(store.ModelTurnOutcome, record)
}

func (j *turnJournalWriter) append(kind store.ModelTurnEntryKind, value any) {
	if j == nil {
		return
	}

	j.enqueue(nil, kind, value, j.now())
}

// enqueue marshals `value` and hands it to the queue. The timestamp
// and the sequence number are taken here, where the turn produced the
// entry, so neither reports when the store happened to accept it.
func (j *turnJournalWriter) enqueue(
	begin *journalBegin,
	kind store.ModelTurnEntryKind,
	value any,
	at time.Time,
) {
	seq := j.seq
	j.seq++

	data, err := json.Marshal(value)
	if err != nil {
		slog.Default().With("component", "modelclient").WarnContext(
			j.queue.writeContext, "marshal model turn entry",
			"kind", string(kind), "seq", seq, "error", err,
		)

		return
	}

	j.queue.enqueue(journalWrite{
		turn:  j.turn,
		begin: begin,
		entry: store.ModelTurnEntry{Kind: kind, Seq: seq, Data: data, At: at},
	})
}
