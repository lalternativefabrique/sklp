package control

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// The rollout result values are declared twice: the supervisor owns them, and
// protocol.go restates them so a client can read a RolloutState without the
// embedded-only build tag. Two copies of the same literals drift, and a
// drifted one would silently match no rollout at all.
func TestRolloutResultsMatchTheSupervisor(t *testing.T) {
	for _, c := range []struct{ wire, supervisor string }{
		{RolloutRolling, spacemanager.RolloutRolling},
		{RolloutSoaking, spacemanager.RolloutSoaking},
		{RolloutSucceeded, spacemanager.RolloutSucceeded},
		{RolloutRolledBack, spacemanager.RolloutRolledBack},
		{RolloutSuperseded, spacemanager.RolloutSuperseded},
	} {
		if c.wire != c.supervisor {
			t.Errorf("wire constant %q does not match the supervisor's %q", c.wire, c.supervisor)
		}
	}
}

// encoding/json never omits a struct, so a zero time.Time would ship as
// "0001-01-01T00:00:00Z" — an instant in year 1, indistinguishable from a real
// one to every consumer down the chain. The timestamps are pointers for that
// reason, and this guards the reason from being refactored away.
func TestZeroInstanceTimestampsStayOffTheWire(t *testing.T) {
	body, err := json.Marshal(InstanceState{ID: "core-0", State: "pending"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"started_at", "restart_at"} {
		if bytes.Contains(body, []byte(field)) {
			t.Errorf("%s is present on an instance that never ran: %s", field, body)
		}
	}
	if bytes.Contains(body, []byte("0001-01-01")) {
		t.Errorf("a zero instant reached the wire: %s", body)
	}
}

func TestSetInstanceTimestampsReachTheWire(t *testing.T) {
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	body, err := json.Marshal(InstanceState{ID: "core-0", State: "ready", StartedAt: &at})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(body, []byte("2026-09-19T12:00:00Z")) {
		t.Errorf("a real instant did not survive: %s", body)
	}
}
