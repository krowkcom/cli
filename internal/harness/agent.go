package harness

import "sync"

// AgentInfo is one coding agent: what to call it, how to tell whether it is
// installed, and what to check once it is. Both callbacks take an Env rather
// than reading the process environment, so a caller can ask the question about
// a home directory other than its own.
type AgentInfo struct {
	Name   string // "Claude Code", as a person would say it
	ID     string // "claude", as a flag or JSON key would spell it
	Detect func(env Env) bool
	Checks func(env Env, cwd string) []StatusCheck
}

var (
	registryMu sync.RWMutex
	registry   []AgentInfo
)

// RegisterAgent adds an agent to the global registry, from the init() of the
// file that defines it. An empty or duplicate ID, or a missing callback, is a
// programming mistake in this repository rather than anything a user did, so
// it panics: a registry with two "claude" entries, or with an agent that
// cannot say whether it is installed, has no defined meaning and no useful
// recovery. Every registered agent is therefore safe to call.
func RegisterAgent(info AgentInfo) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if info.ID == "" {
		panic("harness: RegisterAgent called with empty agent ID")
	}
	for i := range registry {
		if registry[i].ID == info.ID {
			panic("harness: RegisterAgent called with duplicate agent ID: " + info.ID)
		}
	}
	if info.Detect == nil {
		panic("harness: RegisterAgent called without a Detect function: " + info.ID)
	}
	if info.Checks == nil {
		panic("harness: RegisterAgent called without a Checks function: " + info.ID)
	}
	registry = append(registry, info)
}

// AllAgents returns every registered agent, in registration order.
func AllAgents() []AgentInfo {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]AgentInfo, len(registry))
	copy(out, registry)
	return out
}

// DetectedAgents returns the agents Detect says are installed. The registry is
// copied before any callback runs: detection touches the filesystem, and a
// lock held across disk I/O is a lock held for an unbounded time.
func DetectedAgents(env Env) []AgentInfo {
	var detected []AgentInfo
	for _, a := range AllAgents() {
		if a.Detect(env) {
			detected = append(detected, a)
		}
	}
	return detected
}

// FindAgent returns the agent with the given ID, or nil when nothing
// registered it.
func FindAgent(id string) *AgentInfo {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for i := range registry {
		if registry[i].ID == id {
			info := registry[i]
			return &info
		}
	}
	return nil
}
