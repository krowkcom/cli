package harness

import (
	"testing"
)

// No test in this file may call t.Parallel: they swap the process-wide agent
// registry out and back, and two doing that at once would see each other's
// fake agents.

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
	RegisterAgent(fakeAgent("Fake", "fake", true))

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
	RegisterAgent(fakeAgent("Here", "here", true))
	RegisterAgent(fakeAgent("Gone", "gone", false))

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
	RegisterAgent(fakeAgent("Nameless", "", true))
}

func TestRegisterAgentPanicsOnADuplicateID(t *testing.T) {
	withCleanRegistry(t)
	RegisterAgent(fakeAgent("First", "dup", true))
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate ID did not panic")
		}
	}()
	RegisterAgent(fakeAgent("Second", "dup", true))
}

func TestRegisterAgentPanicsWithoutCallbacks(t *testing.T) {
	cases := map[string]AgentInfo{
		"no Detect": {Name: "Blind", ID: "blind", Checks: func(Env, string) []StatusCheck { return nil }},
		"no Checks": {Name: "Mute", ID: "mute", Detect: func(Env) bool { return true }},
	}
	for name, info := range cases {
		t.Run(name, func(t *testing.T) {
			withCleanRegistry(t)
			defer func() {
				if recover() == nil {
					t.Fatalf("registering an agent with %s did not panic", name)
				}
			}()
			RegisterAgent(info)
		})
	}
}

// fakeAgent is a registrable agent whose detection answer is fixed.
func fakeAgent(name, id string, detected bool) AgentInfo {
	return AgentInfo{
		Name:   name,
		ID:     id,
		Detect: func(Env) bool { return detected },
		Checks: func(Env, string) []StatusCheck { return nil },
	}
}
