// Package schedule computes alarm fire times for the two kinds: once (a
// relative delay or an absolute instant) and cron (a standard 5-field
// expression / @descriptor / @every, evaluated in the alarm's IANA timezone via
// robfig/cron/v3).
package schedule

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser accepts the standard 5-field syntax plus @descriptors
// (@yearly/@monthly/@weekly/@daily/@hourly) and @every Nm — the day-one
// surface the spec mandates.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// LoadLocation resolves an IANA timezone, defaulting to UTC when empty.
func LoadLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("schedule: invalid timezone %q: %w", tz, err)
	}
	return loc, nil
}

// ParseCron validates a cron expression, returning the compiled schedule.
func ParseCron(expr string) (cron.Schedule, error) {
	if expr == "" {
		return nil, errors.New("schedule: empty cron expression")
	}
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("schedule: invalid cron expression %q: %w", expr, err)
	}
	return sched, nil
}

// NextCron returns the next fire time strictly after `after`, evaluated in tz.
func NextCron(expr, tz string, after time.Time) (time.Time, error) {
	sched, err := ParseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := LoadLocation(tz)
	if err != nil {
		return time.Time{}, err
	}
	next := sched.Next(after.In(loc))
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("schedule: cron %q yields no future time", expr)
	}
	return next.UTC(), nil
}

// NextOnce resolves the single fire instant for a once alarm from exactly one
// of delaySeconds (relative to now) or fireAt (RFC3339 absolute). The resolved
// time must be in the future.
func NextOnce(delaySeconds int64, fireAt string, now time.Time) (time.Time, error) {
	hasDelay := delaySeconds > 0
	hasAbs := fireAt != ""
	switch {
	case hasDelay && hasAbs:
		return time.Time{}, errors.New("schedule: once alarm takes either delay_seconds or fire_at, not both")
	case hasDelay:
		return now.Add(time.Duration(delaySeconds) * time.Second).UTC(), nil
	case hasAbs:
		t, err := time.Parse(time.RFC3339, fireAt)
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule: invalid fire_at %q (want RFC3339): %w", fireAt, err)
		}
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("schedule: fire_at %s is not in the future", t.Format(time.RFC3339))
		}
		return t.UTC(), nil
	default:
		return time.Time{}, errors.New("schedule: once alarm requires delay_seconds or fire_at")
	}
}
