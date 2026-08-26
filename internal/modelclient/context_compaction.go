package modelclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

const (
	contextSummaryPromptAllowance  = 512
	contextSummaryPlaceholderBytes = contextSummaryPromptAllowance * 4
)

// maxContextSummaryRounds bounds the summary round trips one turn may
// spend bringing itself inside the model's context window. Every one
// of them is an upstream call the turn pays for before it has sent
// its own request, and [maxToolLoopTurns] bounds what follows, so the
// worst case for one dispatch stays a fixed number of calls.
//
// Past the bound the turn drops its oldest lines and sends what fits.
// The audit log keeps every line either way, so what a drop costs is
// the model's recollection of them and not the record.
const maxContextSummaryRounds = 3

// errContextSummaryBudgetSpent reports that a turn has used its
// [maxContextSummaryRounds] summary calls. Compaction answers it by
// dropping the prefix it was about to summarise.
var errContextSummaryBudgetSpent = errors.New("context summary round trips are spent")

// contextSummaryBudget counts down the summary calls one turn has
// left. The compaction request is copied between helpers, so the
// count lives behind a pointer and every copy spends from the same
// one.
type contextSummaryBudget struct {
	remaining int
}

func (b *contextSummaryBudget) spend() bool {
	if b.remaining <= 0 {
		return false
	}
	b.remaining--

	return true
}

type contextCompactionRequest struct {
	plan       contextPlanRequest
	rawRecent  []protocol.IRCMessage
	rawEvents  []protocol.IRCMessage
	summaries  []store.ContextSummary
	window     protocol.WindowContext
	projection providerTargetProjection
	contexts   ContextStore
	guard      protocol.WindowGuard
	summarizer api.ContextSummarizer
	now        func() time.Time
	budget     *contextSummaryBudget

	recordRequest   func(api.RenderedEventRequest)
	recordAssistant func(api.ContextSummaryResult)
}

// ContextWindowExceededError reports that the fixed request material
// and its smallest summary cannot fit the configured model window.
type ContextWindowExceededError struct {
	ContextLength int
	PromptTokens  int
}

func (e *ContextWindowExceededError) Error() string {
	return fmt.Sprintf(
		"model context window is %d tokens but the compacted request needs about %d prompt tokens",
		e.ContextLength,
		e.PromptTokens,
	)
}

