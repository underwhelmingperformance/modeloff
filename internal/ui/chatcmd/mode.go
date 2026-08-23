package chatcmd

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	invjsonschema "github.com/invopop/jsonschema"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

const maxModeChanges = 16

// Change specifies whether a channel mode is added or removed.
type Change string

const (
	// Add enables a mode or grants a member mode.
	Add Change = "add"
	// Remove disables a mode or revokes a member mode.
	Remove Change = "remove"
)

// PositiveInt is an integer greater than zero.
type PositiveInt int

// UnmarshalText parses a positive decimal integer.
func (n *PositiveInt) UnmarshalText(text []byte) error {
	value, err := strconv.Atoi(string(text))
	if err != nil || value <= 0 {
		return &InvalidPositiveIntError{Value: string(text)}
	}

	*n = PositiveInt(value)
	return nil
}

// UnmarshalJSON parses and validates a JSON integer.
func (n *PositiveInt) UnmarshalJSON(data []byte) error {
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}

	rational, ok := new(big.Rat).SetString(number.String())
	if !ok || !rational.IsInt() {
		return &InvalidPositiveIntError{Value: number.String()}
	}

	value, err := strconv.Atoi(rational.Num().String())
	if err != nil || value <= 0 {
		return &InvalidPositiveIntError{Value: number.String()}
	}

	*n = PositiveInt(value)
	return nil
}

// Validate checks that the integer is positive.
func (n PositiveInt) Validate() error {
	if n <= 0 {
		return &InvalidPositiveIntError{Value: strconv.Itoa(int(n))}
	}

	return nil
}

type modeDefinition struct {
	name        string
	flag        domain.Mode
	description string
}

// Mode is the closed set of channel modes accepted by the mode command.
type Mode interface {
	definition() modeDefinition
	parameter(Change) (domain.Nick, string, error)
}

// ModeOperator changes channel-operator status for one member.
type ModeOperator struct {
	Target domain.Nick `json:"target" jsonschema:"minLength=1" jsonschema_description:"The member whose channel-operator status should change."`
}

func (m ModeOperator) definition() modeDefinition {
	return modeDefinition{
		name:        "operator",
		flag:        domain.ModeOperator,
		description: "Grant or revoke channel-operator status for the target nick. Channel operators can perform moderation tasks.",
	}
}

func (m ModeOperator) parameter(Change) (domain.Nick, string, error) {
	return memberModeParameter(m.definition(), m.Target)
}

// ModeVoice changes voice status for one member.
type ModeVoice struct {
	Target domain.Nick `json:"target" jsonschema:"minLength=1" jsonschema_description:"The member whose voice status should change."`
}

func (m ModeVoice) definition() modeDefinition {
	return modeDefinition{
		name:        "voice",
		flag:        domain.ModeChannelVoice,
		description: "Grant or revoke voice for the target nick. Voice allows a member to speak in a moderated channel.",
	}
}

func (m ModeVoice) parameter(Change) (domain.Nick, string, error) {
	return memberModeParameter(m.definition(), m.Target)
}

// ModeAnonymous hides member identities in channel traffic.
type ModeAnonymous struct{}

func (ModeAnonymous) definition() modeDefinition {
	return modeDefinition{"anonymous", domain.ModeAnonymous, "Hide member identities in messages and membership events."}
}

func (ModeAnonymous) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModeInviteOnly requires an invitation before joining.
type ModeInviteOnly struct{}

func (ModeInviteOnly) definition() modeDefinition {
	return modeDefinition{"invite_only", domain.ModeInviteOnly, "Require a client to receive an invitation before joining the channel."}
}

func (ModeInviteOnly) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModeModerated restricts channel messages to privileged members.
type ModeModerated struct{}

func (ModeModerated) definition() modeDefinition {
	return modeDefinition{"moderated", domain.ModeModerated, "Allow only channel operators and voiced members to send messages or actions to the channel."}
}

