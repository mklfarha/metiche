package mcp

import (
	"strings"
	"testing"
)

// This reproduces a failure that happened live, during a demo.
//
// One person, two agents on one machine. Neither agent could start a session:
// the server said "you have N active agents, so this call needs the client_key
// of the one making it", and neither agent knew its own. One guessed four
// times -- codex, backend, ui, tests -- and was rejected four times. The other
// looked for the value, could not find it, and refused to guess on the
// grounds that a wrong guess would file its work under a different agent on a
// board other people read. It was right to refuse, and that is the point: the
// information genuinely was not available to it.
//
// The connection now carries it. These tests run through the same resolver
// the tools use.
func TestIntegrationSeveralAgentsAndNoArgument(t *testing.T) {
	hs := newHarness(t)

	first := hs.join(t, "Maykel", "claude-1")
	_ = hs.rejoin(t, first, "Maykel", "codex-1")

	base := hs.ctxForToken(t, first.token)

	// The failure as it actually happened: several agents, no client_key.
	if _, err := hs.h.RequireAgent(base, "", ""); err == nil {
		t.Fatal("two agents and no way to tell them apart should be an error")
	} else {
		// The message has to point at the permanent fix, not just this call.
		if !strings.Contains(err.Error(), "X-Metiche-Client-Key") {
			t.Errorf("the error should name the header that fixes this for good, got: %v", err)
		}
	}

	// With the connection declaring who it is, it simply works.
	for _, want := range []string{"claude-1", "codex-1"} {
		res, err := hs.h.RequireAgent(WithClientKey(base, want), "", "")
		if err != nil {
			t.Fatalf("client key %q from the connection: %v", want, err)
		}
		if res.Agent.ClientKey != want {
			t.Fatalf("resolved agent %q, want %q", res.Agent.ClientKey, want)
		}
	}
}

// An explicit argument still wins, for a caller deliberately driving several
// of its own agents over one connection.
func TestIntegrationExplicitClientKeyBeatsTheHeader(t *testing.T) {
	hs := newHarness(t)
	first := hs.join(t, "Maykel", "claude-1")
	_ = hs.rejoin(t, first, "Maykel", "codex-1")

	ctx := WithClientKey(hs.ctxForToken(t, first.token), "claude-1")
	res, err := hs.h.RequireAgent(ctx, "", "codex-1")
	if err != nil {
		t.Fatalf("explicit client_key: %v", err)
	}
	if res.Agent.ClientKey != "codex-1" {
		t.Fatalf("resolved %q, want the explicit codex-1 to beat the header", res.Agent.ClientKey)
	}
}

// A header naming an agent that does not exist must be an ERROR, never a
// silently created agent -- otherwise a typo in a config file quietly spawns a
// second lane on the board that nobody is driving.
func TestIntegrationUnknownClientKeyHeaderIsRefused(t *testing.T) {
	hs := newHarness(t)
	c := hs.join(t, "Maykel", "claude-1")

	ctx := WithClientKey(hs.ctxForToken(t, c.token), "not-an-agent")
	if _, err := hs.h.RequireAgent(ctx, "", ""); err == nil {
		t.Fatal("an unknown client_key in the header was accepted; it must be refused")
	}
}

// One agent and no header is still fine: the single-agent case must not have
// been made harder by fixing the multi-agent one.
func TestIntegrationOneAgentStillNeedsNothing(t *testing.T) {
	hs := newHarness(t)
	c := hs.join(t, "Maykel", "only-one")

	res, err := hs.h.RequireAgent(hs.ctxForToken(t, c.token), "", "")
	if err != nil {
		t.Fatalf("one agent, no client_key anywhere: %v", err)
	}
	if res.Agent.ClientKey != "only-one" {
		t.Fatalf("resolved %q, want only-one", res.Agent.ClientKey)
	}
}
