package utils

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const simRate = 48000

// simSender models a sender of 20 ms Opus frames and the network to the
// clock. Its media clock runs at rate(t) times its wall clock t, as when it
// loses captured audio.
type simSender struct {
	duration time.Duration
	rate     func(t time.Duration) float64 // nil: 1
	delay    func(t time.Duration) time.Duration
	// srEvery is the time between sender reports; 0 sends none. With
	// forwardEvery, the clock gets a report that often instead, as LiveKit
	// forwards them: the sender's latest, extended at the nominal rate for the
	// time since it reached the SFU.
	srEvery, forwardEvery time.Duration
	// srSpread, if set, draws each report interval from srEvery x [0.5, 1.5],
	// as libwebrtc's RTCP sender does.
	srSpread *rand.Rand
	// ntpStep moves the sender's NTP clock by ntpStep.by from ntpStep.at on.
	ntpStep struct{ at, by time.Duration }
	// stall pauses the capture for stall.dur at stall.at; the timestamps go
	// on from where they were.
	stall struct{ at, dur time.Duration }
	// realignAt moves the timestamps to where the wall clock puts them, as
	// LiveKit does when the publisher mutes and unmutes.
	realignAt time.Duration
	// tsJump moves the timestamps by tsJump.by ticks at tsJump.at.
	tsJump struct {
		at time.Duration
		by int32
	}
	// swapAt swaps the arrivals of the first two packets sent from then on.
	swapAt time.Duration
	// dtx sends one packet every 400 ms over [dtx.from, dtx.to), as Opus DTX
	// does in silence.
	dtx     struct{ from, to time.Duration }
	startTS uint32
}

type simPacket struct {
	sent, at time.Duration
	seq      uint16
	ts, out  uint32
}

// run feeds c the sender's packets and reports in arrival order, and
// returns the packets with the timestamps c gave them.
func (s simSender) run(c *MediaClock) []*simPacket {
	type event struct {
		at  time.Duration
		pkt *simPacket
		ntp uint64
		rtp uint32
	}

	type pubReport struct {
		sent, atSFU time.Duration
		ntp         time.Duration
		rtp         uint32
	}

	var (
		events  []event
		pkts    []*simPacket
		pubSRs  []pubReport
		t       time.Duration
		ts      = s.startTS
		nextSR  = s.srEvery
		nextFwd = s.forwardEvery
		stall   = s.stall.dur > 0
		realign = s.realignAt > 0
		jump    = s.tsJump.by != 0
		swapped = s.swapAt == 0
	)

	delay := func(t time.Duration) time.Duration {
		if s.delay == nil {
			return 50 * time.Millisecond
		}

		return s.delay(t)
	}

	for t < s.duration {
		for s.srEvery > 0 && nextSR <= t && len(pkts) > 0 {
			last := pkts[len(pkts)-1]
			ntp := nextSR
			if s.ntpStep.by != 0 && nextSR >= s.ntpStep.at {
				ntp += s.ntpStep.by
			}
			rtp := last.ts + uint32(math.Round((nextSR-last.sent).Seconds()*simRate))
			if s.forwardEvery > 0 {
				pubSRs = append(pubSRs, pubReport{sent: nextSR, atSFU: nextSR + delay(nextSR), ntp: ntp, rtp: rtp})
			} else {
				events = append(events, event{at: nextSR + delay(nextSR), ntp: toNTP(ntp), rtp: rtp})
			}
			if s.srSpread != nil {
				nextSR += time.Duration((0.5 + s.srSpread.Float64()) * float64(s.srEvery))
			} else {
				nextSR += s.srEvery
			}
		}

		for s.forwardEvery > 0 && nextFwd <= t {
			for i := len(pubSRs) - 1; i >= 0; i-- {
				if p := pubSRs[i]; p.atSFU <= nextFwd {
					since := nextFwd - p.atSFU
					events = append(events, event{at: nextFwd, ntp: toNTP(p.ntp + since),
						rtp: p.rtp + uint32(math.Round(since.Seconds()*simRate))})

					break
				}
			}
			nextFwd += s.forwardEvery
		}

		if realign && t >= s.realignAt {
			ts = s.startTS + uint32(math.Round(t.Seconds()*simRate))
			realign = false
		}
		if jump && t >= s.tsJump.at {
			ts += uint32(s.tsJump.by)
			jump = false
		}

		p := &simPacket{sent: t, at: t + delay(t), seq: uint16(len(pkts)), ts: ts}
		pkts = append(pkts, p)
		events = append(events, event{at: p.at, pkt: p})
		if n := len(events); !swapped && t >= s.swapAt && n >= 2 && events[n-2].pkt != nil && events[n-2].pkt.sent >= s.swapAt {
			events[n-1].at, events[n-1].pkt.at = events[n-2].at-time.Microsecond, events[n-2].at-time.Microsecond
			swapped = true
		}

		rate := 1.0
		if s.rate != nil {
			rate = s.rate(t)
		}
		frames := 1
		if t >= s.dtx.from && t < s.dtx.to {
			frames = 20
		}
		t += time.Duration(float64(frames) * float64(20*time.Millisecond) / rate)
		ts += uint32(frames) * 960

		if stall && t >= s.stall.at {
			t += s.stall.dur
			stall = false
		}
	}

	slices.SortStableFunc(events, func(a, b event) int { return int(a.at - b.at) })

	base := time.Unix(1_800_000_000, 0)
	for _, e := range events {
		if e.pkt != nil {
			e.pkt.out = c.Timestamp(e.pkt.ts, e.pkt.seq, base.Add(e.at))
		} else {
			c.SenderReport(e.ntp, e.rtp, base.Add(e.at))
		}
	}

	return pkts
}

