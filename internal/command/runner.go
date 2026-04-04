package command

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
)

// RunContext carries the dependencies a command needs to execute.
type RunContext struct {
	Ctx     context.Context
	Session *session.Session
	Active  domain.ChannelName
	Nick    domain.Nick
}

// Runner is implemented by command structs that can execute
// themselves given a RunContext.
type Runner interface {
	Run(rc RunContext) tea.Cmd
}

// HelpResult signals that the help screen should be shown.
type HelpResult struct{}

// WhoisResult carries the instance metadata for a /whois reply.
type WhoisResult struct {
	Instance domain.ModelInstance
}

// ListResult carries the channel list for a /list reply.
type ListResult struct {
	Channels []domain.Channel
}

// UsageError indicates a command was invoked incorrectly.
type UsageError struct {
	Command string
}

// NoChannelError indicates a command requires an active channel but
// none is set.
type NoChannelError struct{}

// APIKeySetResult signals that the API key was updated.
type APIKeySetResult struct{}

// PokeIntervalSetResult signals that the poke interval was updated.
type PokeIntervalSetResult struct {
	Interval time.Duration
}

// NickModelSetResult signals that the nick generation model was
// updated.
type NickModelSetResult struct {
	ModelID domain.ModelID
}
