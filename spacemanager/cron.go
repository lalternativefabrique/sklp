package spacemanager

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSchedule is a parsed 5-field cron expression (minute hour dom month dow).
// It is deliberately minimal — `*`, single values, comma lists and `*/step` —
// enough for backup schedules without pulling a cron dependency. Ranges (a-b)
// and named months/days are NOT supported.
type CronSchedule struct {
	expr   string
	minute fieldMatcher
	hour   fieldMatcher
	dom    fieldMatcher
	month  fieldMatcher
	dow    fieldMatcher
}

// fieldMatcher is the set of allowed values for one cron field. A nil set means
// "*" (any value).
type fieldMatcher map[int]struct{}

// ParseCron parses a 5-field cron expression. dow 0 and 7 both mean Sunday.
func ParseCron(expr string) (CronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return CronSchedule{}, fmt.Errorf("cron %q: want 5 fields (min hour dom month dow), got %d", expr, len(fields))
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron %q minute: %w", expr, err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron %q hour: %w", expr, err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron %q day-of-month: %w", expr, err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron %q month: %w", expr, err)
	}
	dow, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return CronSchedule{}, fmt.Errorf("cron %q day-of-week: %w", expr, err)
	}
	// Normalise Sunday 7 -> 0 so Match only ever checks 0.
	if dow != nil {
		if _, ok := dow[7]; ok {
			dow[0] = struct{}{}
			delete(dow, 7)
		}
	}
	return CronSchedule{expr: expr, minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

// parseCronField parses one field into a matcher over [min,max]. "*" -> nil.
func parseCronField(field string, min, max int) (fieldMatcher, error) {
	if field == "*" {
		return nil, nil
	}
	set := fieldMatcher{}
	for _, part := range strings.Split(field, ",") {
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			step, err := strconv.Atoi(stepStr)
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("bad step %q", part)
			}
			if base != "*" {
				return nil, fmt.Errorf("step base must be * (got %q)", part)
			}
			for v := min; v <= max; v += step {
				set[v] = struct{}{}
			}
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("bad value %q", part)
		}
		if v < min || v > max {
			return nil, fmt.Errorf("value %d out of range [%d,%d]", v, min, max)
		}
		set[v] = struct{}{}
	}
	return set, nil
}

// Match reports whether t falls on this schedule (minute precision). Per cron
// convention, when BOTH day-of-month and day-of-week are restricted the entry
// fires if EITHER matches; otherwise the restricted one(s) must match.
func (c CronSchedule) Match(t time.Time) bool {
	if !c.minute.has(t.Minute()) || !c.hour.has(t.Hour()) || !c.month.has(int(t.Month())) {
		return false
	}
	domRestricted := c.dom != nil
	dowRestricted := c.dow != nil
	domOK := c.dom.has(t.Day())
	dowOK := c.dow.has(int(t.Weekday()))
	if domRestricted && dowRestricted {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// has reports membership; a nil matcher ("*") matches everything.
func (f fieldMatcher) has(v int) bool {
	if f == nil {
		return true
	}
	_, ok := f[v]
	return ok
}