func compactContextPlan(
	ctx context.Context,
	req contextCompactionRequest,
) (ContextPlan, error) {
	plan, err := buildContextPlan(req.plan)
	if err != nil {
		return ContextPlan{}, err
	}
	if plan.Budget.Fits || plan.Budget.PromptLimit <= 0 {
		return plan, nil
	}
	if req.contexts == nil || req.summarizer == nil {
		return ContextPlan{}, &ContextWindowExceededError{
			ContextLength: plan.Budget.ContextLength,
			PromptTokens:  plan.Budget.EstimatedPrompt,
		}
	}
	if req.now == nil {
		req.now = time.Now
	}
	if req.budget == nil {
		req.budget = &contextSummaryBudget{remaining: maxContextSummaryRounds}
	}

	for !plan.Budget.Fits {
		total := len(req.rawRecent) + len(req.rawEvents)
		if total == 0 {
			return ContextPlan{}, &ContextWindowExceededError{
				ContextLength: plan.Budget.ContextLength,
				PromptTokens:  plan.Budget.EstimatedPrompt,
			}
		}

		wanted, err := contextPrefixNeeded(req)
		if errors.Is(err, errContextPrefixInsufficient) {
			return ContextPlan{}, &ContextWindowExceededError{
				ContextLength: plan.Budget.ContextLength,
				PromptTokens:  plan.Budget.EstimatedPrompt,
			}
		}
		if err != nil {
			return ContextPlan{}, err
		}
		batch, err := contextSummaryBatch(req, wanted)
		if err != nil {
			return ContextPlan{}, err
		}

		previous := contextSummaryTexts(req.summaries)
		projectedSources := append(
			append([]protocol.IRCMessage{}, req.plan.recent[:batch.recent]...),
			req.plan.events[:batch.events]...,
		)
		rawSources := append(
			append([]protocol.IRCMessage{}, req.rawRecent[:batch.recent]...),
			req.rawEvents[:batch.events]...,
		)
		result, err := summarizeContextSources(ctx, req, previous, projectedSources)
		if err != nil {
			if ctx.Err() != nil {
				return ContextPlan{}, err
			}

			slog.WarnContext(ctx, "dropping the oldest turn context unsummarised",
				"component", "modelclient",
				"instance_id", req.plan.instanceID,
				"window", protocol.WindowKey(req.window.Target()),
				"dropped", batch.recent+batch.events,
				"error", err,
			)

			req = dropContextSources(req, batch)
			plan, err = buildContextPlan(req.plan)
			if err != nil {
				return ContextPlan{}, err
			}

			continue
		}

		committed, err := req.contexts.CommitContextSummary(ctx, req.guard, store.ContextSummaryUpdate{
			InstanceID: req.plan.instanceID,
			Window:     req.window.Target(),
			Summary:    result.Summary,
			Sources:    rawSources,
			Supersedes: contextSummaryIDs(req.summaries),
			CreatedAt:  req.now(),
		})
		if errors.Is(err, protocol.ErrWindowAuthorityChanged) {
			return ContextPlan{}, errDispatchWindowClosed
		}
		if err != nil {
			return ContextPlan{}, observability.ErrWithKind(
				fmt.Errorf("commit context summary: %w", err),
				observability.ErrorKindStore,
			)
		}

		req.summaries = []store.ContextSummary{committed}
		req = dropContextSources(req, batch)
		req.plan.summaries = req.projection.messages(
			contextSummaryMessages(req.window, req.summaries),
		)

		plan, err = buildContextPlan(req.plan)
		if err != nil {
			return ContextPlan{}, err
		}
	}

	return plan, nil
}

// dropContextSources advances the request past `batch`, which is the
// prefix a summary has just replaced or which the turn is giving up
// on. Both slice pairs move together: the raw messages are what a
// committed summary records as its sources, and the projected ones
// are what the prompt renders.
func dropContextSources(
	req contextCompactionRequest,
	batch contextPrefix,
) contextCompactionRequest {
	req.rawRecent = req.rawRecent[batch.recent:]
	req.rawEvents = req.rawEvents[batch.events:]
	req.plan.recent = req.plan.recent[batch.recent:]
	req.plan.events = req.plan.events[batch.events:]

	return req
}

func summarizeContextSources(
	ctx context.Context,
	req contextCompactionRequest,
	previous []string,
	sources []protocol.IRCMessage,
) (api.ContextSummaryResult, error) {
	rendered, err := renderContextSummaryRequest(req, previous, sources)
	if err != nil {
		return api.ContextSummaryResult{}, err
	}
	limit := req.plan.contextLength - contextSummaryPromptAllowance
	if limit > 0 && summaryPromptTokens(req, rendered) <= limit {
		return sendContextSummaryRequest(ctx, req, previous, sources, rendered)
	}
	if len(sources) != 1 || sources[0].Body == "" {
		return api.ContextSummaryResult{}, &ContextWindowExceededError{
			ContextLength: req.plan.contextLength,
			PromptTokens:  summaryPromptTokens(req, rendered),
		}
	}

	return summarizeContextMessage(ctx, req, previous, sources[0], limit)
}

func summarizeContextMessage(
	ctx context.Context,
	req contextCompactionRequest,
	previous []string,
	source protocol.IRCMessage,
	limit int,
) (api.ContextSummaryResult, error) {
	remaining := source.Body
	currentPrevious := previous
	var result api.ContextSummaryResult
	for remaining != "" {
		fragment, rendered, err := fittingContextMessageFragment(
			req, currentPrevious, source, remaining, limit,
		)
		if err != nil {
			return api.ContextSummaryResult{}, err
		}
		result, err = sendContextSummaryRequest(
			ctx, req, currentPrevious, []protocol.IRCMessage{fragment}, rendered,
		)
		if err != nil {
			return api.ContextSummaryResult{}, err
		}

		remaining = remaining[len(fragment.Body):]
		currentPrevious = []string{result.Summary}
	}

	return result, nil
}

