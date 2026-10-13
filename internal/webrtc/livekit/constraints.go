package livekit

import (
	"maps"
	"slices"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/livekit/protocol/livekit"
)

// trackQuality is the effective quality of a video track before and
// after a constraint event happens.
type trackQuality struct {
	TrackID string
	Source  livekit.TrackSource
	From    livekit.VideoQuality
	To      livekit.VideoQuality
}

// constraintEvent describes why (reason) and over what (tracks) a constraint
// was exercised.
type constraintEvent struct {
	Reason         string
	Tracks         []trackQuality
	Opened         bool // true if this event opened the reason's interval
	FirstForReason bool // true if this is the first time the reason has constrained the capture
	Record         appstats.ConstraintRecord
}

// constraintBook is the constraint state of ONE capture.
// Unsafe for concurrent use.
type constraintBook struct {
	preferred livekit.VideoQuality // Generally: cfg.PreferredVideoQuality
	active    map[string]types.CaptureConstraint
	tracks    map[string]livekit.TrackSource
	quality   map[string]livekit.VideoQuality
	openIdx   map[string]int
	counted   map[string]bool
	records   []appstats.ConstraintRecord
	stopped   bool
}

func newConstraintBook(preferred livekit.VideoQuality) *constraintBook {
	return &constraintBook{
		preferred: preferred,
		active:    make(map[string]types.CaptureConstraint),
		tracks:    make(map[string]livekit.TrackSource),
		quality:   make(map[string]livekit.VideoQuality),
		openIdx:   make(map[string]int),
		counted:   make(map[string]bool),
	}
}

// binds reports whether constraint c lowers a track matching source below the
// preferred quality, regardless of other active constraints.
func (b *constraintBook) binds(c types.CaptureConstraint, source livekit.TrackSource) bool {
	return c.VideoQuality < b.preferred && slices.Contains(c.Sources, source)
}

// effective returns the effective lowest quality for a track matching source,
// considering all active constraints.
func (b *constraintBook) effective(source livekit.TrackSource) livekit.VideoQuality {
	quality := b.preferred

	for _, c := range b.active {
		if slices.Contains(c.Sources, source) && c.VideoQuality < quality {
			quality = c.VideoQuality
		}
	}

	return quality
}

// apply activates constraint c. It returns a non-empty noop reason when c changes
// nothing; c stays active unless the reason is already active or the capture stopped.
func (b *constraintBook) apply(c types.CaptureConstraint, now time.Time) (constraintEvent, string) {
	ev := constraintEvent{Reason: c.Reason}

	if b.stopped {
		return ev, "capture stopped"
	}

	if _, ok := b.active[c.Reason]; ok {
		return ev, "reason already active"
	}

	c.Sources = slices.Clone(c.Sources)
	b.active[c.Reason] = c

	for _, trackID := range slices.Sorted(maps.Keys(b.tracks)) {
		source := b.tracks[trackID]

		if !b.binds(c, source) {
			continue
		}

		from := b.quality[trackID]
		b.quality[trackID] = b.effective(source)
		ev.Tracks = append(ev.Tracks, trackQuality{trackID, source, from, b.quality[trackID]})
	}

	if len(ev.Tracks) == 0 {
		if len(b.tracks) == 0 {
			return ev, "no video track subscribed yet"
		}

		return ev, "no track source matches or preferred quality at or below cap"
	}

	b.openRecord(&ev, c, now)

	return ev, ""
}

// lift deactivates reason and closes its interval, if one is open.
func (b *constraintBook) lift(reason string, now time.Time) (constraintEvent, string) {
	ev := constraintEvent{Reason: reason}

	if b.stopped {
		return ev, "capture stopped"
	}

	c, ok := b.active[reason]

	if !ok {
		return ev, "reason not active"
	}

	delete(b.active, reason)

	for _, trackID := range slices.Sorted(maps.Keys(b.tracks)) {
		source := b.tracks[trackID]

		if !b.binds(c, source) {
			continue
		}

		from := b.quality[trackID]
		b.quality[trackID] = b.effective(source)
		ev.Tracks = append(ev.Tracks, trackQuality{trackID, source, from, b.quality[trackID]})
	}

	idx, open := b.openIdx[reason]

	if !open {
		return ev, "reason was not constraining the capture"
	}

	b.closeRecord(idx, now, appstats.ConstraintEndReleased)
	ev.Record = cloneRecord(b.records[idx])

	return ev, ""
}

