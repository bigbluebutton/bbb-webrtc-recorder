package utils

import (
	"math"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	log "github.com/sirupsen/logrus"
)

const (
	// The lowest lateness over this window is the arrival estimate of how far
	// the media clock is behind. Network delay only adds to a packet's
	// lateness, so one packet on time in the window is enough. A longer
	// window trails a slow clock by more.
	mediaClockArrivalWindow = time.Second
	// Playback (record-and-playback's aresample=async) fills a timestamp gap
	// over 100 ms with silence. It absorbs a smaller gap by stretching the
	// audio up to 1 %, and inserts silence once the audio falls 100 ms
	// behind. So a correction is a gap of a multiple of 120 ms (six Opus
	// frames), inserted once the lag reaches 40 ms. The audio then stays
	// within about 80 ms of its place.
	mediaClockGap     = 120 * time.Millisecond
	mediaClockTrigger = 40 * time.Millisecond
	// Opus frames are 20 ms: giving an offset back leaves at least that much
	// between two timestamps, so that no two frames overlap.
	mediaClockFrame = 20 * time.Millisecond
	// The deficit rate the reports show is measured over this much arrival
	// time, once they cover mediaClockRateMinSpan of it.
	mediaClockRateWindow  = 20 * time.Second
	mediaClockRateMinSpan = 5 * time.Second
	mediaClockMaxRate     = 0.25
	mediaClockMaxReports  = 32
	// Without reports, unconfirmed jumps would pile up: the newest are kept.
	mediaClockMaxJumps = 32
	// seed is updated from the reports' rate only for this long after the
	// first report.
	mediaClockSeedWindow = 30 * time.Second
	// LiveKit forwards a report every 3 s, built from the publisher's latest;
	// Chrome sends those every 2.5 to 7.5 s. A deficit unchanged for longer
	// has stopped growing, so reportedCap no longer adds its rate.
	mediaClockReportGap = 11 * time.Second
	// LiveKit moves a downtrack's audio timestamps to the wall clock when the
	// publisher mutes and unmutes, once they are 200 ms behind. A jump this
	// far ahead of the packet before it may be that; the margin under 200 ms
	// allows for network jitter. A network delay that drops in silence makes
	// such jumps too, so a jump counts only once a report shows media gained
	// of at least a quarter of it. The quarter leaves room for the sender's
	// own loss over the report interval.
	mediaClockRealign = 150 * time.Millisecond
	// Reports whose losses differ by no more than this show the same.
	mediaClockSameDeficit = 5 * time.Millisecond
	// A report whose NTP time moves this much more or less than its arrival
	// time comes from a sender clock that stepped.
	mediaClockNTPStep = time.Second
	// A report whose RTP time is this far from the stream's is ignored. A
	// packet this far behind the newest is not late: the timestamps jumped
	// back, and the clock carries on from it.
	mediaClockJump = 10 * time.Second
	// The newest corrections are kept, so that a late packet gets the offset
	// of its neighbours.
	mediaClockSteps = 16
)

// MediaClock keeps one audio stream's RTP timestamps on the wall clock when
// the sender's media clock falls behind it. When a sender loses captured
// audio before encoding it, its timestamps count only the samples it kept,
// and a writer that times frames by them plays them early. Timestamp moves
// each timestamp forward by the time lost, in steps of mediaClockGap, which
// the writer leaves as a gap between frames.
//
// Two estimates of the time lost bound each other. Arrival times measure it
// finely, but cannot tell it from a network delay that grows for over a
// second. Sender reports carry the sender's NTP and RTP clocks, which
// network delay does not move, but come only every few seconds. The
// correction follows arrivals, up to what the reports show. With no
// report, nothing is corrected.
//
// A timestamp jump ahead of the arrivals takes back as much of the offset
// as it covers: LiveKit itself moves audio timestamps to the wall clock
// when the publisher mutes and unmutes. Nothing else moves timestamps
// back, so a sender whose media clock runs fast is only measured, as is
// every sender when correcting is off.
//
// The methods are safe for concurrent use.
type MediaClock struct {
	mu         sync.Mutex
	logger     *log.Entry
	correcting bool

	rate    float64 // ticks per second
	gap     int64   // ticks
	trigger int64   // ticks
	frame   int64   // ticks

	started bool
	firstAt time.Time
	highTS  uint32
	highExt int64 // the first packet's is 0
	highSeq uint16
	lastAt  time.Time

	// window holds the lateness of recent packets, increasing, so its first
	// entry is the lowest within mediaClockArrivalWindow; floor is the lowest
	// ever, the network delay with no time lost.
	window []lateness
	floor  int64 // ticks

	offset int64       // ticks added to new timestamps
	steps  []clockStep // the newest offset changes, oldest first
	base   int64       // the offset before the oldest kept step
	// jumps are the timestamp jumps ahead of arrival that no report has
	// confirmed yet. LiveKit moves its reports along with the timestamps, so
	// the reports after a realignment show it as media gained. reportedAhead
	// is what they confirmed.
	jumps         []timestampJump
	reportedAhead int64

	srs []senderInfo // the newest reports, oldest first
	// The reports measure the deficit only from the first one, at firstReport.
	// The loss before it, seed, is the arrival lag then (preLag, preSpan after
	// the first packet), capped by the reports' rate over preSpan.
	firstReport time.Time
	preLag      int64
	preSpan     time.Duration
	seed        int64

	stats types.MediaClockStats
}

