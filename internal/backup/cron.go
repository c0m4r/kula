package backup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed standard 5-field crontab expression:
//
//	minute  hour  day-of-month  month  day-of-week
//	0-59    0-23  1-31          1-12   0-6 (0 = Sunday, 7 also accepted)
//
// Each field supports "*", lists ("a,b"), ranges ("a-b"), and steps ("*/n",
// "a-b/n", "a/n"). Day-of-month and day-of-week follow the conventional Vixie
// cron rule: when both are restricted (neither is "*") a tick matches if either
// field matches; otherwise both must match.
type Schedule struct {
	minute  uint64 // bits 0-59
	hour    uint64 // bits 0-23
	dom     uint64 // bits 1-31
	month   uint64 // bits 1-12
	dow     uint64 // bits 0-6
	domStar bool
	dowStar bool
}

// ParseSchedule parses a 5-field crontab expression. Fields are separated by
// runs of whitespace.
func ParseSchedule(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression %q must have exactly 5 fields, got %d", expr, len(fields))
	}

	minute, _, err := parseField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("cron minute field: %w", err)
	}
	hour, _, err := parseField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("cron hour field: %w", err)
	}
	dom, domStar, err := parseField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("cron day-of-month field: %w", err)
	}
	month, _, err := parseField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("cron month field: %w", err)
	}
	dow, dowStar, err := parseField(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("cron day-of-week field: %w", err)
	}
	// Normalize Sunday: accept 7 and fold it onto 0.
	if dow&(1<<7) != 0 {
		dow |= 1
		dow &^= 1 << 7
	}

	return &Schedule{
		minute:  minute,
		hour:    hour,
		dom:     dom,
		month:   month,
		dow:     dow,
		domStar: domStar,
		dowStar: dowStar,
	}, nil
}

// Matches reports whether the schedule fires at minute t (seconds and below are
// ignored — cron resolution is one minute).
func (s *Schedule) Matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if s.month&(1<<uint(t.Month())) == 0 {
		return false
	}

	domMatch := s.dom&(1<<uint(t.Day())) != 0
	dowMatch := s.dow&(1<<uint(int(t.Weekday()))) != 0

	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowMatch
	case s.dowStar:
		return domMatch
	default:
		// Both restricted: OR semantics (classic Vixie cron behavior).
		return domMatch || dowMatch
	}
}

// parseField parses a single crontab field into a bitmask over [min, max] and
// reports whether the field is a bare "*" (relevant only for dom/dow).
func parseField(field string, min, max int) (uint64, bool, error) {
	star := field == "*"
	var bits uint64

	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, false, fmt.Errorf("empty term in %q", field)
		}

		step := 1
		rng := part
		if slash := strings.Index(part, "/"); slash >= 0 {
			rng = part[:slash]
			st, err := strconv.Atoi(part[slash+1:])
			if err != nil || st < 1 {
				return 0, false, fmt.Errorf("invalid step in %q", part)
			}
			step = st
		}

		var lo, hi int
		switch {
		case rng == "*":
			lo, hi = min, max
		case strings.Contains(rng, "-"):
			bounds := strings.SplitN(rng, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil {
				return 0, false, fmt.Errorf("invalid range %q", rng)
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(rng)
			if err != nil {
				return 0, false, fmt.Errorf("invalid value %q", rng)
			}
			lo = v
			// A single value with a step ("a/n") means "a, a+n, ... up to max".
			if step > 1 {
				hi = max
			} else {
				hi = v
			}
		}

		if lo < min || hi > max || lo > hi {
			return 0, false, fmt.Errorf("value out of range in %q (allowed %d-%d)", part, min, max)
		}
		for i := lo; i <= hi; i += step {
			bits |= 1 << uint(i)
		}
	}

	return bits, star, nil
}