// addTrack registers a video track and returns the quality to subscribe
// it with, plus one event per active constraint that binds it.
func (b *constraintBook) addTrack(trackID string, source livekit.TrackSource, now time.Time) (livekit.VideoQuality, []constraintEvent) {
	quality := b.effective(source)
	b.tracks[trackID] = source
	b.quality[trackID] = quality

	if b.stopped {
		return quality, nil
	}

	var events []constraintEvent

	for _, reason := range slices.Sorted(maps.Keys(b.active)) {
		c := b.active[reason]

		if !b.binds(c, source) {
			continue
		}

		ev := constraintEvent{
			Reason: reason,
			Tracks: []trackQuality{{trackID, source, b.preferred, quality}},
		}

		if idx, open := b.openIdx[reason]; open {
			if !slices.Contains(b.records[idx].TrackIDs, trackID) {
				b.records[idx].TrackIDs = append(b.records[idx].TrackIDs, trackID)
			}

			ev.Record = cloneRecord(b.records[idx])
		} else {
			b.openRecord(&ev, c, now)
		}

		events = append(events, ev)
	}

	return quality, events
}

func (b *constraintBook) stop(now time.Time) []appstats.ConstraintRecord {
	if b.stopped {
		return nil
	}

	b.stopped = true
	reasons := slices.Sorted(maps.Keys(b.openIdx))
	closed := make([]appstats.ConstraintRecord, 0, len(reasons))

	for _, reason := range reasons {
		idx := b.openIdx[reason]
		b.closeRecord(idx, now, appstats.ConstraintEndCaptureStopped)
		closed = append(closed, cloneRecord(b.records[idx]))
	}

	return closed
}

func (b *constraintBook) snapshot(now time.Time) (bool, []appstats.ConstraintRecord) {
	records := make([]appstats.ConstraintRecord, 0, len(b.records))

	for _, record := range b.records {
		record = cloneRecord(record)

		if record.EndReason == "" {
			record.EndTimeMs = now.UnixMilli()
			record.EndReason = appstats.ConstraintEndCaptureStopped
		}

		records = append(records, record)
	}

	return len(records) > 0, records
}

// openRecord appends a new record to the book and marks it open. It also sets
// the FirstForReason flag on the event if this is the first time the reason has
// constrained the capture.
func (b *constraintBook) openRecord(ev *constraintEvent, c types.CaptureConstraint, now time.Time) {
	trackIDs := make([]string, 0, len(ev.Tracks))

	for _, tq := range ev.Tracks {
		trackIDs = append(trackIDs, tq.TrackID)
	}

	b.records = append(b.records, appstats.ConstraintRecord{
		Reason:       c.Reason,
		VideoQuality: c.VideoQuality.String(),
		TrackIDs:     trackIDs,
		StartTimeMs:  now.UnixMilli(),
	})
	b.openIdx[c.Reason] = len(b.records) - 1

	ev.Opened = true
	ev.FirstForReason = !b.counted[c.Reason]
	ev.Record = cloneRecord(b.records[len(b.records)-1])
	b.counted[c.Reason] = true
}

func (b *constraintBook) closeRecord(idx int, now time.Time, endReason string) {
	b.records[idx].EndTimeMs = now.UnixMilli()
	b.records[idx].EndReason = endReason
	delete(b.openIdx, b.records[idx].Reason)
}

func qualityChanged(tracks []trackQuality) bool {
	return slices.ContainsFunc(tracks, func(tq trackQuality) bool {
		return tq.From != tq.To
	})
}

func cloneRecord(record appstats.ConstraintRecord) appstats.ConstraintRecord {
	record.TrackIDs = slices.Clone(record.TrackIDs)

	return record
}

// Map livekit.TrackSource values to their string names (for use in logging and
// metrics).
func sourceNames(sources []livekit.TrackSource) []string {
	names := make([]string, 0, len(sources))

	for _, source := range sources {
		names = append(names, source.String())
	}

	return names
}
