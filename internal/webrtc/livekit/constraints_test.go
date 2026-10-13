package livekit

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
)

var constraintT0 = time.UnixMilli(1_791_300_000_000)

func metricValue(m prometheus.Metric) float64 {
	var pb dto.Metric
	_ = m.Write(&pb)

	if pb.Gauge != nil {
		return pb.GetGauge().GetValue()
	}

	return pb.GetCounter().GetValue()
}

func cameraCap(reason string, quality livekit.VideoQuality) types.CaptureConstraint {
	return types.CaptureConstraint{
		Reason:       reason,
		VideoQuality: quality,
		Sources:      []livekit.TrackSource{livekit.TrackSource_CAMERA},
	}
}

func TestConstraintBook_EffectiveQualityIsLowestMatchingCap(t *testing.T) {
	book := newConstraintBook(livekit.VideoQuality_HIGH)
	book.addTrack("cam", livekit.TrackSource_CAMERA, constraintT0)
	book.addTrack("screen", livekit.TrackSource_SCREEN_SHARE, constraintT0)

	_, noop := book.apply(cameraCap("a", livekit.VideoQuality_LOW), constraintT0)
	assert.Empty(t, noop)

	ev, noop := book.apply(types.CaptureConstraint{
		Reason:       "b",
		VideoQuality: livekit.VideoQuality_MEDIUM,
		Sources:      []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE},
	}, constraintT0)
	assert.Empty(t, noop)
	assert.Equal(t, []trackQuality{
		{"cam", livekit.TrackSource_CAMERA, livekit.VideoQuality_LOW, livekit.VideoQuality_LOW},
		{"screen", livekit.TrackSource_SCREEN_SHARE, livekit.VideoQuality_HIGH, livekit.VideoQuality_MEDIUM},
	}, ev.Tracks)
	assert.Equal(t, livekit.VideoQuality_LOW, book.quality["cam"])
	assert.Equal(t, livekit.VideoQuality_MEDIUM, book.quality["screen"])

	ev, noop = book.lift("a", constraintT0.Add(time.Second))
	assert.Empty(t, noop)
	assert.Equal(t, []trackQuality{
		{"cam", livekit.TrackSource_CAMERA, livekit.VideoQuality_LOW, livekit.VideoQuality_MEDIUM},
	}, ev.Tracks)

	ev, noop = book.lift("b", constraintT0.Add(2*time.Second))
	assert.Empty(t, noop)
	assert.Equal(t, livekit.VideoQuality_HIGH, ev.Tracks[0].To)
	assert.Equal(t, livekit.VideoQuality_HIGH, ev.Tracks[1].To)
}

func TestConstraintBook_IdempotentPerReason(t *testing.T) {
	book := newConstraintBook(livekit.VideoQuality_HIGH)
	book.addTrack("cam", livekit.TrackSource_CAMERA, constraintT0)

	_, noop := book.apply(cameraCap("disk", livekit.VideoQuality_LOW), constraintT0)
	assert.Empty(t, noop)

	ev, noop := book.apply(cameraCap("disk", livekit.VideoQuality_MEDIUM), constraintT0)
	assert.Equal(t, "reason already active", noop)
	assert.Empty(t, ev.Tracks)
	assert.Equal(t, livekit.VideoQuality_LOW, book.quality["cam"])

	_, noop = book.lift("disk", constraintT0)
	assert.Empty(t, noop)

	_, noop = book.lift("disk", constraintT0)
	assert.Equal(t, "reason not active", noop)
	assert.Len(t, book.records, 1)
}

func TestConstraintBook_SourceNotListedIsNoop(t *testing.T) {
	book := newConstraintBook(livekit.VideoQuality_HIGH)
	book.addTrack("screen", livekit.TrackSource_SCREEN_SHARE, constraintT0)

	ev, noop := book.apply(cameraCap("disk", livekit.VideoQuality_LOW), constraintT0)
	assert.NotEmpty(t, noop)
	assert.False(t, ev.Opened)
	assert.Equal(t, livekit.VideoQuality_HIGH, book.quality["screen"])

	constrained, records := book.snapshot(constraintT0)
	assert.False(t, constrained)
	assert.Empty(t, records)
	assert.NotNil(t, records)
}

