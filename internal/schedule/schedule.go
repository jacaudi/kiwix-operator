// Package schedule decides when a ZimArchive is next due to be refreshed.
package schedule

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// Triggers say why an archive is due.
const (
	TriggerRequested = "requested"
	TriggerInitial   = "initial"
	TriggerBackoff   = "backoff"
	TriggerSchedule  = "schedule"
)

// failureNotifyInterval is the minimum gap between repeat failure notifications.
const failureNotifyInterval = 24 * time.Hour

// DueInput is everything the due rules need; the caller derives it from the ZimArchive.
type DueInput struct {
	HasCurrent, EverAttempted, LastFailedOrRejected, Running, AwaitingAdoption bool
	FailedAttempts                                                             int32
	LastFinished, CreatedAt                                                    time.Time
	Schedule, TimeZone                                                         string
	LastScheduledAt                                                            time.Time
	RequestedAt, LastHandledRequestedAt                                        string
}

// Due is the verdict; Next is the earliest future firing time (zero if none, meaningful only when not Due); SkippedTick passed during a run.
type Due struct {
	Due                             bool
	Trigger                         string
	ScheduleTick, Next, SkippedTick time.Time
}

// Evaluate applies the due rules in priority order; all returned times are UTC.
func Evaluate(in DueInput, now time.Time) (Due, error) {
	if in.AwaitingAdoption {
		return Due{}, nil
	}
	sched, loc, err := parse(in.Schedule, in.TimeZone)
	if err != nil {
		return Due{}, err
	}
	baseline := in.LastScheduledAt
	if baseline.IsZero() {
		baseline = in.CreatedAt
	}
	tick := latestTick(sched, loc, baseline, now)

	if in.Running {
		return Due{SkippedTick: tick}, nil
	}
	if in.RequestedAt != in.LastHandledRequestedAt {
		return Due{Due: true, Trigger: TriggerRequested}, nil
	}
	if !in.HasCurrent && !in.EverAttempted {
		return Due{Due: true, Trigger: TriggerInitial}, nil
	}
	var next time.Time
	if !in.HasCurrent && in.LastFailedOrRejected {
		at := in.LastFinished.Add(Backoff(in.FailedAttempts))
		if !now.Before(at) {
			return Due{Due: true, Trigger: TriggerBackoff}, nil
		}
		next = at.UTC()
	}
	if !tick.IsZero() {
		return Due{Due: true, Trigger: TriggerSchedule, ScheduleTick: tick}, nil
	}
	return Due{Next: earliest(next, nextTick(sched, loc, now))}, nil
}

// earliest returns the earlier of two times, treating zero as "none".
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// nextTick returns the first tick after now in UTC, or zero when there is none or no schedule.
func nextTick(sched cron.Schedule, loc *time.Location, now time.Time) time.Time {
	if sched == nil {
		return time.Time{}
	}
	return sched.Next(now.In(loc)).UTC()
}

// parse returns a nil schedule for an empty spec; an empty zone is UTC.
func parse(spec, zone string) (cron.Schedule, *time.Location, error) {
	if spec == "" {
		return nil, time.UTC, nil
	}
	loc := time.UTC
	if zone != "" {
		var err error
		if loc, err = time.LoadLocation(zone); err != nil {
			return nil, nil, fmt.Errorf("loading time zone %q: %w", zone, err)
		}
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing schedule %q: %w", spec, err)
	}
	return sched, loc, nil
}

// latestTick returns the latest tick in (after, now] in UTC, or zero when none; ponytail: walks every tick since the baseline, O(ticks) after a long outage.
func latestTick(sched cron.Schedule, loc *time.Location, after, now time.Time) time.Time {
	if sched == nil {
		return time.Time{}
	}
	var latest time.Time
	for t := sched.Next(after.In(loc)); !t.IsZero() && !t.After(now); t = sched.Next(t) {
		latest = t
	}
	return latest.UTC()
}

// Backoff is the wait after the given number of consecutive failures with no current version.
func Backoff(failed int32) time.Duration {
	switch {
	case failed <= 1:
		return time.Hour
	case failed == 2:
		return 4 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// ShouldNotifyFailure is true for the first failure, and afterwards at most once per 24 hours.
func ShouldNotifyFailure(failedAttempts int32, lastNotified *time.Time, now time.Time) bool {
	return failedAttempts == 1 || lastNotified == nil || now.Sub(*lastNotified) >= failureNotifyInterval
}
