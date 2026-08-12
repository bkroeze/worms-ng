package ui

import "time"

// TickScheduler issues at most one authoritative autonomous command per due
// deadline. It never accumulates debt, so a throttled/background tab cannot
// replay a burst of stale decisions when it becomes visible again.
type TickScheduler struct {
	due     time.Time
	armed   bool
	lastKey string
}

func presentationDelay(speed int) time.Duration {
	if speed < 1 {
		speed = 1
	}
	if speed > 9 {
		speed = 9
	}
	return time.Duration(1000-(speed-1)*112) * time.Millisecond
}

func (s *TickScheduler) Reset() {
	s.due = time.Time{}
	s.armed = false
	s.lastKey = ""
}

func (s *TickScheduler) Due(now time.Time, speed int, paused, autonomous bool, identity string) (bool, time.Time) {
	if paused || !autonomous || identity == "" {
		s.Reset()
		return false, time.Time{}
	}
	if !s.armed || s.lastKey != identity {
		s.armed = true
		s.lastKey = identity
		s.due = now.Add(presentationDelay(speed))
		return false, s.due
	}
	if now.Before(s.due) {
		return false, s.due
	}
	// Re-arm from the observation time, never from an overdue deadline. This is
	// the background-gap safety invariant: one frame can dispatch one command.
	s.due = now.Add(presentationDelay(speed))
	return true, s.due
}
