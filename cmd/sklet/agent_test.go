package main

import "testing"

// TestAgentCommandShape: the agent sub-command declares the expected name and
// flags (listen + space) with :6464 as the default control-API port —
// deliberately off the :9443/:9080 traefik front door. A regression here is what
// broke prod HTTPS once: the control API stealing the front-door port.
func TestAgentCommandShape(t *testing.T) {
	c := agentCommand()
	if c.Name != "agent" {
		t.Fatalf("Name = %q, want agent", c.Name)
	}
	byName := map[string]string{} // flag name -> default
	for _, f := range c.Flags {
		byName[f.Name] = f.StringDefault
	}
	if _, ok := byName["listen"]; !ok {
		t.Fatal("agent command missing --listen flag")
	}
	if got := byName["listen"]; got != ":6464" {
		t.Fatalf("--listen default = %q, want :6464 (must not be the :9443 front door)", got)
	}
	if _, ok := byName["space"]; !ok {
		t.Fatal("agent command missing --space flag")
	}
}

// TestAgentInTree: the agent command is wired under the sklet root, so the CLI
// dispatch can reach it.
func TestAgentInTree(t *testing.T) {
	found := false
	for _, s := range rootCommand().Sub {
		if s.Name == "agent" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("agent command not registered under the sklet root")
	}
}

// TestSuperviseInTree: the hidden __supervise daemon must stay reachable — it is
// what systemd and the setsid detach re-invoke on this binary.
func TestSuperviseInTree(t *testing.T) {
	for _, s := range rootCommand().Sub {
		if s.Name == "__supervise" {
			if !s.Hidden {
				t.Fatal("__supervise should stay hidden from the help surface")
			}
			return
		}
	}
	t.Fatal("__supervise not registered under the sklet root; systemd re-invocation would break")
}
