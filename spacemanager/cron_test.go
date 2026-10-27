package spacemanager

import (
	"testing"
	"time"
)

func TestParseCron_Errors(t *testing.T) {
	bad := []string{
		"",            // empty
		"* * * *",     // 4 fields
		"* * * * * *", // 6 fields
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // dom min is 1
		"* * * 13 *",  // month out of range
		"* * * * 8",   // dow max is 7
		"x * * * *",   // non-numeric
		"*/0 * * * *", // zero step
		"1/2 * * * *", // step base not *
	}
	for _, expr := range bad {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q): want error, got nil", expr)
		}
	}
}

func TestCronMatch_FixedTime(t *testing.T) {
	// 0 3 * * *  — daily at 03:00.
	c, err := ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 6, 7, h, m, 0, 0, time.UTC) }
	if !c.Match(at(3, 0)) {
		t.Error("want match at 03:00")
	}
	if c.Match(at(3, 1)) {
		t.Error("want no match at 03:01")
	}
	if c.Match(at(4, 0)) {
		t.Error("want no match at 04:00")
	}
}

func TestCronMatch_Step(t *testing.T) {
	// every 15 minutes.
	c, err := ParseCron("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	at := func(m int) time.Time { return time.Date(2026, 6, 7, 10, m, 0, 0, time.UTC) }
	for _, m := range []int{0, 15, 30, 45} {
		if !c.Match(at(m)) {
			t.Errorf("want match at minute %d", m)
		}
	}
	for _, m := range []int{1, 14, 31} {
		if c.Match(at(m)) {
			t.Errorf("want no match at minute %d", m)
		}
	}
}

func TestCronMatch_ListAndDOW(t *testing.T) {
	// At minute 0 of hours 0 and 12, on Sundays (0).
	c, err := ParseCron("0 0,12 * * 0")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-06-07 is a Sunday.
	sun := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	if sun.Weekday() != time.Sunday {
		t.Fatalf("fixture date is %s, expected Sunday", sun.Weekday())
	}
	if !c.Match(sun) {
		t.Error("want match Sunday 12:00")
	}
	mon := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	if c.Match(mon) {
		t.Error("want no match Monday 12:00")
	}
}

func TestCronMatch_SundaySevenNormalises(t *testing.T) {
	c, err := ParseCron("0 0 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	sun := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC) // Sunday
	if !c.Match(sun) {
		t.Error("dow=7 should match Sunday")
	}
}