func TestConstraintBook_PreferredAtOrBelowCapIsNoop(t *testing.T) {
	for _, tc := range []struct {
		preferred livekit.VideoQuality
		cap       livekit.VideoQuality
	}{
		{livekit.VideoQuality_LOW, livekit.VideoQuality_LOW},
		{livekit.VideoQuality_MEDIUM, livekit.VideoQuality_HIGH},
	} {
		book := newConstraintBook(tc.preferred)
		book.addTrack("cam", livekit.TrackSource_CAMERA, constraintT0)

		ev, noop := book.apply(cameraCap("disk", tc.cap), constraintT0)
		assert.NotEmpty(t, noop)
		assert.False(t, ev.Opened)
		assert.Equal(t, tc.preferred, book.quality["cam"])

		_, noop = book.lift("disk", constraintT0)
		assert.Equal(t, "reason was not constraining the capture", noop)
	}
}

func TestConstraintBook_ApplyBeforeTrackOpensAtSubscribe(t *testing.T) {
	book := newConstraintBook(livekit.VideoQuality_HIGH)

	_, noop := book.apply(cameraCap("disk", livekit.VideoQuality_LOW), constraintT0)
	assert.Equal(t, "no video track subscribed yet", noop)

	subscribeAt := constraintT0.Add(3 * time.Second)
	quality, events := book.addTrack("cam", livekit.TrackSource_CAMERA, subscribeAt)
	assert.Equal(t, livekit.VideoQuality_LOW, quality)
	assert.Len(t, events, 1)
	assert.True(t, events[0].Opened)
	assert.True(t, events[0].FirstForReason)
	assert.Equal(t, subscribeAt.UnixMilli(), events[0].Record.StartTimeMs)
	assert.Equal(t, []string{"cam"}, events[0].Record.TrackIDs)

	quality, events = book.addTrack("cam2", livekit.TrackSource_CAMERA, subscribeAt)
	assert.Equal(t, livekit.VideoQuality_LOW, quality)
	assert.False(t, events[0].Opened)
	assert.Equal(t, []string{"cam", "cam2"}, book.records[0].TrackIDs)
}

func TestConstraintBook_IntervalsCloseOnLiftAndAtSnapshot(t *testing.T) {
	book := newConstraintBook(livekit.VideoQuality_HIGH)
	book.addTrack("cam", livekit.TrackSource_CAMERA, constraintT0)

	ev, _ := book.apply(cameraCap("disk", livekit.VideoQuality_LOW), constraintT0)
	assert.True(t, ev.FirstForReason)

	ev, _ = book.lift("disk", constraintT0.Add(10*time.Second))
	assert.Equal(t, appstats.ConstraintRecord{
		Reason:       "disk",
		VideoQuality: "LOW",
		TrackIDs:     []string{"cam"},
		StartTimeMs:  constraintT0.UnixMilli(),
		EndTimeMs:    constraintT0.Add(10 * time.Second).UnixMilli(),
		EndReason:    appstats.ConstraintEndReleased,
	}, ev.Record)

	ev, _ = book.apply(cameraCap("disk", livekit.VideoQuality_LOW), constraintT0.Add(20*time.Second))
	assert.True(t, ev.Opened)
	assert.False(t, ev.FirstForReason)

	statsAt := constraintT0.Add(30 * time.Second)
	constrained, records := book.snapshot(statsAt)
	assert.True(t, constrained)
	assert.Len(t, records, 2)
	assert.Equal(t, statsAt.UnixMilli(), records[1].EndTimeMs)
	assert.Equal(t, appstats.ConstraintEndCaptureStopped, records[1].EndReason)

	// The snapshot leaves the interval open in the book.
	ev, noop := book.lift("disk", constraintT0.Add(40*time.Second))
	assert.Empty(t, noop)
	assert.Equal(t, appstats.ConstraintEndReleased, ev.Record.EndReason)
}