// placement is how far each packet's output timestamp puts it from where
// its sender sent it, relative to the first: negative when it plays early.
func placement(pkts []*simPacket) []time.Duration {
	out := make([]time.Duration, len(pkts))
	for i, p := range pkts {
		media := time.Duration(float64(int32(p.out-pkts[0].out)) / simRate * float64(time.Second))
		out[i] = media - (p.sent - pkts[0].sent)
	}

	return out
}

func maxAbs(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		m = max(m, d.Abs())
	}

	return m
}

func toNTP(d time.Duration) uint64 {
	d += 3_990_000_000 * time.Second // well into the NTP era
	secs := uint64(d / time.Second)
	frac := uint64(d%time.Second) << 32 / uint64(time.Second)

	return secs<<32 | frac
}

// jitter returns a network delay of base plus up to maxJitter.
func jitter(seed uint64, base, maxJitter time.Duration) func(time.Duration) time.Duration {
	rng := rand.New(rand.NewPCG(seed, seed))

	return func(time.Duration) time.Duration {
		return base + time.Duration(rng.Float64()*float64(maxJitter))
	}
}

func between(from, to time.Duration, rate float64) func(time.Duration) float64 {
	return func(t time.Duration) float64 {
		if t >= from && t < to {
			return rate
		}

		return 1
	}
}

