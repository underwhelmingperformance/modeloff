// Package chatcmd defines the concrete slash-command types for the
// chat screen. It consumes the generic command library and binds it
// to the application's session layer and Bubble Tea runtime.
package chatcmd

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
)

// Command is the typed command interface for the chat screen.
type Command = command.Command[Context, tea.Cmd]

// Parser is the typed parser for the chat screen.
type Parser = command.Parser[Context, tea.Cmd]

// Context carries the dependencies a command needs to execute.
type Context struct {
	Ctx     context.Context
	Session *session.Session
	Active  domain.ChannelName
	Nick    domain.Nick
}

// HelpResult signals that the help screen should be shown.
type HelpResult struct{}

// WhoisResult carries the instance metadata for a /whois reply.
type WhoisResult struct {
	Instance domain.Instance
}

// TopicInfoResult carries the current topic metadata for display.
type TopicInfoResult struct {
	Channel domain.Channel
}

// ListResult carries the channel list for a /list reply.
type ListResult struct {
	Channels []domain.Channel
}

// UsageError indicates a command was invoked incorrectly. Usage
// carries the human-readable usage string (e.g. "/invite <model>").
type UsageError struct {
	Command string
	Usage   string
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

// HighlightWordsSetResult signals that the highlight words were
// updated.
type HighlightWordsSetResult struct {
	Words []string
}

// BaseURLSetResult signals that the API base URL was updated.
type BaseURLSetResult struct {
	URL string
}

// EmbeddingModelSetResult signals that the embedding model was
// updated.
type EmbeddingModelSetResult struct {
	ModelID domain.ModelID
}

// TimestampFormatSetResult signals that the timestamp format was
// updated.
type TimestampFormatSetResult struct {
	Format *string
}

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

func usageCmd(cmd, usage string) tea.Cmd {
	return func() tea.Msg { return UsageError{Command: cmd, Usage: usage} }
}

func noChannelCmd() tea.Cmd {
	return func() tea.Msg { return NoChannelError{} }
}
