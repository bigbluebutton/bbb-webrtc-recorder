package server

import (
	"maps"
	"slices"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	log "github.com/sirupsen/logrus"
)

var reconcilerStopTimeout = 5 * time.Second

// constraintReconciler propagates active constraint changes to live sessions.
// Runs on its own goroutine so we do not block the recorder's main loop as
// we've done in the past with other server-level mechanisms.
type constraintReconciler struct {
	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// SetConstraint activates c for live and future sessions.
func (s *Server) SetConstraint(c types.CaptureConstraint) {
	s.constraintsMu.Lock()
	defer s.constraintsMu.Unlock()

	s.constraints[c.Reason] = c
	s.reasons[c.Reason] = struct{}{}
	s.wakeReconciler()
}

// ClearConstraint deactivates the constraint of the given reason.
func (s *Server) ClearConstraint(reason string) {
	s.constraintsMu.Lock()
	defer s.constraintsMu.Unlock()

	if _, ok := s.constraints[reason]; !ok {
		return
	}

	delete(s.constraints, reason)
	s.wakeReconciler()
}

// wakeReconciler must be called with constraintsMu held.
func (s *Server) wakeReconciler() {
	if s.closed {
		return
	}

	if s.reconciler == nil {
		s.reconciler = &constraintReconciler{
			wake: make(chan struct{}, 1),
			stop: make(chan struct{}),
			done: make(chan struct{}),
		}

		go s.runReconciler(s.reconciler)
	}

	select {
	case s.reconciler.wake <- struct{}{}:
	default:
		// A pass is already pending, and it reads the set when it starts.
	}
}

func (s *Server) stopReconciler() {
	s.constraintsMu.Lock()
	s.closed = true
	rec := s.reconciler
	s.constraintsMu.Unlock()

	if rec == nil {
		return
	}

	close(rec.stop)

	select {
	case <-rec.done:
	case <-time.After(reconcilerStopTimeout):
		log.Warn("Capture constraint reconciler did not stop in time, a constraint send is stalled")
	}
}

func (s *Server) runReconciler(rec *constraintReconciler) {
	defer close(rec.done)

	for {
		select {
		case <-rec.stop:
			return
		case <-rec.wake:
			select {
			case <-rec.stop:
				return
			default:
				s.reconcileConstraints()
			}
		}
	}
}

// reconcileConstraints applies every active constraint to every live
// session and lifts every other reason ever set. Both are no-ops on a
// capture already in that state, so the counts are the captures whose
// quality this pass changed.
func (s *Server) reconcileConstraints() {
	start := time.Now()

	s.constraintsMu.Lock()
	active := maps.Clone(s.constraints)
	reasons := slices.Sorted(maps.Keys(s.reasons))
	s.constraintsMu.Unlock()

	changed := make(map[string]int, len(reasons))

	s.sessions.Range(func(_, value any) bool {
		sess := value.(*Session)

		for _, reason := range reasons {
			var didChange bool

			if c, ok := active[reason]; ok {
				didChange = sess.applyConstraint(c)
			} else {
				didChange = sess.liftConstraint(reason)
			}

			if didChange {
				changed[reason]++
			}
		}

		return true
	})

	duration := time.Since(start).Round(time.Microsecond)

	for _, reason := range reasons {
		action, summary := "lift", "lifted, restored"
		if _, ok := active[reason]; ok {
			action, summary = "apply", "applied, capped"
		}

		log.WithFields(log.Fields{
			"reason":   reason,
			"action":   action,
			"captures": changed[reason],
			"duration": duration.String(),
		}).Infof("Capture constraint %s %s %d live LiveKit captures in %s",
			reason, summary, changed[reason], duration)
	}
}
