// Package chatcmd defines the concrete slash-command types for the
// chat screen. It consumes the generic command library and binds it
// to the application's session layer and Bubble Tea runtime.
package chatcmd

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
)

// Command is the typed command interface for the chat screen.
type Command = command.Command[Context, tea.Cmd]

// Parser is the typed parser for the chat screen.
type Parser = command.Parser[Context, tea.Cmd]

// Context carries the dependencies a command needs to execute.
type Context struct {
	Ctx        context.Context
	Session    *session.Session
	Config     config.Store
	Active     domain.ChannelName
	Nick       domain.Nick
	Invocation command.Invocation
}

// updateConfig loads the current configuration, applies fn, and saves
// the result. It returns the updated configuration.
func (rc Context) updateConfig(fn func(*config.Config)) (config.Config, error) {
	cfg, err := rc.Config.Load(rc.Ctx)
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	fn(&cfg)

	if err := rc.Config.Save(rc.Ctx, cfg); err != nil {
		return config.Config{}, fmt.Errorf("save config: %w", err)
	}

	return cfg, nil
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
type APIKeySetResult struct {
	Reset bool
}

// PokeIntervalSetResult signals that the poke interval was updated.
type PokeIntervalSetResult struct {
	Interval time.Duration
	Reset    bool
}

// NickModelSetResult signals that the nick generation model was
// updated.
type NickModelSetResult struct {
	ModelID domain.ModelID
	Reset   bool
}

// HighlightWordsSetResult signals that the highlight words were
// updated.
type HighlightWordsSetResult struct {
	Words []string
	Reset bool
}

// BaseURLSetResult signals that the API base URL was updated.
type BaseURLSetResult struct {
	URL   string
	Reset bool
}

// EmbeddingModelSetResult signals that the embedding model was
// updated.
type EmbeddingModelSetResult struct {
	ModelID domain.ModelID
	Reset   bool
}

// TimestampFormatSetResult signals that the timestamp format was
// updated.
type TimestampFormatSetResult struct {
	Format *string
	Reset  bool
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

func (c Context) configResetRequested() bool {
	value, ok := c.Invocation.ValueAtPath("config")
	if !ok {
		return false
	}

	cfg, ok := value.(ConfigCommand)
	if !ok {
		return false
	}

	return cfg.Reset
}