func TestMediaClockFollowsASlowSender(t *testing.T) {
	// 0.955 from 10 to 40 s of the sender's time: 30 s x 4.5 % = 1.35 s lost.
	s := simSender{
		duration: 60 * time.Second,
		rate:     between(10*time.Second, 40*time.Second, 0.955),
		delay:    jitter(1, 40*time.Millisecond, 60*time.Millisecond),
		srEvery:  5 * time.Second,
		startTS:  0xFFFFFFFF - 5*simRate, // wraps 5 s in
	}
	c := NewMediaClock(simRate, true, nil)
	pkts := s.run(c)
	p := placement(pkts)
	st := c.Stats()
	t.Logf("stats %+v; placement max %v, final %v", st, maxAbs(p), p[len(p)-1])

	// A burst's onset is corrected once a report shows it: one report
	// interval of 4.5 %, plus a frame.
	assert.LessOrEqual(t, maxAbs(p), 245*time.Millisecond, "largest placement error")
	// From then on the error is at most 105 ms early: the 45 ms the arrival
	// window trails by, the trigger and a frame. Late, it is at most a gap
	// less the trigger.
	var steady []time.Duration
	for i, pkt := range pkts {
		if pkt.sent >= 16*time.Second && pkt.sent < 40*time.Second {
			steady = append(steady, p[i])
		}
	}
	assert.LessOrEqual(t, maxAbs(steady), 105*time.Millisecond, "placement while the deficit grows")
	// Once the deficit stops, the corrections catch up.
	settled := mediaClockGap - mediaClockTrigger
	assert.LessOrEqual(t, p[len(p)-1].Abs(), settled, "placement at the end")
	assert.Positive(t, st.InsertedGaps)
	assert.InDelta(t, 1350, st.InsertedGapMs, float64(settled.Milliseconds()), "gaps inserted, against the 1.35 s lost")
	assert.InDelta(t, 0.977, st.RateRatio, 0.002, "RTP over arrival duration")
	assert.Equal(t, 11, st.SenderReports)
	assert.Zero(t, st.SenderClockSteps)

	gap := int32(mediaClockGap.Seconds() * simRate)
	for i := 1; i < len(pkts); i++ {
		step := int32(pkts[i].out - pkts[i-1].out)
		require.True(t, step >= 960 && (step-960)%gap == 0, "packet %d moved by %d ticks, not whole gaps forward", i, step)
	}
}

func TestMediaClockLeavesAHealthySenderAlone(t *testing.T) {
	steps := []struct {
		name  string
		delay func(time.Duration) time.Duration
	}{
		{name: "jitter", delay: jitter(2, 30*time.Millisecond, 150*time.Millisecond)},
		{name: "delay step", delay: func(t time.Duration) time.Duration {
			// The arrivals alone would add a gap here.
			if t >= 20*time.Second && t < 35*time.Second {
				return 440 * time.Millisecond
			}

			return 40 * time.Millisecond
		}},
		{name: "sender clock step", delay: nil},
	}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			s := simSender{duration: 60 * time.Second, delay: tc.delay, srEvery: 3 * time.Second}
			if tc.delay == nil {
				s.ntpStep.at, s.ntpStep.by = 30*time.Second, 2*time.Second
			}

			c := NewMediaClock(simRate, true, nil)
			pkts := s.run(c)
			st := c.Stats()
			t.Logf("stats %+v", st)

			assert.Zero(t, st.InsertedGaps)
			assert.Less(t, maxAbs(placement(pkts)), time.Microsecond)
			if tc.delay == nil {
				assert.Equal(t, 1, st.SenderClockSteps)
			}
		})
	}
}

func TestMediaClockWithoutReportsOnlyMeasures(t *testing.T) {
	s := simSender{duration: 30 * time.Second, rate: between(0, time.Hour, 0.96)}
	c := NewMediaClock(simRate, true, nil)
	pkts := s.run(c)
	st := c.Stats()

	assert.Zero(t, st.InsertedGaps)
	assert.Zero(t, st.SenderReports)
	assert.Zero(t, pkts[len(pkts)-1].out-pkts[len(pkts)-1].ts, "timestamps unchanged")
	assert.InDelta(t, 0.96, st.RateRatio, 0.001)
	// 30 s x 4 % behind, less the 40 ms that a 1 s window trails by.
	assert.InDelta(t, 1160, st.LagMs, 10, "behind by the arrivals")
}

func TestMediaClockNotCorrectingOnlyMeasures(t *testing.T) {
	s := simSender{duration: 30 * time.Second, rate: between(0, time.Hour, 0.96), srEvery: 3 * time.Second}
	c := NewMediaClock(simRate, false, nil)
	pkts := s.run(c)
	st := c.Stats()

	assert.Zero(t, st.InsertedGaps)
	assert.Zero(t, pkts[len(pkts)-1].out-pkts[len(pkts)-1].ts, "timestamps unchanged")
	// 4 % of the 27 s up to the last report, less the 40 ms the arrival
	// window trails by at the first one.
	assert.InDelta(t, -1040, st.ReportedDriftMs, 10, "drift by the reports")
}