func fittingContextMessageFragment(
	req contextCompactionRequest,
	previous []string,
	source protocol.IRCMessage,
	body string,
	limit int,
) (protocol.IRCMessage, api.RenderedEventRequest, error) {
	boundaries := contextBodyBoundaries(body)
	best := 0
	var bestRequest api.RenderedEventRequest
	for low, high := 1, len(boundaries)-1; low <= high; {
		middle := low + (high-low)/2
		candidate := source
		candidate.Body = body[:boundaries[middle]]
		rendered, err := renderContextSummaryRequest(
			req, previous, []protocol.IRCMessage{candidate},
		)
		if err != nil {
			return protocol.IRCMessage{}, api.RenderedEventRequest{}, err
		}
		if limit > 0 && summaryPromptTokens(req, rendered) <= limit {
			best = middle
			bestRequest = rendered
			low = middle + 1
			continue
		}

		high = middle - 1
	}
	if best == 0 {
		candidate := source
		candidate.Body = body[:boundaries[1]]
		rendered, err := renderContextSummaryRequest(
			req, previous, []protocol.IRCMessage{candidate},
		)
		if err != nil {
			return protocol.IRCMessage{}, api.RenderedEventRequest{}, err
		}

		return protocol.IRCMessage{}, api.RenderedEventRequest{}, &ContextWindowExceededError{
			ContextLength: req.plan.contextLength,
			PromptTokens:  summaryPromptTokens(req, rendered),
		}
	}

	fragment := source
	fragment.Body = body[:boundaries[best]]

	return fragment, bestRequest, nil
}

func contextBodyBoundaries(body string) []int {
	boundaries := make([]int, 1, len(body)+1)
	for index := range body {
		if index > 0 {
			boundaries = append(boundaries, index)
		}
	}

	return append(boundaries, len(body))
}

// summaryPromptTokens estimates a summary request the same way the
// dispatch plan estimates its own, so both sides of one turn's budget
// read the same tokeniser.
func summaryPromptTokens(
	req contextCompactionRequest,
	rendered api.RenderedEventRequest,
) int {
	return estimatePromptTokens(req.summarizer, req.plan.modelID, rendered)
}

func renderContextSummaryRequest(
	req contextCompactionRequest,
	previous []string,
	sources []protocol.IRCMessage,
) (api.RenderedEventRequest, error) {
	rendered, err := req.summarizer.RenderContextSummaryRequest(
		req.plan.modelID, req.plan.instanceID, previous, sources,
	)
	if err != nil {
		return api.RenderedEventRequest{}, fmt.Errorf("render context summary request: %w", err)
	}

	return rendered, nil
}

func sendContextSummaryRequest(
	ctx context.Context,
	req contextCompactionRequest,
	previous []string,
	sources []protocol.IRCMessage,
	rendered api.RenderedEventRequest,
) (api.ContextSummaryResult, error) {
	if !req.budget.spend() {
		return api.ContextSummaryResult{}, errContextSummaryBudgetSpent
	}
	if req.recordRequest != nil {
		req.recordRequest(rendered)
	}

	result, err := req.summarizer.SummarizeContext(
		ctx, req.plan.modelID, req.plan.instanceID, previous, sources,
	)
	if err != nil {
		return api.ContextSummaryResult{}, fmt.Errorf("summarise model context: %w", err)
	}
	if req.recordAssistant != nil {
		req.recordAssistant(result)
	}

	return result, nil
}

type contextPrefix struct {
	recent int
	events int
}

