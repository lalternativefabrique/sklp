package domain

import (
	"errors"
	"testing"
	"time"
)

func validWatch(t *testing.T) *Watch {
	t.Helper()
	w, err := NewWatch("stack-1", "acme-www", "registry.example.com/acme/www",
		"", "www", "https://141.95.197.144:6464", "", "tok", "ab12")
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	return w
}

// TestNewWatchDefaults: an omitted pattern and space fall back to the shape
// every sklp publish emits and the node's default space.
func TestNewWatchDefaults(t *testing.T) {
	w := validWatch(t)
	if w.TagPattern != DefaultTagPattern {
		t.Errorf("TagPattern = %q, want the default", w.TagPattern)
	}
	if w.NodeSpace != DefaultSpace {
		t.Errorf("NodeSpace = %q, want %q", w.NodeSpace, DefaultSpace)
	}
	if w.Status != StatusIdle || !w.Enabled {
		t.Errorf("status/enabled = %q/%v, want idle/true", w.Status, w.Enabled)
	}
}

// TestNewWatchRejectsTaggedRepo: the tag is what the watcher discovers, so a
// repository carrying one describes a watch that could never do anything.
func TestNewWatchRejectsTaggedRepo(t *testing.T) {
	_, err := NewWatch("s", "n", "rg.example.com/ns/www:1.0", "", "www", "https://h:6464", "", "tok", "")
	if err == nil {
		t.Fatal("expected an error for a tagged image repository")
	}
}

// TestNewWatchAcceptsRegistryPort: the colon in "host:5000/ns/img" belongs to
// the host, not to a tag — a private registry on a port must stay watchable.
func TestNewWatchAcceptsRegistryPort(t *testing.T) {
	if _, err := NewWatch("s", "n", "registry.internal:5000/ns/www", "", "www", "https://h:6464", "", "tok", ""); err != nil {
		t.Fatalf("NewWatch with a registry port: %v", err)
	}
}

func TestNewWatchValidation(t *testing.T) {
	cases := []struct {
		name                                                   string
		stack, wname, repo, pattern, svc, server, space, token string
	}{
		{"empty name", "s", "", "r/i", "", "www", "https://h:6464", "", "t"},
		{"empty repo", "s", "n", "", "", "www", "https://h:6464", "", "t"},
		{"empty service", "s", "n", "r/i", "", "", "https://h:6464", "", "t"},
		{"bad service name", "s", "n", "r/i", "", "Web_1", "https://h:6464", "", "t"},
		{"empty server", "s", "n", "r/i", "", "www", "", "", "t"},
		{"server without scheme", "s", "n", "r/i", "", "www", "141.95.197.144:6464", "", "t"},
		{"empty token", "s", "n", "r/i", "", "www", "https://h:6464", "", ""},
		{"bad pattern", "s", "n", "r/i", "([", "www", "https://h:6464", "", "t"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewWatch(c.stack, c.wname, c.repo, c.pattern, c.svc, c.server, c.space, c.token, "")
			if err == nil {
				t.Fatal("expected an error")
			}
			// Every rejection must carry ErrValidation so the API answers 400
			// without enumerating each cause.
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("error %v does not wrap ErrValidation", err)
			}
		})
	}
}

// TestNewerTag: the newest matching tag wins, non-matching tags are ignored, and
// a tag that is not strictly above the current one is not an update.
func TestNewerTag(t *testing.T) {
	w := validWatch(t)
	w.CurrentTag = "20260818-0331-d205d58"

	tags := []string{
		"latest",                // no match: not the immutable shape
		"20260817-1058-f4431b8", // older
		"20260818-0331-d205d58", // current
		"20260820-1349-bff7534", // newer
		"main",                  // no match
	}
	got, ok := w.NewerTag(tags)
	if !ok {
		t.Fatal("expected a newer tag")
	}
	if got != "20260820-1349-bff7534" {
		t.Fatalf("NewerTag = %q, want the newest matching tag", got)
	}

	// Nothing newer than the newest.
	w.CurrentTag = "20260820-1349-bff7534"
	if _, ok := w.NewerTag(tags); ok {
		t.Fatal("expected no update when already on the newest tag")
	}

	// A registry holding only unmatched tags yields nothing, never "latest".
	w.CurrentTag = ""
	if _, ok := w.NewerTag([]string{"latest", "main"}); ok {
		t.Fatal("expected no update from tags outside the pattern")
	}
}

// TestNewerTagFirstRoll: a watch that has never rolled adopts the newest tag.
func TestNewerTagFirstRoll(t *testing.T) {
	w := validWatch(t)
	got, ok := w.NewerTag([]string{"20260817-1058-f4431b8", "20260820-1349-bff7534"})
	if !ok || got != "20260820-1349-bff7534" {
		t.Fatalf("NewerTag = %q/%v, want the newest tag on a first roll", got, ok)
	}
}

