package schedule_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/schedule"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return v
}

func TestEvaluateInitial(t *testing.T) {
	got, err := schedule.Evaluate(schedule.DueInput{}, at(t, "2026-11-24T10:00:00Z"))
	require.NoError(t, err)
	assert.True(t, got.Due)
	assert.Equal(t, "initial", got.Trigger)
}

func TestEvaluateBackoff(t *testing.T) {
	finished := at(t, "2026-11-24T10:00:00Z")
	base := schedule.DueInput{EverAttempted: true, LastFailedOrRejected: true, FailedAttempts: 1, LastFinished: finished}
	tests := []struct {
		name     string
		failed   int32
		after    time.Duration
		wantDue  bool
		wantNext time.Time
	}{
		{"first failure, 59m", 1, 59 * time.Minute, false, finished.Add(time.Hour)},
		{"first failure, exactly 1h", 1, time.Hour, true, time.Time{}},
		{"first failure, 61m", 1, 61 * time.Minute, true, time.Time{}},
		{"second failure, 3h", 2, 3 * time.Hour, false, finished.Add(4 * time.Hour)},
		{"third failure, 23h", 3, 23 * time.Hour, false, finished.Add(24 * time.Hour)},
		{"third failure, 25h", 3, 25 * time.Hour, true, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			in.FailedAttempts = tt.failed
			got, err := schedule.Evaluate(in, finished.Add(tt.after))
			require.NoError(t, err)
			assert.Equal(t, tt.wantDue, got.Due)
			assert.Equal(t, tt.wantNext, got.Next)
			if tt.wantDue {
				assert.Equal(t, "backoff", got.Trigger)
			}
		})
	}
}

func TestEvaluateRejectedCountsAsFailure(t *testing.T) {
	finished := at(t, "2026-11-24T10:00:00Z")
	in := schedule.DueInput{EverAttempted: true, LastFailedOrRejected: true, FailedAttempts: 1, LastFinished: finished}
	got, err := schedule.Evaluate(in, finished.Add(30*time.Minute))
	require.NoError(t, err)
	assert.False(t, got.Due)
	assert.Equal(t, finished.Add(time.Hour), got.Next)
}

func TestEvaluateAttemptedNoCurrentNoFailureWaitsForSchedule(t *testing.T) {
	in := schedule.DueInput{EverAttempted: true, Schedule: "0 3 * * *", CreatedAt: at(t, "2026-11-24T04:00:00Z")}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T05:00:00Z"))
	require.NoError(t, err)
	assert.False(t, got.Due)
	assert.Equal(t, at(t, "2026-11-25T03:00:00Z"), got.Next)
}

func TestEvaluateSchedule(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, Schedule: "0 3 * * *", LastScheduledAt: at(t, "2026-11-23T03:00:00Z")}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T03:00:01Z"))
	require.NoError(t, err)
	assert.True(t, got.Due)
	assert.Equal(t, "schedule", got.Trigger)
	assert.Equal(t, at(t, "2026-11-24T03:00:00Z"), got.ScheduleTick)
}

func TestEvaluateScheduleNotYet(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, Schedule: "0 3 * * *", LastScheduledAt: at(t, "2026-11-24T03:00:00Z")}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	require.NoError(t, err)
	assert.False(t, got.Due)
	assert.Equal(t, at(t, "2026-11-25T03:00:00Z"), got.Next)
}

func TestEvaluateNextIsEarliestOfBackoffAndTick(t *testing.T) {
	finished := at(t, "2026-11-24T10:00:00Z")
	in := schedule.DueInput{
		EverAttempted: true, LastFailedOrRejected: true, FailedAttempts: 2, LastFinished: finished,
		Schedule: "0 12 * * *", LastScheduledAt: at(t, "2026-11-24T00:00:00Z"),
	}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T10:30:00Z"))
	require.NoError(t, err)
	assert.False(t, got.Due)
	assert.Equal(t, at(t, "2026-11-24T12:00:00Z"), got.Next, "tick at 12:00 beats backoff at 14:00")
}