func TestConstraintBook_StopReportsOpenRecordsOnce(t *testing.T) {
	book := newConstraintBook(livekit.VideoQuality_HIGH)
	book.addTrack("cam", livekit.TrackSource_CAMERA, constraintT0)
	book.apply(cameraCap("disk", livekit.VideoQuality_LOW), constraintT0)

	assert.Equal(t, []appstats.ConstraintRecord{{
		Reason:       "disk",
		VideoQuality: "LOW",
		TrackIDs:     []string{"cam"},
		StartTimeMs:  constraintT0.UnixMilli(),
		EndTimeMs:    constraintT0.Add(time.Second).UnixMilli(),
		EndReason:    appstats.ConstraintEndCaptureStopped,
	}}, book.stop(constraintT0.Add(time.Second)))
	assert.Nil(t, book.stop(constraintT0.Add(2*time.Second)))
	assert.Equal(t, appstats.ConstraintEndCaptureStopped, book.records[0].EndReason)
	assert.Equal(t, constraintT0.Add(time.Second).UnixMilli(), book.records[0].EndTimeMs)

	_, noop := book.lift("disk", constraintT0.Add(3*time.Second))
	assert.Equal(t, "capture stopped", noop)
}

// qualitySend is one quality sent through LiveKitWebRTC.sendVideoQuality.
type qualitySend struct {
	pub     *lksdk.RemoteTrackPublication
	quality livekit.VideoQuality
}

type qualitySends struct {
	mu    sync.Mutex
	sends map[string][]qualitySend
}

func (q *qualitySends) record(trackID string, pub *lksdk.RemoteTrackPublication, quality livekit.VideoQuality) {
	// Widens the window a racing compute would need to send out of order.
	runtime.Gosched()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.sends[trackID] = append(q.sends[trackID], qualitySend{pub, quality})
}

func (q *qualitySends) qualities(trackID string) []livekit.VideoQuality {
	q.mu.Lock()
	defer q.mu.Unlock()

	qualities := make([]livekit.VideoQuality, 0, len(q.sends[trackID]))

	for _, send := range q.sends[trackID] {
		qualities = append(qualities, send.quality)
	}

	return qualities
}

func setupConstrainedLK() (*LiveKitWebRTC, *qualitySends) {
	lk, _ := setupMockLK()
	lk.constraints = newConstraintBook(livekit.VideoQuality_HIGH)
	sends := &qualitySends{sends: make(map[string][]qualitySend)}
	lk.sendVideoQuality = sends.record

	return lk, sends
}

// constraintMetrics holds the global constraint metrics of one reason.
// Tests compare two reads: other runs of a test (-count) share them.
type constraintMetrics struct {
	active, total, applies, lifts float64
}

func readConstraintMetrics(reason string) constraintMetrics {
	return constraintMetrics{
		active:  metricValue(appstats.ConstrainedCapturesActive.WithLabelValues(reason)),
		total:   metricValue(appstats.ConstrainedCapturesTotal.WithLabelValues(reason)),
		applies: metricValue(appstats.CaptureConstraintChanges.WithLabelValues(reason, "apply")),
		lifts:   metricValue(appstats.CaptureConstraintChanges.WithLabelValues(reason, "lift")),
	}
}

func (m constraintMetrics) since(before constraintMetrics) constraintMetrics {
	return constraintMetrics{
		active:  m.active - before.active,
		total:   m.total - before.total,
		applies: m.applies - before.applies,
		lifts:   m.lifts - before.lifts,
	}
}

func TestLiveKitWebRTC_ConstraintMetricsAndStats(t *testing.T) {
	const reason = "test_apply_lift"

	lk, _ := setupConstrainedLK()
	lk.constraints.addTrack("cam", livekit.TrackSource_CAMERA, time.Now())
	before := readConstraintMetrics(reason)

	assert.True(t, lk.ApplyConstraint(cameraCap(reason, livekit.VideoQuality_LOW)))
	assert.False(t, lk.ApplyConstraint(cameraCap(reason, livekit.VideoQuality_LOW)))
	assert.Equal(t, constraintMetrics{active: 1, total: 1, applies: 1}, readConstraintMetrics(reason).since(before))

	stats := lk.GetStats()
	assert.True(t, stats.Constrained)
	assert.Len(t, stats.Constraints, 1)
	assert.Equal(t, appstats.ConstraintEndCaptureStopped, stats.Constraints[0].EndReason)

	assert.True(t, lk.LiftConstraint(reason))
	assert.True(t, lk.ApplyConstraint(cameraCap(reason, livekit.VideoQuality_LOW)))
	assert.True(t, lk.LiftConstraint(reason))
	assert.Equal(t, constraintMetrics{total: 1, applies: 2, lifts: 2}, readConstraintMetrics(reason).since(before))

	stats = lk.GetStats()
	assert.Len(t, stats.Constraints, 2)
	assert.Equal(t, appstats.ConstraintEndReleased, stats.Constraints[1].EndReason)

	lk.Close()
}