func TestMediaClockFastSenderIsOnlyMeasured(t *testing.T) {
	s := simSender{duration: 30 * time.Second, rate: between(0, time.Hour, 1.02), srEvery: 5 * time.Second}
	c := NewMediaClock(simRate, true, nil)
	s.run(c)
	st := c.Stats()

	assert.Zero(t, st.InsertedGaps)
	assert.InDelta(t, 1.02, st.RateRatio, 0.001)
	assert.Positive(t, st.ReportedDriftMs, "ahead of the wall clock")
}

func TestMediaClockCaptureStall(t *testing.T) {
	s := simSender{duration: 20 * time.Second, srEvery: 2 * time.Second}
	s.stall.at, s.stall.dur = 10*time.Second, 2*time.Second
	c := NewMediaClock(simRate, true, nil)
	p := placement(s.run(c))
	st := c.Stats()
	t.Logf("stats %+v", st)

	// One gap of 17 x 120 ms: the stall, and 40 ms more.
	assert.Equal(t, 1, st.InsertedGaps)
	assert.Equal(t, int64(2040), st.InsertedGapMs)
	assert.Equal(t, 40*time.Millisecond, p[len(p)-1].Round(time.Millisecond), "placement at the end: late by the overshoot")
}

func TestMediaClockLatePacketsKeepTheirOffset(t *testing.T) {
	c := NewMediaClock(simRate, true, nil)
	base := time.Unix(1_800_000_000, 0)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	// 5 s of 20 ms frames; the sender loses 100 ms twice, and reports every
	// 500 ms show it.
	ts, sent := uint32(1000), 0
	var pkts []uint32
	for i := range 250 {
		if i == 75 || i == 110 {
			sent += 100
		}
		if i > 0 && i%25 == 0 {
			c.SenderReport(toNTP(time.Duration(sent)*time.Millisecond), ts, at(sent))
		}
		pkts = append(pkts, c.Timestamp(ts, uint16(i), at(sent)))
		ts += 960
		sent += 20
	}

	// Each 100 ms lost passes the trigger and gets one gap.
	st := c.Stats()
	require.Equal(t, 2, st.InsertedGaps, "%+v", st)
	assert.Equal(t, int64(240), st.InsertedGapMs)

	// A frame from before each correction, read late, keeps the offset of its
	// neighbours; a duplicate gets what its first copy got.
	for _, i := range []int{60, 74, 100, 149, 249} {
		assert.Equal(t, pkts[i], c.Timestamp(1000+uint32(i)*960, uint16(i), at(sent)), "frame %d", i)
	}
}

func TestMediaClockReportsOutsideTheStream(t *testing.T) {
	c := NewMediaClock(simRate, true, nil)
	now := time.Unix(1_800_000_000, 0)

	c.SenderReport(toNTP(time.Second), 5000, now)
	assert.Zero(t, c.Stats().SenderReports, "no timestamps to place it against")

	c.Timestamp(5000, 0, now)
	c.SenderReport(toNTP(time.Second), 5000+11*simRate, now)
	c.SenderReport(toNTP(time.Second), 5000, now)
	c.SenderReport(toNTP(time.Second), 5000, now)

	st := c.Stats()
	assert.Equal(t, 1, st.SenderReports)
	assert.Equal(t, 2, st.SenderReportsIgnored, "one 11 s away, one repeated")
}

func TestNTPDuration(t *testing.T) {
	assert.Equal(t, 1500*time.Millisecond, ntpDuration(1<<32|1<<31))
	d := 3_990_000_123*time.Second + 250*time.Millisecond
	assert.Equal(t, d, ntpDuration(toNTP(d-3_990_000_000*time.Second)))
}