func TestEvaluateTimeZone(t *testing.T) {
	in := schedule.DueInput{
		HasCurrent: true, EverAttempted: true, Schedule: "0 3 * * *", TimeZone: "America/Los_Angeles",
		LastScheduledAt: at(t, "2026-11-23T00:00:00Z"),
	}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	require.NoError(t, err)
	assert.True(t, got.Due)
	assert.Equal(t, at(t, "2026-11-24T11:00:00Z"), got.ScheduleTick)
}

func TestEvaluateBadTimeZone(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, Schedule: "0 3 * * *", TimeZone: "Mars/Olympus"}
	_, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	assert.Error(t, err)
}

func TestEvaluateRequested(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, RequestedAt: "x"}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	require.NoError(t, err)
	assert.True(t, got.Due)
	assert.Equal(t, "requested", got.Trigger)

	in.LastHandledRequestedAt = "x"
	got, err = schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	require.NoError(t, err)
	assert.False(t, got.Due)
}

func TestEvaluateRunningSkipsTick(t *testing.T) {
	in := schedule.DueInput{
		HasCurrent: true, EverAttempted: true, Running: true, RequestedAt: "x",
		Schedule: "0 3 * * *", LastScheduledAt: at(t, "2026-11-23T03:00:00Z"),
	}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T03:00:01Z"))
	require.NoError(t, err)
	assert.False(t, got.Due, "running beats a pending request")
	assert.Equal(t, at(t, "2026-11-24T03:00:00Z"), got.SkippedTick)
}

func TestEvaluateRunningNoTickPassed(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, Running: true, Schedule: "0 3 * * *", LastScheduledAt: at(t, "2026-11-24T03:00:00Z")}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T04:00:00Z"))
	require.NoError(t, err)
	assert.False(t, got.Due)
	assert.True(t, got.SkippedTick.IsZero())
}

func TestEvaluateAwaitingAdoption(t *testing.T) {
	in := schedule.DueInput{AwaitingAdoption: true, RequestedAt: "x", Schedule: "* * * * *"}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	require.NoError(t, err)
	assert.Equal(t, schedule.Due{}, got)
}

func TestEvaluateBadCron(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, Schedule: "nope"}
	_, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	assert.Error(t, err)
}

func TestEvaluateZeroBaselineUsesCreation(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, Schedule: "0 3 * * *", CreatedAt: at(t, "2026-11-24T10:00:00Z")}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	require.NoError(t, err)
	assert.False(t, got.Due)
}

func TestEvaluateEmptySchedule(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, LastScheduledAt: at(t, "2020-01-01T00:00:00Z")}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T12:00:00Z"))
	require.NoError(t, err)
	assert.Equal(t, schedule.Due{}, got)
}

func TestEvaluateLatestTickOnly(t *testing.T) {
	in := schedule.DueInput{HasCurrent: true, EverAttempted: true, Schedule: "0 * * * *", LastScheduledAt: at(t, "2026-11-24T07:00:00Z")}
	got, err := schedule.Evaluate(in, at(t, "2026-11-24T12:30:00Z"))
	require.NoError(t, err)
	assert.True(t, got.Due)
	assert.Equal(t, at(t, "2026-11-24T12:00:00Z"), got.ScheduleTick)
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		failed int32
		want   time.Duration
	}{
		{0, time.Hour},
		{1, time.Hour},
		{2, 4 * time.Hour},
		{3, 24 * time.Hour},
		{10, 24 * time.Hour},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, schedule.Backoff(tt.failed), "failed=%d", tt.failed)
	}
}

func TestShouldNotifyFailure(t *testing.T) {
	now := at(t, "2026-11-24T12:00:00Z")
	ago := func(d time.Duration) *time.Time { return new(now.Add(-d)) }
	tests := []struct {
		name     string
		failed   int32
		notified *time.Time
		want     bool
	}{
		{"first failure", 1, ago(time.Minute), true},
		{"never notified", 2, nil, true},
		{"notified 23h ago", 2, ago(23 * time.Hour), false},
		{"notified 24h ago", 2, ago(24 * time.Hour), true},
		{"notified 25h ago", 2, ago(25 * time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, schedule.ShouldNotifyFailure(tt.failed, tt.notified, now))
		})
	}
}
