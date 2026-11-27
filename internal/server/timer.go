package server

import (
	"math"
	"time"

	"github.com/rouxlabs/headcoach/internal/store"
)

// maxTimer is the longest timer a task takes: a week is already longer than any run it is meant for.
const maxTimer = 7 * 24 * 3600

// timerField reads the timer of a request, in whole seconds. Zero means no timer.
func timerField(data map[string]any) (int, error) {
	return seconds(data, "timer", "The timer")
}

// seconds reads a delay of a request, in whole seconds, from zero for none up to maxTimer.
func seconds(data map[string]any, key, what string) (int, error) {
	value, ok := data[key].(float64)
	if !ok || value < 0 || value > maxTimer || value != math.Trunc(value) {
		return 0, fail(400, "%s is a whole number of seconds, from 0 for none to %d", what, maxTimer)
	}
	return int(value), nil
}

// endsAt is when a timer set now runs out. Zero for no timer.
func endsAt(timer int) float64 {
	if timer == 0 {
		return 0
	}
	return store.Now() + float64(timer)
}

// timerLeft is how many seconds a task's timer has left, at least 1 while it runs. Zero without a timer.
func timerLeft(task *store.Task, now float64) int {
	return left(task.EndsAt, now)
}

// left is how many seconds there are until at, at least 1 until it has passed. Zero when at is zero.
func left(at, now float64) int {
	if at == 0 {
		return 0
	}
	return max(1, int(math.Ceil(at-now)))
}

// Expire closes the tasks whose timer ran out by now: their session ends, as when parked, and they
// move to Done, whatever they were doing. The timer is a hard limit, so a run that hangs never lingers.
// Then it deletes the tasks whose delete is due, as the delete key does.
func (s *Server) Expire(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := float64(now.UnixNano()) / 1e9
	if err := s.closeExpired(at); err != nil {
		return err
	}
	doomed, err := s.st.Tasks("WHERE delete_at > 0 AND delete_at <= ?", at)
	if err != nil {
		return err
	}
	for _, task := range doomed {
		// A task under one deleted before it is gone already: deleting it again does nothing.
		if _, err := s.removeTask(task.ID); err != nil {
			return err
		}
	}
	return nil
}

// closeExpired closes the tasks whose timer ran out by now.
func (s *Server) closeExpired(now float64) error {
	due, err := s.st.Tasks("WHERE ends_at > 0 AND ends_at <= ?", now)
	if err != nil {
		return err
	}
	for i := range due {
		task := &due[i]
		// A task you moved to Done yourself is closed already: a session you kept open there stays.
		if task.State == "done" {
			if err := s.st.UpdateTask(task.ID, store.Fields{"ends_at": 0}); err != nil {
				return err
			}
			continue
		}
		live, err := s.liveSession(task.ID)
		if err != nil {
			return err
		}
		if live != nil {
			s.tmux.KillPane(live.Pane, live.Socket)
		}
		// Done first: a running task whose sessions close would otherwise go to Review on the way.
		_, err = s.st.SetState(task.ID, "done", "timer", store.Fields{"ends_at": 0, "last": "Closed when its timer ran out"})
		if err != nil {
			return err
		}
		if err := s.closeSessions(task.ID); err != nil {
			return err
		}
	}
	return nil
}