func TestMediaClockHealthySenderBehindAnEarlyDelay(t *testing.T) {
	// The delay rises before the first report. The arrivals see a lag there,
	// which the reports must not take for lost media.
	for _, extra := range []time.Duration{50 * time.Millisecond, 300 * time.Millisecond} {
		s := simSender{duration: 30 * time.Second, srEvery: 3 * time.Second, delay: func(t time.Duration) time.Duration {
			if t >= 1200*time.Millisecond && t < 20*time.Second {
				return 40*time.Millisecond + extra
			}

			return 40 * time.Millisecond
		}}
		c := NewMediaClock(simRate, true, nil)
		pkts := s.run(c)
		st := c.Stats()

		assert.Zero(t, st.InsertedGaps, "%v more: %+v", extra, st)
		assert.Less(t, maxAbs(placement(pkts)), time.Microsecond)
	}
}

func TestMediaClockDelayStepAfterABurst(t *testing.T) {
	// A delay step comes while the reports' rate still includes the burst.
	// reportedCap must not keep adding that rate once the deficit stops
	// growing.
	s := simSender{duration: 80 * time.Second, rate: between(5*time.Second, 35*time.Second, 0.955), srEvery: 3 * time.Second,
		delay: func(t time.Duration) time.Duration {
			if t >= 48*time.Second && t < 70*time.Second {
				return 390 * time.Millisecond
			}

			return 40 * time.Millisecond
		}}
	c := NewMediaClock(simRate, true, nil)
	p := placement(s.run(c))
	st := c.Stats()
	t.Logf("stats %+v; final %v", st, p[len(p)-1])

	assert.InDelta(t, 1350, st.InsertedGapMs, 80, "gaps inserted, against the 1.35 s lost")
	assert.LessOrEqual(t, p[len(p)-1].Abs(), mediaClockGap-mediaClockTrigger, "placement at the end")
}

func TestMediaClockDelayDropAfterTheStart(t *testing.T) {
	// 300 ms more delay for the first 1.5 s, then a 0.96 sender: arrivals
	// measured against the start alone would read the lag 300 ms short.
	s := simSender{duration: 50 * time.Second, rate: between(5*time.Second, 35*time.Second, 0.96), srEvery: 3 * time.Second,
		delay: func(t time.Duration) time.Duration {
			if t < 1500*time.Millisecond {
				return 340 * time.Millisecond
			}

			return 40 * time.Millisecond
		}}
	c := NewMediaClock(simRate, true, nil)
	p := placement(s.run(c))
	st := c.Stats()
	t.Logf("stats %+v; final %v", st, p[len(p)-1])

	assert.LessOrEqual(t, p[len(p)-1].Abs(), mediaClockGap-mediaClockTrigger, "placement at the end")
}

func TestMediaClockGivesBackWhatTheSenderRealigns(t *testing.T) {
	// 60 s at 0.96, 2.4 s lost and corrected; then LiveKit realigns the
	// timestamps.
	s := simSender{duration: 90 * time.Second, rate: between(0, 60*time.Second, 0.96), srEvery: 3 * time.Second, realignAt: 60 * time.Second}
	c := NewMediaClock(simRate, true, nil)
	p := placement(s.run(c))
	st := c.Stats()
	t.Logf("stats %+v; final %v", st, p[len(p)-1])

	assert.Equal(t, 1, st.Realignments)
	assert.InDelta(t, 2400, st.RealignedMs, 20, "what LiveKit moved the timestamps by")
	assert.InDelta(t, st.InsertedGapMs, st.GivenBackMs, 80, "the gaps given back")
	assert.LessOrEqual(t, p[len(p)-1].Abs(), mediaClockGap-mediaClockTrigger, "placement at the end")
	// The rate and drift leave the realignment out: 2.4 s lost over 90 s,
	// less the 40 ms the arrival window trails by at the first report.
	assert.InDelta(t, 0.973, st.RateRatio, 0.001)
	assert.InDelta(t, -2360, st.ReportedDriftMs, 20)
}