// errContextPrefixInsufficient reports that replacing the whole
// transcript and burst still leaves the request over its limit, so no
// prefix is the answer and no summary call would help.
var errContextPrefixInsufficient = errors.New(
	"no transcript prefix brings the request inside its prompt limit",
)

// contextPrefixNeeded returns the shortest prefix of the recent
// transcript and the current burst whose replacement by one summary
// brings the turn inside its prompt limit. Dropping a longer prefix
// leaves a smaller request, so the predicate is monotone and the
// search bisects.
//
// Replacing everything is the most compaction can do. When that does not
// fit, the system prompt, the current state and the tools are over the
// limit between them, and returning the whole prefix anyway would spend a
// summary call and commit a durable summary to reach the same refusal.
func contextPrefixNeeded(req contextCompactionRequest) (int, error) {
	total := len(req.rawRecent) + len(req.rawEvents)
	placeholder := store.ContextSummary{
		Window: req.window.Target(), Summary: strings.Repeat("x", contextSummaryPlaceholderBytes),
		CreatedAt: req.now(),
	}
	fits := func(count int) (bool, error) {
		prefix := splitContextPrefix(count, len(req.rawRecent))
		candidate := req.plan
		candidate.recent = candidate.recent[prefix.recent:]
		candidate.events = candidate.events[prefix.events:]
		candidate.summaries = req.projection.messages(
			contextSummaryMessages(req.window, []store.ContextSummary{placeholder}),
		)
		plan, err := buildContextPlan(candidate)
		if err != nil {
			return false, err
		}

		return plan.Budget.Fits, nil
	}

	switch ok, err := fits(total); {
	case err != nil:
		return 0, err
	case !ok:
		return 0, errContextPrefixInsufficient
	}

	needed := total
	for low, high := 1, total-1; low <= high; {
		middle := low + (high-low)/2
		ok, err := fits(middle)
		if err != nil {
			return 0, err
		}
		if ok {
			needed = middle
			high = middle - 1
			continue
		}

		low = middle + 1
	}

	return needed, nil
}

func contextSummaryBatch(req contextCompactionRequest, wanted int) (contextPrefix, error) {
	previous := contextSummaryTexts(req.summaries)
	limit := req.plan.contextLength - contextSummaryPromptAllowance
	if limit <= 0 {
		return splitContextPrefix(1, len(req.rawRecent)), nil
	}

	fits := func(count int) (bool, error) {
		prefix := splitContextPrefix(count, len(req.rawRecent))
		sources := append(
			append([]protocol.IRCMessage{}, req.plan.recent[:prefix.recent]...),
			req.plan.events[:prefix.events]...,
		)
		rendered, err := req.summarizer.RenderContextSummaryRequest(
			req.plan.modelID, req.plan.instanceID, previous, sources,
		)
		if err != nil {
			return false, fmt.Errorf("render context summary request: %w", err)
		}

		return summaryPromptTokens(req, rendered) <= limit, nil
	}

	selected := 1
	for low, high := 1, wanted; low <= high; {
		middle := low + (high-low)/2
		ok, err := fits(middle)
		if err != nil {
			return contextPrefix{}, err
		}
		if ok {
			selected = middle
			low = middle + 1
			continue
		}

		high = middle - 1
	}

	return splitContextPrefix(selected, len(req.rawRecent)), nil
}

func splitContextPrefix(count, recent int) contextPrefix {
	if count <= recent {
		return contextPrefix{recent: count}
	}

	return contextPrefix{recent: recent, events: count - recent}
}

func contextSummaryTexts(summaries []store.ContextSummary) []string {
	if len(summaries) == 0 {
		return nil
	}

	texts := make([]string, len(summaries))
	for i, summary := range summaries {
		texts[i] = summary.Summary
	}

	return texts
}

func contextSummaryIDs(summaries []store.ContextSummary) []store.ContextSummaryID {
	if len(summaries) == 0 {
		return nil
	}

	ids := make([]store.ContextSummaryID, len(summaries))
	for i, summary := range summaries {
		ids[i] = summary.ID
	}

	return ids
}