type lateness struct {
	at time.Time
	r  int64 // ticks: arrival time since the first packet, less media time
}

type timestampJump struct {
	at    time.Time
	ticks int64
}

type clockStep struct {
	fromExt int64
	offset  int64
}

type senderInfo struct {
	at      time.Time
	ntp     time.Duration // since the NTP epoch
	ext     int64
	deficit int64 // ticks lost since the first report
	// gained counts only the losses: a realignment takes the deficit down
	// without the sender losing less.
	gained int64
}

// NewMediaClock returns a clock for a stream of clockRate ticks per second,
// which moves timestamps only if correcting. logger, if not nil, gets the
// first gap inserted, the first realignment and the first gap given back.
func NewMediaClock(clockRate uint32, correcting bool, logger *log.Entry) *MediaClock {
	rate := float64(clockRate)
	ticks := func(d time.Duration) int64 { return int64(math.Round(rate * d.Seconds())) }

	return &MediaClock{
		logger:     logger,
		correcting: correcting,
		rate:       rate,
		gap:        ticks(mediaClockGap),
		trigger:    ticks(mediaClockTrigger),
		frame:      ticks(mediaClockFrame),
	}
}

// Timestamp returns ts moved forward by the time the sender's media clock
// has lost, for packet seq that arrived at at.
func (c *MediaClock) Timestamp(ts uint32, seq uint16, at time.Time) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started {
		c.started = true
		c.firstAt, c.lastAt = at, at
		c.highTS, c.highSeq = ts, seq
		c.window = append(c.window, lateness{at: at})

		return ts
	}

	ext := c.highExt + int64(int32(ts-c.highTS))
	if ext <= c.highExt {
		if c.highExt-ext <= c.ticks(mediaClockJump) {
			// Reordered, repeated or retransmitted: it keeps the offset of the
			// timestamps around it, and its late arrival measures nothing.
			return uint32(int64(ts) + c.offsetFor(ext))
		}

		ext = c.highExt + c.frame
	}

	prevExt, step := c.highExt, ext-c.highExt
	missing := int64(seq-c.highSeq) - 1
	// The frames still missing between the two account for part of the step:
	// newer packets that overtake delayed ones are not a realignment.
	if jump := step - max(missing, 0)*c.frame - c.ticks(at.Sub(c.lastAt)); jump >= c.ticks(mediaClockRealign) {
		c.dropJumps(at)
		c.jumps = append(c.jumps, timestampJump{at: at, ticks: jump})
	}
	c.highTS, c.highExt, c.highSeq, c.lastAt = ts, ext, seq, at

	r := c.ticks(at.Sub(c.firstAt)) - ext
	c.floor = min(c.floor, r)
	for len(c.window) > 0 && c.window[len(c.window)-1].r >= r {
		c.window = c.window[:len(c.window)-1]
	}
	c.window = append(c.window, lateness{at: at, r: r})
	for c.window[0].at.Before(at.Add(-mediaClockArrivalWindow)) {
		c.window = c.window[1:]
	}

	lag := c.window[0].r - c.floor
	c.giveBack(lag, prevExt, step, missing)
	c.catchUp(lag, ext, at)

	c.stats.LagMs = c.ms(lag - c.offset)
	c.stats.MaxLagMs = max(c.stats.MaxLagMs, c.stats.LagMs)

	return uint32(int64(ts) + c.offset)
}