func TestMediaClockSenderClockStepsBack(t *testing.T) {
	s := simSender{duration: 120 * time.Second, srEvery: 3 * time.Second}
	s.ntpStep.at, s.ntpStep.by = 20*time.Second, -300*time.Second
	c := NewMediaClock(simRate, true, nil)
	s.run(c)
	st := c.Stats()

	assert.Equal(t, 1, st.SenderClockSteps)
	assert.Zero(t, st.InsertedGaps)
	assert.LessOrEqual(t, len(c.srs), mediaClockMaxReports, "reports kept")
}

func TestMediaClockTimestampsJumpBack(t *testing.T) {
	// The timestamps jump 30 s back after 10 s: the clock carries on.
	s := simSender{duration: 40 * time.Second, srEvery: 3 * time.Second}
	s.tsJump.at, s.tsJump.by = 10*time.Second, -30*simRate
	c := NewMediaClock(simRate, true, nil)
	s.run(c)
	st := c.Stats()

	assert.InDelta(t, 40000, st.RTPDurationMs, 40)
	assert.InDelta(t, 40000, st.ArrivalDurationMs, 40)
	assert.Zero(t, st.InsertedGaps)
}

func TestMediaClockReorderedRealignment(t *testing.T) {
	// The first two packets after LiveKit's realignment arrive swapped. The
	// clock sees the jump on the second, then gets the first, whose
	// timestamp is a frame earlier.
	s := simSender{duration: 70 * time.Second, rate: between(0, time.Hour, 0.96), srEvery: 3 * time.Second,
		realignAt: 60 * time.Second, swapAt: 60 * time.Second}
	c := NewMediaClock(simRate, true, nil)
	pkts := s.run(c)
	st := c.Stats()
	t.Logf("stats %+v", st)

	require.Equal(t, 1, st.Realignments)
	for i := 1; i < len(pkts); i++ {
		require.GreaterOrEqual(t, int32(pkts[i].out-pkts[i-1].out), int32(960), "packet %d goes back or overlaps the one before", i)
	}
}

func TestMediaClockStillSlowAfterARealignment(t *testing.T) {
	// The realignment takes the deficit down, not the rate it grows at.
	s := simSender{duration: 100 * time.Second, rate: between(0, time.Hour, 0.96), srEvery: 3 * time.Second, realignAt: 60 * time.Second}
	c := NewMediaClock(simRate, true, nil)
	pkts := s.run(c)
	p := placement(pkts)
	st := c.Stats()
	t.Logf("stats %+v; final %v", st, p[len(p)-1])

	var after []time.Duration
	for i, pkt := range pkts {
		if pkt.sent >= 61*time.Second {
			after = append(after, p[i])
		}
	}
	assert.LessOrEqual(t, maxAbs(after), 105*time.Millisecond, "placement after the realignment")
	// 4 % of the 99 s up to the last report, less the 40 ms the arrival
	// window trails by at the first report.
	assert.InDelta(t, -3920, st.ReportedDriftMs, 20, "drift by the reports")
}