func TestImageRef(t *testing.T) {
	w := validWatch(t)
	if got := w.ImageRef("20260820-1349-bff7534"); got != "registry.example.com/acme/www:20260820-1349-bff7534" {
		t.Fatalf("ImageRef = %q", got)
	}
}

func TestNewerTagSkipsHeldTag(t *testing.T) {
	w := &Watch{TagPattern: DefaultTagPattern, CurrentTag: "20260904-0816-1886fc44e7c4", HeldTag: "20260904-0848-c05895e956ba"}
	if tag, ok := w.NewerTag([]string{"20260904-0816-1886fc44e7c4", "20260904-0848-c05895e956ba"}); ok {
		t.Fatalf("held tag %q offered again", tag)
	}
	if tag, ok := w.NewerTag([]string{"20260904-0848-c05895e956ba", "20260904-0930-0123456789ab"}); !ok || tag != "20260904-0930-0123456789ab" {
		t.Fatalf("newer tag past the hold not offered: %q %v", tag, ok)
	}
}

// The roll transitions used to live in SQL, one copy per repository. These
// pin them where both adapters now read them from.

func TestClaimForRefusesAWatchAlreadyRolling(t *testing.T) {
	w := &Watch{Status: StatusRolling, CurrentTag: "a"}
	if w.ClaimFor("b", time.Now()) {
		t.Fatal("claimed a watch that was already rolling")
	}
	if w.Status != StatusRolling {
		t.Errorf("status = %q, want it untouched by a refused claim", w.Status)
	}
}

// Rolling onto the tag already live is not a roll: without this the dispatcher
// would restart a service to hand it the image it is already running.
func TestClaimForRefusesTheCurrentTag(t *testing.T) {
	w := &Watch{Status: StatusIdle, CurrentTag: "20260904-0807"}
	if w.ClaimFor("20260904-0807", time.Now()) {
		t.Fatal("claimed a roll onto the tag already current")
	}
}

func TestClaimForTakesAnIdleWatch(t *testing.T) {
	now := time.Now()
	w := &Watch{Status: StatusIdle, CurrentTag: "old"}
	if !w.ClaimFor("new", now) {
		t.Fatal("an idle watch on another tag must be claimable")
	}
	if w.Status != StatusRolling {
		t.Errorf("status = %q, want rolling", w.Status)
	}
	if !w.UpdatedAt.Equal(now.UTC()) {
		t.Errorf("UpdatedAt = %v, want the claim time", w.UpdatedAt)
	}
}

// An operator's pin is on the tag they chose. A roll onto a DIFFERENT tag
// wins, so the pin is stale and must go — otherwise the watch stays pointed at
// a tag nothing is rolling to.
func TestClaimForClearsAPinForAnotherTag(t *testing.T) {
	w := &Watch{Status: StatusIdle, CurrentTag: "old", PinnedTag: "pinned"}
	if !w.ClaimFor("new", time.Now()) {
		t.Fatal("claim refused")
	}
	if w.PinnedTag != "" {
		t.Errorf("PinnedTag = %q, want it cleared by a roll to another tag", w.PinnedTag)
	}
}

func TestClaimForKeepsAPinForTheSameTag(t *testing.T) {
	w := &Watch{Status: StatusIdle, CurrentTag: "old", PinnedTag: "new"}
	if !w.ClaimFor("new", time.Now()) {
		t.Fatal("claim refused")
	}
	if w.PinnedTag != "new" {
		t.Errorf("PinnedTag = %q, want the pin kept when the roll honours it", w.PinnedTag)
	}
}

// "applied" means the node accepted the change, not that it worked. Advancing
// the tag here would report a version as live before the rollout settled — and
// leave it advanced if the rollout then failed.
func TestFinishLeavesAnAppliedRollRolling(t *testing.T) {
	w := &Watch{Status: StatusRolling, CurrentTag: "old"}
	w.Finish(&Roll{ToTag: "new", Result: RollResultApplied}, time.Now())
	if w.Status != StatusRolling || w.CurrentTag != "old" {
		t.Fatalf("status=%q tag=%q, want the watch still rolling on the old tag", w.Status, w.CurrentTag)
	}
}

