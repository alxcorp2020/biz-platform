package notify

import "time"

// NextDailyRun returns the next occurrence of hour:min in loc at or after
// now — today if that time hasn't passed yet, otherwise tomorrow. Pulled out
// as a pure function so the "run once a day at a fixed time" scheduling logic
// (cmd/apiserver) is unit-testable without a real clock.
func NextDailyRun(now time.Time, loc *time.Location, hour, min int) time.Time {
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, min, 0, 0, loc)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