func TestMediaClockLiveKitReportCadence(t *testing.T) {
	// The sender reports every 5 s and LiveKit forwards its latest every 3 s,
	// so a change reaches the clock up to 3 s after the sender measured it.
	steady := func(s simSender) []time.Duration {
		c := NewMediaClock(simRate, true, nil)
		pkts := s.run(c)
		p := placement(pkts)

		var out []time.Duration
		for i, pkt := range pkts {
			if pkt.sent >= 20*time.Second {
				out = append(out, p[i])
			}
		}

		return out
	}

	fixed := steady(simSender{duration: 60 * time.Second, rate: between(5*time.Second, time.Hour, 0.955),
		srEvery: 5 * time.Second, forwardEvery: 3 * time.Second, delay: jitter(3, 40*time.Millisecond, 40*time.Millisecond)})
	// 105 ms as with direct reports, plus 4.5 % of half the 3 s forwarding
	// interval, since the clock does not know when the sender measured.
	assert.LessOrEqual(t, maxAbs(fixed), 173*time.Millisecond, "placement while the deficit grows")

	// With Chrome's report spread, a change can take 10 s to show. Over these
	// runs the worst is 196 ms, and 0.06 % of packets exceed 173 ms.
	var worst time.Duration
	var beyond, total int
	for seed := uint64(1); seed <= 10; seed++ {
		p := steady(simSender{duration: 300 * time.Second, rate: between(5*time.Second, time.Hour, 0.955),
			srEvery: 5 * time.Second, srSpread: rand.New(rand.NewPCG(seed, 7)), forwardEvery: 3 * time.Second,
			delay: jitter(seed, 40*time.Millisecond, 40*time.Millisecond)})
		worst = max(worst, maxAbs(p))
		total += len(p)
		for _, d := range p {
			if d.Abs() > 173*time.Millisecond {
				beyond++
			}
		}
	}
	t.Logf("Chrome cadence: worst %v, %d of %d packets beyond 173 ms", worst, beyond, total)
	assert.LessOrEqual(t, worst, 240*time.Millisecond, "placement with Chrome's report spread")
	assert.LessOrEqual(t, float64(beyond)/float64(total), 0.002, "share beyond 173 ms")
}

func TestMediaClockLongDeficitRealigned(t *testing.T) {
	// 400 s at 0.955 loses 18 s; then LiveKit realigns and the sender is
	// healthy. A delay step after that must not count as lost media. The 18 s
	// realignment exceeds the time between reports plus mediaClockJump, yet
	// must not count as a clock step.
	for _, correcting := range []bool{true, false} {
		s := simSender{duration: 440 * time.Second, rate: between(0, 400*time.Second, 0.955), srEvery: 3 * time.Second, realignAt: 400 * time.Second,
			delay: func(t time.Duration) time.Duration {
				if t >= 410*time.Second && t < 425*time.Second {
					return 340 * time.Millisecond
				}

				return 40 * time.Millisecond
			}}
		c := NewMediaClock(simRate, correcting, nil)
		p := placement(s.run(c))
		st := c.Stats()
		t.Logf("correcting %v: stats %+v; final %v", correcting, st, p[len(p)-1])

		assert.Zero(t, st.SenderClockSteps, "the realignment is not a timestamp jump")
		assert.Equal(t, 1, st.Realignments)
		// The realignment stays out of the rate and drift: 18 s lost, 422 s of
		// media over 440 s.
		assert.InDelta(t, -18000, st.ReportedDriftMs, 100)
		assert.InDelta(t, 0.959, st.RateRatio, 0.001)
		if correcting {
			assert.LessOrEqual(t, p[len(p)-1].Abs(), mediaClockGap-mediaClockTrigger, "placement at the end")
		}
	}
}

