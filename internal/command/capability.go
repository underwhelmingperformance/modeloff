package command

// Capability names a runtime predicate that gates a command's
// visibility on a command surface — completion popovers, `/help`
// output, and the model tool registry. Values are open: concrete
// capabilities live with their consumers (e.g. `protocol.CapOperator`).
// The framework knows only the type.
type Capability string

// CapabilityHolder is the runtime side of the contract: it answers
// whether a capability is currently held by the holder. Both surfaces
// (the chat-screen completion context and the model tool registry
// filter) pass a holder when asking "which commands are visible right
// now".
//
// A nil holder is treated as holding no capabilities — every command
// with a non-empty [Node.RequiredCapabilities] is filtered out.
type CapabilityHolder interface {
	Has(Capability) bool
}

// NoCapabilities returns a holder that grants nothing. Useful as the
// zero value for code paths that need to enumerate commands without a
// concrete holder yet (e.g. tests, default unfiltered displays before
// the runtime context is wired).
func NoCapabilities() CapabilityHolder { return noCapabilities{} }

type noCapabilities struct{}

func (noCapabilities) Has(Capability) bool { return false }

// Holds returns true if the holder grants every capability in caps.
// A nil or empty caps slice is trivially satisfied.
func Holds(holder CapabilityHolder, caps []Capability) bool {
	if len(caps) == 0 {
		return true
	}

	if holder == nil {
		return false
	}

	for _, cap := range caps {
		if !holder.Has(cap) {
			return false
		}
	}

	return true
}