func TestLiveKitWebRTC_ConstraintChangesReportAndSendQualityChanges(t *testing.T) {
	lk, sends := setupConstrainedLK()
	defer lk.Close()

	assert.False(t, lk.ApplyConstraint(cameraCap("early", livekit.VideoQuality_MEDIUM)), "no track yet")
	lk.setSubscribeVideoQuality("cam", livekit.TrackSource_CAMERA, &lksdk.RemoteTrackPublication{})
	lk.setSubscribeVideoQuality("screen", livekit.TrackSource_SCREEN_SHARE, &lksdk.RemoteTrackPublication{})
	assert.Equal(t, []livekit.VideoQuality{livekit.VideoQuality_MEDIUM}, sends.qualities("cam"))
	assert.Equal(t, []livekit.VideoQuality{livekit.VideoQuality_HIGH}, sends.qualities("screen"))

	assert.False(t, lk.ApplyConstraint(types.CaptureConstraint{
		Reason:       "screen_only",
		VideoQuality: livekit.VideoQuality_HIGH,
		Sources:      []livekit.TrackSource{livekit.TrackSource_SCREEN_SHARE},
	}), "cap at preferred quality")
	assert.True(t, lk.ApplyConstraint(cameraCap("low", livekit.VideoQuality_LOW)))
	assert.False(t, lk.ApplyConstraint(cameraCap("medium", livekit.VideoQuality_MEDIUM)), "a lower cap is active")
	assert.False(t, lk.LiftConstraint("medium"), "a lower cap is still active")
	assert.True(t, lk.LiftConstraint("low"))
	assert.True(t, lk.LiftConstraint("early"))
	assert.False(t, lk.LiftConstraint("early"), "reason not active")
	assert.False(t, lk.LiftConstraint("screen_only"), "reason was not constraining")

	assert.Equal(t, []livekit.VideoQuality{
		livekit.VideoQuality_MEDIUM,
		livekit.VideoQuality_LOW,
		livekit.VideoQuality_MEDIUM,
		livekit.VideoQuality_HIGH,
	}, sends.qualities("cam"))
	assert.Equal(t, []livekit.VideoQuality{livekit.VideoQuality_HIGH}, sends.qualities("screen"))
}

func TestLiveKitWebRTC_StopConstraintsFreezesStats(t *testing.T) {
	const reason = "test_stop"

	lk, _ := setupConstrainedLK()
	defer lk.Close()

	lk.constraints.addTrack("cam", livekit.TrackSource_CAMERA, time.Now())
	before := readConstraintMetrics(reason)

	assert.True(t, lk.ApplyConstraint(cameraCap(reason, livekit.VideoQuality_LOW)))
	lk.StopConstraints()
	stats := lk.GetStats()

	assert.False(t, lk.LiftConstraint(reason))
	assert.False(t, lk.ApplyConstraint(cameraCap("test_stop_other", livekit.VideoQuality_LOW)))
	lk.StopConstraints()

	assert.Equal(t, stats.Constraints, lk.GetStats().Constraints)
	assert.Len(t, stats.Constraints, 1)
	assert.Equal(t, appstats.ConstraintEndCaptureStopped, stats.Constraints[0].EndReason)
	assert.Equal(t, constraintMetrics{total: 1, applies: 1}, readConstraintMetrics(reason).since(before))
}

func TestLiveKitWebRTC_CloseDecrementsGaugeOnce(t *testing.T) {
	const reason = "test_close"

	lk, sends := setupConstrainedLK()
	before := readConstraintMetrics(reason)

	assert.False(t, lk.ApplyConstraint(cameraCap(reason, livekit.VideoQuality_LOW)))
	lk.setSubscribeVideoQuality("cam", livekit.TrackSource_CAMERA, &lksdk.RemoteTrackPublication{})
	assert.Equal(t, []livekit.VideoQuality{livekit.VideoQuality_LOW}, sends.qualities("cam"))
	assert.Equal(t, constraintMetrics{active: 1, total: 1, applies: 1}, readConstraintMetrics(reason).since(before))

	lk.Close()
	lk.Close()
	assert.False(t, lk.LiftConstraint(reason))
	assert.Equal(t, constraintMetrics{total: 1, applies: 1}, readConstraintMetrics(reason).since(before))
}