func TestMediaClockDelayedPacketInSilence(t *testing.T) {
	// DTX from 20 s on. One packet is delayed 250 ms, and those from 40 to
	// 41.2 s by 300 ms. The packet after each arrives on time: its timestamp
	// moves 400 ms and its arrival less, so both look like jumps that no
	// report confirms.
	delay := func(t time.Duration) time.Duration {
		switch {
		case t >= 30*time.Second && t < 30400*time.Millisecond:
			return 290 * time.Millisecond
		case t >= 40*time.Second && t < 41200*time.Millisecond:
			return 340 * time.Millisecond
		default:
			return 40 * time.Millisecond
		}
	}
	dtx := struct{ from, to time.Duration }{20 * time.Second, time.Hour}

	t.Run("healthy", func(t *testing.T) {
		s := simSender{duration: 60 * time.Second, srEvery: 3 * time.Second, dtx: dtx, delay: delay}
		c := NewMediaClock(simRate, true, nil)
		pkts := s.run(c)
		st := c.Stats()
		t.Logf("stats %+v", st)

		assert.Zero(t, st.Realignments, "no realignment")
		assert.InDelta(t, 1, st.RateRatio, 0.001)
		assert.InDelta(t, 0, st.ReportedDriftMs, 5, "drift by the reports")
		assert.Zero(t, st.InsertedGaps)
		assert.Less(t, maxAbs(placement(pkts)), time.Microsecond)
	})

	t.Run("slow, LiveKit cadence", func(t *testing.T) {
		// Between two of the sender's reports, LiveKit's forwarded ones show
		// no loss at all.
		s := simSender{duration: 60 * time.Second, rate: between(0, time.Hour, 0.955), srEvery: 5 * time.Second,
			srSpread: rand.New(rand.NewPCG(1, 7)), forwardEvery: 3 * time.Second, dtx: dtx, delay: delay}
		c := NewMediaClock(simRate, false, nil)
		s.run(c)
		st := c.Stats()
		t.Logf("stats %+v", st)

		assert.Zero(t, st.Realignments, "no realignment")
		assert.InDelta(t, 0.955, st.RateRatio, 0.001)
	})
}

func TestMediaClockRealignedAmongDelayedPackets(t *testing.T) {
	// A sender at 0.96 in DTX from 5 s, one packet held up by 250 ms every
	// 8 s, then LiveKit realigns at 80 s: only that jump counts.
	s := simSender{duration: 100 * time.Second, rate: between(0, time.Hour, 0.96), srEvery: 3 * time.Second,
		dtx: struct{ from, to time.Duration }{5 * time.Second, 80 * time.Second}, realignAt: 80 * time.Second,
		delay: func(t time.Duration) time.Duration {
			if t >= 10*time.Second && t < 78*time.Second && t%(8*time.Second) < 400*time.Millisecond {
				return 290 * time.Millisecond
			}

			return 40 * time.Millisecond
		}}
	c := NewMediaClock(simRate, false, nil)
	s.run(c)
	st := c.Stats()
	t.Logf("stats %+v", st)

	assert.Equal(t, 1, st.Realignments)
	assert.InDelta(t, 3200, st.RealignedMs, 60, "80 s x 4 %")
	assert.InDelta(t, 0.96, st.RateRatio, 0.002)
}

func TestMediaClockSmallRealignment(t *testing.T) {
	// A sender at 0.96 that LiveKit realigns once it is a little over 200 ms
	// behind.
	for _, at := range []time.Duration{5500 * time.Millisecond, 5800 * time.Millisecond, 6500 * time.Millisecond} {
		s := simSender{duration: 30 * time.Second, rate: between(0, time.Hour, 0.96), srEvery: 3 * time.Second, realignAt: at,
			delay: jitter(4, 40*time.Millisecond, 20*time.Millisecond)}
		c := NewMediaClock(simRate, false, nil)
		s.run(c)
		st := c.Stats()
		t.Logf("realigned at %v: stats %+v", at, st)

		assert.Equal(t, 1, st.Realignments, "realigned at %v", at)
		assert.InDelta(t, 0.04*float64(at.Milliseconds()), st.RealignedMs, 25, "realigned at %v", at)
		assert.InDelta(t, 0.96, st.RateRatio, 0.002, "realigned at %v", at)
	}
}

func TestMediaClockRealignedBetweenSparseReports(t *testing.T) {
	// Reports only every 12 s, and LiveKit realigns 24 s of deficit just
	// after one: the jump waits for the next report, more than 11 s later.
	s := simSender{duration: 520 * time.Second, rate: between(0, 480*time.Second, 0.95), srEvery: 12 * time.Second,
		realignAt: 480500 * time.Millisecond}
	c := NewMediaClock(simRate, false, nil)
	s.run(c)
	st := c.Stats()
	t.Logf("stats %+v", st)

	assert.Equal(t, 1, st.Realignments)
	assert.Zero(t, st.SenderClockSteps, "the realignment is not a timestamp jump")
}
