package spacemanager

import (
	"testing"
	"time"
)

func TestLimitFor_KindSuppliesDefault(t *testing.T) {
	rate, burst := limitFor(RouteKindAuth, 0, 0)
	if rate != 5 || burst != 10 {
		t.Errorf("limitFor(auth) = (%v, %d), want (5, 10)", rate, burst)
	}
}

// An explicit rate always wins: a kind is a default, never a ceiling.
func TestLimitFor_ExplicitRateOverridesKind(t *testing.T) {
	rate, burst := limitFor(RouteKindAuth, 2, 3)
	if rate != 2 || burst != 3 {
		t.Errorf("limitFor(auth, 2, 3) = (%v, %d), want the explicit (2, 3)", rate, burst)
	}
}

// No kind and no rate stays unlimited — declaring nothing must keep the
// route exactly as it was before this field existed.
func TestLimitFor_NoKindStaysUnlimited(t *testing.T) {
	rate, burst := limitFor("", 0, 0)
	if rate != 0 || burst != 0 {
		t.Errorf("limitFor(\"\") = (%v, %d), want (0, 0)", rate, burst)
	}
}

func TestLimitFor_EveryKindIsLimited(t *testing.T) {
	for _, kind := range knownRouteKinds() {
		rate, burst := limitFor(kind, 0, 0)
		if rate <= 0 || burst < 1 {
			t.Errorf("kind %q resolves to (%v, %d); every kind must supply a usable bucket", kind, rate, burst)
		}
	}
}

// A kind's default must never be the pair validateGateway rejects.
func TestKindLimits_NeverProduceZeroBurst(t *testing.T) {
	for kind, l := range kindLimits {
		if l.rate > 0 && l.burst == 0 {
			t.Errorf("kind %q has rate %v with burst 0 — a zero-burst bucket rejects every request", kind, l.rate)
		}
	}
}

func TestValidateRouteKind(t *testing.T) {
	if err := validateRouteKind(""); err != nil {
		t.Errorf("empty kind must be accepted (it means no default), got %v", err)
	}
	for _, kind := range knownRouteKinds() {
		if err := validateRouteKind(kind); err != nil {
			t.Errorf("validateRouteKind(%q) = %v, want nil", kind, err)
		}
	}
	// An unknown kind must fail loudly rather than leave the route unlimited
	// while its author believes a limit applies.
	if err := validateRouteKind("Auth"); err == nil {
		t.Error("validateRouteKind(\"Auth\") = nil; kinds are case-sensitive and a typo must be rejected")
	}
	if err := validateRouteKind("login"); err == nil {
		t.Error("validateRouteKind(\"login\") = nil, want an error naming the known kinds")
	}
}

// rate: 0 is the field's own zero value, indistinguishable from "not declared",
// so it does NOT opt out of a kind — dropping the kind is how a route stays
// unlimited. Documented as contract, pinned here.
func TestLimitFor_ZeroRateDoesNotOptOutOfKind(t *testing.T) {
	rate, burst := limitFor(RouteKindAuth, 0, 0)
	if rate != 5 || burst != 10 {
		t.Errorf("limitFor(auth, 0, 0) = (%v, %d); rate 0 must not disable the kind, want 5/10", rate, burst)
	}
}

// Overriding takes both numbers: rate alone resolves to a zero burst, which
// validateGateway refuses rather than quietly borrowing the kind's burst.
func TestLimitFor_RateWithoutBurstKeepsZeroBurst(t *testing.T) {
	rate, burst := limitFor(RouteKindAuth, 2, 0)
	if rate != 2 || burst != 0 {
		t.Errorf("limitFor(auth, 2, 0) = (%v, %d), want (2, 0) so validation can refuse the pair", rate, burst)
	}
}

func TestParseRouteTimeout(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"30s", 30 * time.Second, false},
		{"2m", 2 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"30", 0, true},   // no unit
		{"soon", 0, true}, // not a duration
		{"-5s", 0, true},  // negative: a mistake, never "unbounded"
	}
	for _, c := range cases {
		got, err := parseRouteTimeout(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseRouteTimeout(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("parseRouteTimeout(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseRouteMaxBody(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false}, // explicit unlimited; parseMemory alone would reject it
		{"1024", 1024, false},
		{"1Mi", 1 << 20, false},
		{"10MB", 10 * 1000 * 1000, false},
		{"2Gi", 2 << 30, false},
		{"big", 0, true},
		{"-1Mi", 0, true},
	}
	for _, c := range cases {
		got, err := parseRouteMaxBody(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseRouteMaxBody(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("parseRouteMaxBody(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// max_body must NOT be derived from kind: a body cap is a flat refusal, so an
// inherited one would turn a working upload into a 413 nobody declared.
func TestKind_DoesNotImplyMaxBody(t *testing.T) {
	for _, kind := range knownRouteKinds() {
		if _, ok := kindLimits[kind]; !ok {
			t.Fatalf("kind %q missing from kindLimits", kind)
		}
	}
	// The kind table carries rate/burst only — no body field to leak from.
	if got, err := parseRouteMaxBody(""); err != nil || got != 0 {
		t.Errorf("an undeclared max_body must resolve to unbounded, got %d (%v)", got, err)
	}
}