func TestLiveKitWebRTC_ApplyConstraintRejectsInvalid(t *testing.T) {
	lk, _ := setupConstrainedLK()
	defer lk.Close()

	assert.False(t, lk.ApplyConstraint(cameraCap("", livekit.VideoQuality_LOW)))
	assert.False(t, lk.ApplyConstraint(cameraCap("test_invalid", livekit.VideoQuality_OFF)))

	lk.m.Lock()
	defer lk.m.Unlock()
	assert.Empty(t, lk.constraints.active)
}

func TestLiveKitWebRTC_ConcurrentConstraintChanges(t *testing.T) {
	const reason = "test_concurrent"

	lk, _ := setupConstrainedLK()
	lk.constraints.addTrack("cam", livekit.TrackSource_CAMERA, time.Now())
	before := readConstraintMetrics(reason)

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := 0; j < 50; j++ {
				lk.ApplyConstraint(cameraCap(reason, livekit.VideoQuality_LOW))
				lk.GetStats()
				lk.LiftConstraint(reason)
			}
		}()
	}

	wg.Wait()
	lk.Close()

	delta := readConstraintMetrics(reason).since(before)
	assert.InDelta(t, 0, delta.active, 0)
	assert.InDelta(t, 1, delta.total, 0)
	assert.InDelta(t, delta.applies, delta.lifts, 0)
}

// The last quality sent for each track must be the one the book computed
// last, whatever the interleaving of subscribe, apply and lift. Only the
// final operations can leave a track out of order, so the test runs many
// short rounds.
func TestLiveKitWebRTC_LastSendMatchesComputedQuality(t *testing.T) {
	for round := 0; round < 20; round++ {
		lastSendRound(t)
	}
}

func lastSendRound(t *testing.T) {
	t.Helper()

	const tracksPerSource = 10

	lk, sends := setupConstrainedLK()
	defer lk.Close()

	camLow := cameraCap("test_last_send_camera", livekit.VideoQuality_LOW)
	allMedium := types.CaptureConstraint{
		Reason:       "test_last_send_all",
		VideoQuality: livekit.VideoQuality_MEDIUM,
		Sources:      []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE},
	}

	pubs := make(map[string]*lksdk.RemoteTrackPublication)
	sources := make(map[string]livekit.TrackSource)

	for i := 0; i < tracksPerSource; i++ {
		pubs[fmt.Sprintf("cam-%d", i)] = &lksdk.RemoteTrackPublication{}
		sources[fmt.Sprintf("cam-%d", i)] = livekit.TrackSource_CAMERA
		pubs[fmt.Sprintf("screen-%d", i)] = &lksdk.RemoteTrackPublication{}
		sources[fmt.Sprintf("screen-%d", i)] = livekit.TrackSource_SCREEN_SHARE
	}

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		for trackID, pub := range pubs {
			lk.setSubscribeVideoQuality(trackID, sources[trackID], pub)
		}
	}()

	for _, c := range []types.CaptureConstraint{camLow, allMedium, camLow, allMedium, camLow, allMedium} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := 0; j < 10; j++ {
				lk.ApplyConstraint(c)
				lk.LiftConstraint(c.Reason)
			}

			lk.ApplyConstraint(c)
		}()
	}

	wg.Wait()

	want := map[livekit.TrackSource]livekit.VideoQuality{
		livekit.TrackSource_CAMERA:       livekit.VideoQuality_LOW,
		livekit.TrackSource_SCREEN_SHARE: livekit.VideoQuality_MEDIUM,
	}

	lk.m.Lock()
	defer lk.m.Unlock()

	for trackID, pub := range pubs {
		assert.Equal(t, want[sources[trackID]], lk.constraints.quality[trackID], trackID)

		sends.mu.Lock()
		trackSends := sends.sends[trackID]
		sends.mu.Unlock()

		if assert.NotEmpty(t, trackSends, trackID) {
			last := trackSends[len(trackSends)-1]
			assert.Same(t, pub, last.pub, trackID)
			assert.Equal(t, lk.constraints.quality[trackID], last.quality, trackID)
		}
	}
}

func TestSourceNames(t *testing.T) {
	sources := []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE}

	assert.Equal(t, []string{"CAMERA", "SCREEN_SHARE"}, sourceNames(sources))
}