func TestFinishAdvancesTheTagWhenLive(t *testing.T) {
	now := time.Now()
	w := &Watch{Status: StatusRolling, CurrentTag: "old", LastError: "stale"}
	w.Finish(&Roll{ToTag: "new", Result: RollResultLive}, now)
	if w.Status != StatusIdle || w.CurrentTag != "new" {
		t.Fatalf("status=%q tag=%q, want idle on the new tag", w.Status, w.CurrentTag)
	}
	if w.LastError != "" {
		t.Errorf("LastError = %q, want a success to clear it", w.LastError)
	}
	if w.LastRolledAt == nil {
		t.Error("LastRolledAt not recorded")
	}
}

func TestFinishFailureKeepsTheTag(t *testing.T) {
	w := &Watch{Status: StatusRolling, CurrentTag: "old"}
	w.Finish(&Roll{ToTag: "new", Result: "failed", ErrorMessage: "pull denied"}, time.Now())
	if w.Status != StatusFailed || w.CurrentTag != "old" {
		t.Fatalf("status=%q tag=%q, want failed on the tag still running", w.Status, w.CurrentTag)
	}
	if w.LastError != "pull denied" {
		t.Errorf("LastError = %q, want the reason", w.LastError)
	}
}

// A rollback holds the tag rather than failing the watch: the image is bad,
// not the watch, so the poller must skip that tag and keep looking for a newer
// one instead of stopping.
func TestSettleHoldsARolledBackTag(t *testing.T) {
	w := &Watch{Status: StatusRolling, CurrentTag: "old", PinnedTag: "new"}
	w.Settle(&Roll{ToTag: "new"}, RollOutcome{
		Result: RollResultRolledBack, Reason: "probe failed", FinishedAt: time.Now(),
	})
	if w.Status != StatusHeld || w.HeldTag != "new" {
		t.Fatalf("status=%q held=%q, want the bad tag held", w.Status, w.HeldTag)
	}
	if w.CurrentTag != "old" {
		t.Errorf("CurrentTag = %q, want the tag that still runs", w.CurrentTag)
	}
	if w.PinnedTag != "" {
		t.Errorf("PinnedTag = %q, want a rollback to clear the operator's pin", w.PinnedTag)
	}
}

func TestSettleLiveReleasesTheHold(t *testing.T) {
	w := &Watch{Status: StatusRolling, CurrentTag: "old", HeldTag: "new", LastError: "earlier"}
	w.Settle(&Roll{ToTag: "new"}, RollOutcome{Result: RollResultLive, FinishedAt: time.Now()})
	if w.Status != StatusIdle || w.CurrentTag != "new" {
		t.Fatalf("status=%q tag=%q, want idle on the new tag", w.Status, w.CurrentTag)
	}
	if w.HeldTag != "" || w.LastError != "" {
		t.Errorf("held=%q err=%q, want both cleared once the tag is live", w.HeldTag, w.LastError)
	}
}

// A hold covers every tag up to the held one: two rolled-back tags must not
// take turns being retried, and only a tag newer than the hold rolls.
func TestNewerTagHoldsEveryTagUpToTheHeldOne(t *testing.T) {
	w := &Watch{TagPattern: DefaultTagPattern, CurrentTag: "20260909-1749-ef32ad056751", HeldTag: "20260910-0545-b09125cda3c0"}
	if tag, ok := w.NewerTag([]string{"20260909-1749-ef32ad056751", "20260909-1759-2a9a94922295", "20260910-0545-b09125cda3c0"}); ok {
		t.Fatalf("tag %q offered while an equal or newer tag is held", tag)
	}
	if tag, ok := w.NewerTag([]string{"20260909-1759-2a9a94922295", "20260910-0545-b09125cda3c0", "20260910-0700-0123456789ab"}); !ok || tag != "20260910-0700-0123456789ab" {
		t.Fatalf("tag newer than the hold not offered: %q %v", tag, ok)
	}
}

func TestSettleKeepsTheHighestHeldTag(t *testing.T) {
	w := &Watch{Status: StatusRolling, CurrentTag: "20260909-1749-ef32ad056751", HeldTag: "20260910-0545-b09125cda3c0"}
	w.Settle(&Roll{ToTag: "20260909-1759-2a9a94922295"}, RollOutcome{Result: RollResultRolledBack, Reason: "crash", FinishedAt: time.Now()})
	if w.HeldTag != "20260910-0545-b09125cda3c0" {
		t.Fatalf("HeldTag = %q, want the newer rolled-back tag kept", w.HeldTag)
	}
	w.Settle(&Roll{ToTag: "20260910-0800-abcdef012345"}, RollOutcome{Result: RollResultRolledBack, Reason: "crash", FinishedAt: time.Now()})
	if w.HeldTag != "20260910-0800-abcdef012345" {
		t.Fatalf("HeldTag = %q, want the hold raised to the newest rolled-back tag", w.HeldTag)
	}
}