func (ModeModerated) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModeNoExternal blocks messages from clients outside the channel.
type ModeNoExternal struct{}

func (ModeNoExternal) definition() modeDefinition {
	return modeDefinition{"no_external", domain.ModeNoExternal, "Require a client to be joined to the channel before sending messages or actions to it."}
}

func (ModeNoExternal) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModePrivate hides the channel from clients outside it.
type ModePrivate struct{}

func (ModePrivate) definition() modeDefinition {
	return modeDefinition{"private", domain.ModePrivate, "Hide the channel from LIST and WHOIS for clients who are not joined to it."}
}

func (ModePrivate) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModeQuiet restricts channel messages to channel operators.
type ModeQuiet struct{}

func (ModeQuiet) definition() modeDefinition {
	return modeDefinition{"quiet", domain.ModeQuiet, "Allow only channel operators to send messages or actions to the channel."}
}

func (ModeQuiet) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModeSecret hides the channel from clients outside it.
type ModeSecret struct{}

func (ModeSecret) definition() modeDefinition {
	return modeDefinition{"secret", domain.ModeSecret, "Hide the channel from LIST and WHOIS for clients who are not joined to it."}
}

func (ModeSecret) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModeTopicLock restricts topic changes to channel operators.
type ModeTopicLock struct{}

func (ModeTopicLock) definition() modeDefinition {
	return modeDefinition{"topic_lock", domain.ModeTopicLock, "Require channel-operator status to change the channel topic."}
}

func (ModeTopicLock) parameter(Change) (domain.Nick, string, error) {
	return "", "", nil
}

// ModeUserLimit limits how many clients may join the channel.
type ModeUserLimit struct {
	Limit PositiveInt `json:"limit,omitempty" jsonschema:"minimum=1" jsonschema_description:"A positive integer giving the maximum number of clients that may be joined."`
}

func (m ModeUserLimit) definition() modeDefinition {
	return modeDefinition{"user_limit", domain.ModeUserLimit, "Limit how many clients may be joined to the channel."}
}

func (m ModeUserLimit) parameter(change Change) (domain.Nick, string, error) {
	return positiveModeParameter(m.definition(), change, m.Limit)
}

// ModeKey requires a key when a client joins the channel.
type ModeKey struct {
	Key string `json:"key,omitempty" jsonschema:"minLength=1" jsonschema_description:"The key a client must supply when joining."`
}

func (m ModeKey) definition() modeDefinition {
	return modeDefinition{"key", domain.ModeKey, "Require clients to supply the channel key when joining."}
}

func (m ModeKey) parameter(change Change) (domain.Nick, string, error) {
	if change == Remove {
		if m.Key != "" {
			return "", "", &UnexpectedModeParameterError{Mode: m.definition().name, Field: "key"}
		}

		return "", "", nil
	}
	if m.Key == "" {
		return "", "", domain.MissingModeParamError{Flag: m.definition().flag}
	}

	return "", m.Key, nil
}

// ModeFloodLimit limits channel messages within each flood window.
type ModeFloodLimit struct {
	Limit PositiveInt `json:"limit,omitempty" jsonschema:"minimum=1" jsonschema_description:"A positive integer giving the maximum number of messages the channel accepts in each flood window."`
}

func (m ModeFloodLimit) definition() modeDefinition {
	return modeDefinition{"flood_limit", domain.ModeFloodLimit, "Limit how many messages the channel accepts in each flood window."}
}

func (m ModeFloodLimit) parameter(change Change) (domain.Nick, string, error) {
	return positiveModeParameter(m.definition(), change, m.Limit)
}

var modePrototypes = []Mode{
	ModeOperator{},
	ModeVoice{},
	ModeAnonymous{},
	ModeInviteOnly{},
	ModeModerated{},
	ModeNoExternal{},
	ModePrivate{},
	ModeQuiet{},
	ModeSecret{},
	ModeTopicLock{},
	ModeUserLimit{},
	ModeKey{},
	ModeFloodLimit{},
}