// giveBack takes off the offset what a timestamp jump of step ticks from
// prevExt moved ahead of the arrivals. An excess under one gap is catchUp's
// own rounding up, and stays.
//
// A missing packet between the two may still arrive. One in the first half
// of the jump keeps the old offset, one in the second half the new. room
// leaves a frame for each missing packet on each side, and one for the new
// packet, so that they all stay in order.
func (c *MediaClock) giveBack(lag, prevExt, step, missing int64) {
	over := c.offset - max(lag, 0)
	room := step - (2*missing+1)*c.frame
	if over < c.gap || room <= 0 || missing < 0 {
		return
	}

	back := min(over, room)
	c.setOffset(c.offset-back, prevExt+step/2+1)
	c.stats.GivenBackMs += c.ms(back)

	if c.stats.GivenBackMs == c.ms(back) && c.logger != nil {
		c.logger.WithField("lagMs", c.ms(lag)).
			WithField("backMs", c.ms(back)).
			Info("Audio timestamps jumped ahead of their arrival: giving back the gaps they cover")
	}
}

// catchUp inserts a gap from ext on once lag, capped by reportedCap,
// exceeds the offset by at least mediaClockTrigger. The gap is the smallest
// multiple of mediaClockGap that brings that excess under mediaClockTrigger.
func (c *MediaClock) catchUp(lag, ext int64, at time.Time) {
	if !c.correcting || len(c.srs) == 0 {
		return
	}

	behind := min(lag, c.reportedCap(at)) - c.offset
	if behind < c.trigger {
		return
	}

	add := ((behind-c.trigger)/c.gap + 1) * c.gap
	c.setOffset(c.offset+add, ext)
	c.stats.InsertedGaps++
	c.stats.InsertedGapMs += c.ms(add)

	if c.stats.InsertedGaps == 1 && c.logger != nil {
		c.logger.WithField("lagMs", c.ms(lag)).
			WithField("reportedDriftMs", c.stats.ReportedDriftMs).
			WithField("gapMs", c.ms(add)).
			Info("Audio media clock behind the wall clock: inserting timestamp gaps")
	}
}

func (c *MediaClock) setOffset(offset, fromExt int64) {
	c.offset = offset
	c.steps = append(c.steps, clockStep{fromExt: fromExt, offset: offset})
	if len(c.steps) > mediaClockSteps {
		c.base = c.steps[0].offset
		c.steps = c.steps[1:]
	}
}

// reportedCap is how much a correction may cover now: the deficit the
// newest report shows, plus, while the sender still loses media, the rate
// of that loss over the time since it last grew. A report arrives up to a
// forwarding interval after the sender measured it, so that time counts
// from halfway between the last report before the change and the first
// after.
func (c *MediaClock) reportedCap(at time.Time) int64 {
	newest := c.srs[len(c.srs)-1]
	i := len(c.srs) - 1
	for i > 0 && newest.gained-c.srs[i-1].gained <= c.ticks(mediaClockSameDeficit) {
		i--
	}
	changedAt := c.srs[i].at
	if i > 0 {
		changedAt = changedAt.Add(-c.srs[i].at.Sub(c.srs[i-1].at) / 2)
	}

	limit := c.seed + newest.deficit
	if age := at.Sub(changedAt); age <= mediaClockReportGap {
		limit += int64(c.reportedRate() * float64(c.ticks(age)))
	}

	return limit
}

// reportedRate is the share of the time the reports show lost over the
// newest mediaClockRateWindow, or 0 while they cover too little of it.
func (c *MediaClock) reportedRate() float64 {
	oldest, newest := c.srs[0], c.srs[len(c.srs)-1]
	span := newest.at.Sub(oldest.at)
	if span < mediaClockRateMinSpan {
		return 0
	}

	rate := float64(newest.gained-oldest.gained) / float64(c.ticks(span))

	return min(max(rate, 0), mediaClockMaxRate)
}

