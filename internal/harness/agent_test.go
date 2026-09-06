package harness

import (
	"testing"
)

// withCleanRegistry empties the global registry for the duration of a test and
// puts the real one back afterwards, so a test's fake agents never leak into
// another test's view of what is installed.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	saved := registry
	registry = nil
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})
}

func TestTheDefaultRegistryHasClaudeCode(t *testing.T) {
	agent := FindAgent("claude")
	if agent == nil {
		t.Fatal("claude is not registered")
	}
	if agent.Name != "Claude Code" {
		t.Fatalf("agent name = %q, want %q", agent.Name, "Claude Code")
	}
	if agent.Detect == nil || agent.Checks == nil {
		t.Fatal("claude registered without a Detect or Checks function")
	}
}

func TestRegisterAgentMakesAnAgentFindable(t *testing.T) {
	withCleanRegistry(t)
	RegisterAgent(AgentInfo{Name: "Fake", ID: "fake", Detect: func(Env) bool { return true }})

	if all := AllAgents(); len(all) != 1 || all[0].ID != "fake" {
		t.Fatalf("AllAgents() = %+v, want the one fake agent", all)
	}
	if got := FindAgent("fake"); got == nil || got.Name != "Fake" {
		t.Fatalf("FindAgent(\"fake\") = %+v", got)
	}
	if got := FindAgent("nobody"); got != nil {
		t.Fatalf("FindAgent for an unregistered ID = %+v, want nil", got)
	}
}

func TestDetectedAgentsReturnsOnlyTheOnesDetectionFound(t *testing.T) {
	withCleanRegistry(t)
	RegisterAgent(AgentInfo{Name: "Here", ID: "here", Detect: func(Env) bool { return true }})
	RegisterAgent(AgentInfo{Name: "Gone", ID: "gone", Detect: func(Env) bool { return false }})
	RegisterAgent(AgentInfo{Name: "Silent", ID: "silent"}) // no Detect at all

	detected := DetectedAgents(envFrom(nil))
	if len(detected) != 1 || detected[0].ID != "here" {
		t.Fatalf("DetectedAgents() = %+v, want only \"here\"", detected)
	}
}

func TestRegisterAgentPanicsOnAnEmptyID(t *testing.T) {
	withCleanRegistry(t)
	defer func() {
		if recover() == nil {
			t.Fatal("registering an agent with no ID did not panic")
		}
	}()
	RegisterAgent(AgentInfo{Name: "Nameless"})
}

func TestRegisterAgentPanicsOnADuplicateID(t *testing.T) {
	withCleanRegistry(t)
	RegisterAgent(AgentInfo{Name: "First", ID: "dup"})
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate ID did not panic")
		}
	}()
	RegisterAgent(AgentInfo{Name: "Second", ID: "dup"})
}