// ModeChange is one declarative channel-mode update.
type ModeChange struct {
	Change Change `json:"change"`
	Mode   Mode   `json:"-"`
}

// MarshalJSON flattens the concrete mode fields beside the discriminators.
func (c ModeChange) MarshalJSON() ([]byte, error) {
	if c.Mode == nil {
		return nil, &MissingModeError{}
	}

	data, err := json.Marshal(c.Mode)
	if err != nil {
		return nil, err
	}

	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}

	object["change"] = c.Change
	object["mode"] = c.Mode.definition().name

	return json.Marshal(object)
}

// UnmarshalJSON decodes a discriminator and its fields into a concrete mode.
func (c *ModeChange) UnmarshalJSON(data []byte) error {
	var header struct {
		Change Change `json:"change"`
		Mode   string `json:"mode"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	if _, err := header.Change.addsMode(); err != nil {
		return err
	}

	prototype, ok := modeForName(header.Mode)
	if !ok {
		return &UnknownModeError{Mode: header.Mode}
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	delete(fields, "change")
	delete(fields, "mode")

	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}

	target := reflect.New(reflect.TypeOf(prototype))
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target.Interface()); err != nil {
		return &ModeParameterDecodeError{Mode: header.Mode, Err: err}
	}

	*c = ModeChange{Change: header.Change, Mode: target.Elem().Interface().(Mode)}
	return c.Validate()
}

// Validate checks the operation and the concrete mode parameters.
func (c ModeChange) Validate() error {
	if c.Mode == nil {
		return &MissingModeError{}
	}
	if _, err := c.Change.addsMode(); err != nil {
		return err
	}

	_, _, err := c.Mode.parameter(c.Change)
	return err
}

// ModeChanges contains the ordered changes in one MODE command.
type ModeChanges []ModeChange

// JSONSchema returns the discriminated union accepted by the mode tool.
func (ModeChanges) JSONSchema() *invjsonschema.Schema {
	branches := make([]*invjsonschema.Schema, 0, len(modePrototypes)*2)
	for _, mode := range modePrototypes {
		branches = append(branches, modeSchemaBranch(mode, Add), modeSchemaBranch(mode, Remove))
	}

	return &invjsonschema.Schema{
		Type:  "array",
		Items: &invjsonschema.Schema{AnyOf: branches},
	}
}

// UnmarshalText parses the IRC command-line form into concrete mode changes.
func (c *ModeChanges) UnmarshalText(text []byte) error {
	fields := strings.Fields(string(text))
	if len(fields) == 0 {
		return &EmptyModeChangesError{}
	}

	changes, err := parseIRCModeChanges(fields[0], fields[1:])
	if err != nil {
		return err
	}

	*c = changes
	return nil
}

// UnmarshalJSON retains the array accepted by model tool calls.
func (c *ModeChanges) UnmarshalJSON(data []byte) error {
	type plainModeChanges []ModeChange

	var changes plainModeChanges
	if err := json.Unmarshal(data, &changes); err != nil {
		return err
	}

	*c = ModeChanges(changes)
	return nil
}

// Validate checks the batch bound and every concrete mode change.
func (c ModeChanges) Validate() error {
	if len(c) == 0 {
		return &EmptyModeChangesError{}
	}
	if len(c) > maxModeChanges {
		return &command.TooManyValuesError{Name: "changes", Maximum: maxModeChanges, Actual: len(c)}
	}

	for index, change := range c {
		if err := change.Validate(); err != nil {
			return &ModeChangeError{Index: index, Err: err}
		}
	}

	return nil
}

func (c ModeChanges) protocolChanges() ([]protocol.ChannelModeChange, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	changes := make([]protocol.ChannelModeChange, 0, len(c))
	for _, change := range c {
		add, _ := change.Change.addsMode()
		target, parameter, _ := change.Mode.parameter(change.Change)
		changes = append(changes, protocol.ChannelModeChange{
			Flag:   change.Mode.definition().flag,
			Add:    add,
			Target: target,
			Param:  parameter,
		})
	}

	return changes, nil
}

func modeSchemaBranch(mode Mode, change Change) *invjsonschema.Schema {
	definition := mode.definition()
	properties := invjsonschema.NewProperties()
	properties.Set("change", &invjsonschema.Schema{
		Type:        "string",
		Enum:        []any{change},
		Description: "Whether to add or remove the mode.",
	})
	properties.Set("mode", &invjsonschema.Schema{
		Type:        "string",
		Enum:        []any{definition.name},
		Description: definition.description,
	})

	required := []string{"change", "mode"}
	if change == Add || definition.flag.MemberMode() {
		reflector := invjsonschema.Reflector{DoNotReference: true}
		parameters := reflector.Reflect(mode)
		for pair := parameters.Properties.Oldest(); pair != nil; pair = pair.Next() {
			properties.Set(pair.Key, pair.Value)
			required = append(required, pair.Key)
		}
	}

	return &invjsonschema.Schema{
		Type:                 "object",
		Properties:           properties,
		Required:             required,
		AdditionalProperties: invjsonschema.FalseSchema,
	}
}

func modeForName(name string) (Mode, bool) {
	for _, mode := range modePrototypes {
		if mode.definition().name == name {
			return mode, true
		}
	}

	return nil, false
}

func modeForFlag(flag domain.Mode) (Mode, bool) {
	for _, mode := range modePrototypes {
		if mode.definition().flag == flag {
			return mode, true
		}
	}

	return nil, false
}

func parseIRCModeChanges(flags string, args []string) (ModeChanges, error) {
	if flags == "" {
		return nil, &EmptyModeChangesError{}
	}

	change := Add
	argumentIndex := 0
	changes := make(ModeChanges, 0, len(flags))

	for _, value := range flags {
		switch value {
		case '+':
			change = Add
			continue
		case '-':
			change = Remove
			continue
		}

		flag := domain.Mode(value)
		mode, ok := modeForFlag(flag)
		if !ok {
			return nil, domain.UnknownModeFlagError{Flag: flag}
		}

		needsParameter := flag.MemberMode() || change == Add && reflect.TypeOf(mode).NumField() > 0
		if needsParameter {
			if argumentIndex >= len(args) {
				return nil, domain.MissingModeParamError{Flag: flag}
			}

			var err error
			mode, err = modeWithParameter(mode, args[argumentIndex])
			if err != nil {
				return nil, err
			}
			argumentIndex++
		}

		changes = append(changes, ModeChange{Change: change, Mode: mode})
	}

	if len(changes) == 0 {
		return nil, &EmptyModeChangesError{}
	}
	if argumentIndex < len(args) {
		return nil, &SurplusModeArgumentsError{Arguments: append([]string(nil), args[argumentIndex:]...)}
	}
	if err := changes.Validate(); err != nil {
		return nil, err
	}

	return changes, nil
}

func modeWithParameter(mode Mode, raw string) (Mode, error) {
	target := reflect.New(reflect.TypeOf(mode)).Elem()
	if target.NumField() != 1 {
		return nil, &ModeParameterCountError{Mode: mode.definition().name, Count: target.NumField()}
	}

	field := target.Field(0)
	if field.CanAddr() && field.Addr().Type().Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
		decoder := field.Addr().Interface().(encoding.TextUnmarshaler)
		if err := decoder.UnmarshalText([]byte(raw)); err != nil {
			return nil, err
		}
	} else if field.Kind() == reflect.String {
		field.SetString(raw)
	} else {
		return nil, &ModeParameterTypeError{Mode: mode.definition().name, Type: field.Type()}
	}

	return target.Interface().(Mode), nil
}

func memberModeParameter(definition modeDefinition, target domain.Nick) (domain.Nick, string, error) {
	if target == "" {
		return "", "", domain.MissingModeParamError{Flag: definition.flag}
	}

	return target, "", nil
}

func positiveModeParameter(definition modeDefinition, change Change, value PositiveInt) (domain.Nick, string, error) {
	if change == Remove {
		if value != 0 {
			return "", "", &UnexpectedModeParameterError{Mode: definition.name, Field: "limit"}
		}

		return "", "", nil
	}
	if err := value.Validate(); err != nil {
		return "", "", domain.MissingModeParamError{Flag: definition.flag}
	}

	return "", strconv.Itoa(int(value)), nil
}

func (c Change) addsMode() (bool, error) {
	switch c {
	case Add:
		return true, nil
	case Remove:
		return false, nil
	default:
		return false, &UnknownModeOperationError{Operation: c}
	}
}

// EmptyModeChangesError reports a MODE call with no changes.
type EmptyModeChangesError struct{}

func (EmptyModeChangesError) Error() string {
	return "mode requires at least one change"
}

// MissingModeError reports a mode change without its mode value.
type MissingModeError struct{}

func (MissingModeError) Error() string {
	return "mode change requires a mode"
}

// UnknownModeOperationError reports an operation other than add or remove.
type UnknownModeOperationError struct {
	Operation Change
}

func (e UnknownModeOperationError) Error() string {
	return fmt.Sprintf("unknown mode operation %q", e.Operation)
}

// UnknownModeError reports a semantic mode name this build does not know.
type UnknownModeError struct {
	Mode string
}

func (e UnknownModeError) Error() string {
	return fmt.Sprintf("unknown mode %q", e.Mode)
}

// UnexpectedModeParameterError reports a parameter on a remove operation.
type UnexpectedModeParameterError struct {
	Mode  string
	Field string
}

func (e UnexpectedModeParameterError) Error() string {
	return fmt.Sprintf("mode %q does not accept %s for this change", e.Mode, e.Field)
}

// SurplusModeArgumentsError reports unused arguments after an IRC mode string.
type SurplusModeArgumentsError struct {
	Arguments []string
}

func (e SurplusModeArgumentsError) Error() string {
	return fmt.Sprintf("mode has %d surplus argument(s)", len(e.Arguments))
}

// InvalidPositiveIntError reports a non-positive or malformed integer.
type InvalidPositiveIntError struct {
	Value string
}

func (e InvalidPositiveIntError) Error() string {
	return fmt.Sprintf("expected a positive integer but got %q", e.Value)
}

// ModeChangeError identifies the invalid entry in a mode batch.
type ModeChangeError struct {
	Index int
	Err   error
}

func (e ModeChangeError) Error() string {
	return fmt.Sprintf("mode change %d: %s", e.Index, e.Err)
}

func (e ModeChangeError) Unwrap() error {
	return e.Err
}

// ModeParameterDecodeError reports invalid JSON fields for a concrete mode.
type ModeParameterDecodeError struct {
	Mode string
	Err  error
}

func (e ModeParameterDecodeError) Error() string {
	return fmt.Sprintf("decode parameters for mode %q: %s", e.Mode, e.Err)
}

func (e ModeParameterDecodeError) Unwrap() error {
	return e.Err
}

// ModeParameterCountError reports a mode type with an unsupported CLI shape.
type ModeParameterCountError struct {
	Mode  string
	Count int
}

func (e ModeParameterCountError) Error() string {
	return fmt.Sprintf("mode %q has %d parameter fields", e.Mode, e.Count)
}

// ModeParameterTypeError reports a mode parameter the CLI cannot decode.
type ModeParameterTypeError struct {
	Mode string
	Type reflect.Type
}

func (e ModeParameterTypeError) Error() string {
	return fmt.Sprintf("mode %q has unsupported parameter type %s", e.Mode, e.Type)
}