// SenderReport takes a sender report for the stream, received at at, with
// its 64-bit NTP time and its RTP time.
func (c *MediaClock) SenderReport(ntp uint64, rtpTime uint32, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started {
		// No timestamps to place it against yet; the next one comes soon.
		return
	}

	ext := c.highExt + int64(int32(rtpTime-c.highTS))
	if d := ext - c.highExt; d > c.ticks(mediaClockJump) || d < -c.ticks(mediaClockJump) {
		c.stats.SenderReportsIgnored++

		return
	}

	sr := senderInfo{at: at, ntp: ntpDuration(ntp), ext: ext}
	if n := len(c.srs); n == 0 {
		c.firstReport = at
		c.preLag = max(c.window[0].r-c.floor, 0)
		c.preSpan = at.Sub(c.firstAt)
	} else {
		last := c.srs[n-1]
		if sr.ntp == last.ntp && sr.ext == last.ext {
			c.stats.SenderReportsIgnored++

			return
		}

		c.dropJumps(at)
		elapsed := sr.ntp - last.ntp
		lost := c.ticks(elapsed) - (sr.ext - last.ext)
		step := elapsed - at.Sub(last.at)
		bound := c.ticks(elapsed.Abs() + mediaClockJump)
		jump, jumped := c.largestJump(4 * -lost)
		realigned := jumped && abs(lost+jump.ticks) <= bound
		if step > mediaClockNTPStep || step < -mediaClockNTPStep || (abs(lost) > bound && !realigned) {
			// What a clock step or a timestamp jump adds is not media lost.
			lost = 0
			c.stats.SenderClockSteps++
		} else if jumped {
			c.confirmRealignment(jump.ticks)
		}
		sr.deficit = last.deficit + lost
		sr.gained = last.gained + max(lost, 0)
	}

	c.srs = append(c.srs, sr)
	for len(c.srs) > 2 && (len(c.srs) > mediaClockMaxReports || at.Sub(c.srs[1].at) >= mediaClockRateWindow) {
		c.srs = c.srs[1:]
	}

	// A sender that lost nothing since its first report lost nothing before
	// it either: what arrivals showed then was the network.
	if at.Sub(c.firstReport) <= mediaClockSeedWindow {
		c.seed = max(c.seed, min(c.preLag, int64(c.reportedRate()*float64(c.ticks(c.preSpan)))))
	}

	c.stats.SenderReports++
	c.stats.ReportedDriftMs = -c.ms(c.seed + sr.deficit + c.reportedAhead)
}

// dropJumps forgets the jumps that a report after them did not confirm
// within mediaClockReportGap: a network delay that dropped made them.
func (c *MediaClock) dropJumps(at time.Time) {
	var lastReport time.Time
	if n := len(c.srs); n > 0 {
		lastReport = c.srs[n-1].at
	}

	for len(c.jumps) > 0 && (len(c.jumps) >= mediaClockMaxJumps ||
		at.Sub(c.jumps[0].at) > mediaClockReportGap && c.jumps[0].at.Before(lastReport)) {
		c.jumps = c.jumps[1:]
	}
}

// largestJump is the largest unconfirmed jump of at most limit ticks.
func (c *MediaClock) largestJump(limit int64) (timestampJump, bool) {
	var found timestampJump
	for _, j := range c.jumps {
		if j.ticks <= limit && j.ticks > found.ticks {
			found = j
		}
	}

	return found, found.ticks > 0
}

// confirmRealignment counts a jump of ticks that a report showed. Any other
// jump still unconfirmed was the network's.
func (c *MediaClock) confirmRealignment(ticks int64) {
	c.reportedAhead += ticks
	c.stats.Realignments++
	c.stats.RealignedMs += c.ms(ticks)
	c.jumps = c.jumps[:0]

	if c.stats.Realignments == 1 && c.logger != nil {
		c.logger.WithField("jumpMs", c.ms(ticks)).
			Info("Audio timestamps jumped ahead of their arrival: LiveKit moved them to the wall clock")
	}
}

// offsetFor is the offset the timestamp ext got, or would have got.
func (c *MediaClock) offsetFor(ext int64) int64 {
	for i := len(c.steps) - 1; i >= 0; i-- {
		if c.steps[i].fromExt <= ext {
			return c.steps[i].offset
		}
	}

	return c.base
}

// Stats returns the stream's media clock stats so far.
func (c *MediaClock) Stats() types.MediaClockStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.stats
	s.RTPDurationMs = c.ms(c.highExt)
	s.ArrivalDurationMs = c.lastAt.Sub(c.firstAt).Milliseconds()
	if s.ArrivalDurationMs > 0 {
		s.RateRatio = float64(s.RTPDurationMs-s.RealignedMs) / float64(s.ArrivalDurationMs)
	}

	return s
}

func (c *MediaClock) ticks(d time.Duration) int64 {
	return int64(math.Round(d.Seconds() * c.rate))
}

func (c *MediaClock) ms(ticks int64) int64 {
	return int64(math.Round(float64(ticks) * 1000 / c.rate))
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}

// ntpDuration is a 64-bit NTP time as the time since the NTP epoch.
func ntpDuration(ntp uint64) time.Duration {
	secs := time.Duration(ntp>>32) * time.Second
	frac := time.Duration((ntp & 0xFFFFFFFF) * uint64(time.Second) >> 32)

	return secs + frac
}
