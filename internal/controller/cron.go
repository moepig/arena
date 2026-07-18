package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronExpr is a minimal 5-field cron matcher (minute hour day-of-month
// month day-of-week) for the Schedule autoscaling policy. Fields support
// "*", "N", "N-M", "*/S", "N-M/S" and comma lists. Standard cron semantics:
// when both day-of-month and day-of-week are restricted, either matching
// suffices.
type cronExpr struct {
	minute, hour, dom, month, dow map[int]bool
	domStar, dowStar              bool
}

func parseCron(expr string) (*cronExpr, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields, got %d", expr, len(fields))
	}
	c := &cronExpr{}
	var err error
	if c.minute, _, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("cron %q minute: %w", expr, err)
	}
	if c.hour, _, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("cron %q hour: %w", expr, err)
	}
	if c.dom, c.domStar, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("cron %q day-of-month: %w", expr, err)
	}
	if c.month, _, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("cron %q month: %w", expr, err)
	}
	if c.dow, c.dowStar, err = parseCronField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("cron %q day-of-week: %w", expr, err)
	}
	if c.dow[7] { // 7 ≡ Sunday ≡ 0
		c.dow[0] = true
	}
	return c, nil
}

func parseCronField(field string, lo, hi int) (set map[int]bool, star bool, err error) {
	set = map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		rangePart, stepPart, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			if step, err = strconv.Atoi(stepPart); err != nil || step < 1 {
				return nil, false, fmt.Errorf("bad step %q", part)
			}
		}
		start, end := lo, hi
		switch {
		case rangePart == "*":
			if !hasStep && len(field) == 1 {
				star = true
			}
		case strings.Contains(rangePart, "-"):
			a, b, _ := strings.Cut(rangePart, "-")
			if start, err = strconv.Atoi(a); err != nil {
				return nil, false, fmt.Errorf("bad range %q", part)
			}
			if end, err = strconv.Atoi(b); err != nil {
				return nil, false, fmt.Errorf("bad range %q", part)
			}
		default:
			if start, err = strconv.Atoi(rangePart); err != nil {
				return nil, false, fmt.Errorf("bad value %q", part)
			}
			if !hasStep {
				end = start
			}
		}
		if start < lo || end > hi || start > end {
			return nil, false, fmt.Errorf("value out of range [%d,%d]: %q", lo, hi, part)
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	return set, star, nil
}

func (c *cronExpr) matches(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}
	domOK := c.dom[t.Day()]
	dowOK := c.dow[int(t.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		return domOK || dowOK // standard cron OR semantics
	}
}

// lastMatch returns the most recent minute ≤ now the expression matched,
// scanning back at most lookback. ok=false when nothing matched.
func (c *cronExpr) lastMatch(now time.Time, lookback time.Duration) (time.Time, bool) {
	t := now.Truncate(time.Minute)
	for end := now.Add(-lookback); !t.Before(end); t = t.Add(-time.Minute) {
		if c.matches(t) {
			return t, true
		}
	}
	return time.Time{}, false
}
