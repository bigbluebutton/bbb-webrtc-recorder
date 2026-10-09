package diskpressure

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
)

func metricValue(m prometheus.Metric) float64 {
	var pb dto.Metric
	_ = m.Write(&pb)

	switch {
	case pb.Gauge != nil:
		return pb.Gauge.GetValue()
	case pb.Counter != nil:
		return pb.Counter.GetValue()
	default:
		return 0
	}
}

type fakeStat struct {
	mu     sync.Mutex
	sample Sample
	err    error
}

func (f *fakeStat) set(freeMiB, totalMiB uint64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sample = Sample{FreeBytes: freeMiB * bytesPerMiB, TotalBytes: totalMiB * bytesPerMiB}
	f.err = err
}

func (f *fakeStat) stat(string) (Sample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sample, f.err
}

type changes struct {
	mu     sync.Mutex
	states []bool
}

func (c *changes) record(active bool, _ Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.states = append(c.states, active)
}

func (c *changes) get() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]bool(nil), c.states...)
}

func newTestMonitor(cfg config.DiskPressure) (*Monitor, *fakeStat, *changes) {
	cfg.Path = "/rec"
	cfg.CheckInterval = time.Hour
	cfg.VideoQuality = "LOW"
	cfg.Sources = []string{"camera"}
	fs := &fakeStat{}
	ch := &changes{}
	m := NewMonitor(cfg, fs.stat)
	m.OnChange(ch.record)

	return m, fs, ch
}

func TestMonitorHysteresis(t *testing.T) {
	type step struct {
		freeMiB uint64
		active  bool
	}

	testCases := []struct {
		name     string
		cfg      config.DiskPressure
		totalMiB uint64
		steps    []step
	}{
		{
			name:     "MiB only",
			cfg:      config.DiskPressure{EngageFreeMiB: 100, ReleaseFreeMiB: 150},
			totalMiB: 1000,
			steps: []step{
				{200, false}, {100, false}, {99, true}, {149, true}, {150, false}, {120, false}, {99, true},
			},
		},
		{
			name:     "percent only",
			cfg:      config.DiskPressure{EngageFreePercent: 10, ReleaseFreePercent: 15},
			totalMiB: 1000,
			steps: []step{
				{200, false}, {99, true}, {149, true}, {150, false},
			},
		},
		{
			name: "percent trips while MiB is clear",
			cfg: config.DiskPressure{
				EngageFreeMiB: 100, ReleaseFreeMiB: 150, EngageFreePercent: 10, ReleaseFreePercent: 15,
			},
			totalMiB: 2000,
			steps: []step{
				// 10% is 200 MiB, 15% is 300 MiB.
				{250, false}, {199, true}, {299, true}, {300, false},
			},
		},
		{
			name: "MiB trips while percent is clear, release needs both",
			cfg: config.DiskPressure{
				EngageFreeMiB: 300, ReleaseFreeMiB: 400, EngageFreePercent: 10, ReleaseFreePercent: 15,
			},
			totalMiB: 2000,
			steps: []step{
				{350, false}, {250, true}, {350, true}, {400, false},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m, fs, ch := newTestMonitor(tc.cfg)
			var want []bool

			for i, s := range tc.steps {
				fs.set(s.freeMiB, tc.totalMiB, nil)

				if i == 0 {
					m.Start()
				} else {
					m.check()
				}

				assert.Equal(t, s.active, m.Active(), "step %d (%d MiB free)", i, s.freeMiB)

				if wasActive := len(want) > 0 && want[len(want)-1]; s.active != wasActive {
					want = append(want, s.active)
				}
			}

			m.Stop()
			assert.Equal(t, want, ch.get(), "OnChange fires once per transition")
		})
	}
}

func TestMonitorStartEngagesSynchronously(t *testing.T) {
	m, fs, ch := newTestMonitor(config.DiskPressure{EngageFreeMiB: 100, ReleaseFreeMiB: 150})
	fs.set(10, 1000, nil)

	m.Start()
	defer m.Stop()

	assert.True(t, m.Active())
	assert.Equal(t, []bool{true}, ch.get())
	assert.Equal(t, 1.0, metricValue(appstats.DiskPressureActive))
	assert.Equal(t, 1.0, metricValue(appstats.DiskPressureEnabled))
	assert.Equal(t, float64(10*bytesPerMiB), metricValue(appstats.DiskFreeBytes))
	assert.Equal(t, float64(1000*bytesPerMiB), metricValue(appstats.DiskTotalBytes))
}

func TestMonitorTransitionMetrics(t *testing.T) {
	engaged := appstats.DiskPressureTransitions.WithLabelValues("engaged")
	released := appstats.DiskPressureTransitions.WithLabelValues("released")
	engagedBefore := metricValue(engaged)
	releasedBefore := metricValue(released)

	m, fs, _ := newTestMonitor(config.DiskPressure{EngageFreeMiB: 100, ReleaseFreeMiB: 150})
	fs.set(10, 1000, nil)
	m.Start()
	fs.set(20, 1000, nil)
	m.check()
	fs.set(500, 1000, nil)
	m.check()
	m.Stop()

	assert.Equal(t, engagedBefore+1, metricValue(engaged))
	assert.Equal(t, releasedBefore+1, metricValue(released))
	assert.Equal(t, 0.0, metricValue(appstats.DiskPressureActive))
	assert.Equal(t, 0.0, metricValue(appstats.DiskPressureEnabled))
}

func TestMonitorStatErrorsKeepState(t *testing.T) {
	errorsBefore := metricValue(appstats.DiskCheckErrors)
	m, fs, ch := newTestMonitor(config.DiskPressure{EngageFreeMiB: 100, ReleaseFreeMiB: 150})

	fs.set(10, 1000, nil)
	m.Start()
	assert.True(t, m.Active())

	fs.set(0, 0, errors.New("statfs failed"))
	m.check()
	m.check()
	assert.True(t, m.Active())
	assert.True(t, m.failing)

	// A zero-sized filesystem is an error, not 0% free.
	fs.set(0, 0, nil)
	m.check()
	assert.True(t, m.Active())
	assert.Equal(t, errorsBefore+3, metricValue(appstats.DiskCheckErrors))

	fs.set(500, 1000, nil)
	m.check()
	assert.False(t, m.Active())
	assert.False(t, m.failing)
	assert.Equal(t, []bool{true, false}, ch.get())

	m.Stop()
}

func TestMonitorErrorAtStartLeavesGuardOff(t *testing.T) {
	m, fs, ch := newTestMonitor(config.DiskPressure{EngageFreeMiB: 100, ReleaseFreeMiB: 150})
	fs.set(0, 0, errors.New("no such file or directory"))

	m.Start()
	defer m.Stop()

	assert.False(t, m.Active())
	assert.Empty(t, ch.get())
}

func TestMonitorTicks(t *testing.T) {
	fs := &fakeStat{}
	ch := &changes{}
	m := NewMonitor(config.DiskPressure{
		Path: "/rec", CheckInterval: 10 * time.Millisecond, EngageFreeMiB: 100, ReleaseFreeMiB: 150,
	}, fs.stat)
	m.OnChange(ch.record)

	fs.set(500, 1000, nil)
	m.Start()
	fs.set(10, 1000, nil)

	assert.Eventually(t, m.Active, time.Second, 5*time.Millisecond)
	m.Stop()
	m.Stop()
	assert.Equal(t, []bool{true}, ch.get())
}

func TestStatfs(t *testing.T) {
	s, err := Statfs(t.TempDir())

	assert.NoError(t, err)
	assert.Positive(t, s.TotalBytes)
	assert.LessOrEqual(t, s.FreeBytes, s.TotalBytes)

	_, err = Statfs("/this/path/does/not/exist")
	assert.Error(t, err)
}

// hangingStat makes stat calls block until gate is closed while hang is
// set, like statfs on an unreachable network mount.
type hangingStat struct {
	fakeStat
	hang    atomic.Bool
	hangs   atomic.Int32
	gate    chan struct{}
	entered chan struct{}
}

func newHangingStat() *hangingStat {
	return &hangingStat{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (h *hangingStat) stat(path string) (Sample, error) {
	if h.hang.Load() {
		h.hangs.Add(1)

		select {
		case h.entered <- struct{}{}:
		default:
		}
		<-h.gate
	}

	return h.fakeStat.stat(path)
}

func within(timeout time.Duration, fn func()) bool {
	done := make(chan struct{})

	go func() {
		defer close(done)
		fn()
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestMonitorStartBoundsHangingFirstCheck(t *testing.T) {
	errorsBefore := metricValue(appstats.DiskCheckErrors)
	hs := newHangingStat()
	hs.set(10, 1000, nil)
	hs.hang.Store(true)
	ch := &changes{}
	m := NewMonitor(config.DiskPressure{
		Path: "/rec", CheckInterval: 20 * time.Millisecond, EngageFreeMiB: 100, ReleaseFreeMiB: 150,
	}, hs.stat)
	m.OnChange(ch.record)

	assert.True(t, within(time.Second, m.Start), "Start waits for a hanging statfs")
	assert.False(t, m.Active())
	assert.GreaterOrEqual(t, metricValue(appstats.DiskCheckErrors), errorsBefore+1)

	// The guard keeps ticking and engages once statfs answers.
	hs.hang.Store(false)
	close(hs.gate)
	assert.Eventually(t, m.Active, time.Second, 5*time.Millisecond)
	m.Stop()
	assert.Equal(t, []bool{true}, ch.get())
}

func TestMonitorEvaluatesLateResultOnArrival(t *testing.T) {
	newMonitor := func(err error) *Monitor {
		hs := newHangingStat()
		hs.set(10, 1000, err)
		hs.hang.Store(true)
		m := NewMonitor(config.DiskPressure{
			Path: "/rec", CheckInterval: time.Second, EngageFreeMiB: 100, ReleaseFreeMiB: 150,
		}, hs.stat)
		m.OnChange((&changes{}).record)
		m.Start()

		// The next tick is a second away.
		hs.hang.Store(false)
		close(hs.gate)

		return m
	}

	t.Run("sample", func(t *testing.T) {
		m := newMonitor(nil)
		defer m.Stop()

		assert.Eventually(t, m.Active, 500*time.Millisecond, 5*time.Millisecond)
	})

	t.Run("error", func(t *testing.T) {
		hook := logtest.NewGlobal()
		defer log.StandardLogger().ReplaceHooks(make(log.LevelHooks))

		errorsBefore := metricValue(appstats.DiskCheckErrors)
		m := newMonitor(errors.New("input/output error"))
		defer m.Stop()

		// The first check's timeout already counted it.
		assert.Eventually(t, func() bool {
			return hasError(hook, "input/output error")
		}, 500*time.Millisecond, 5*time.Millisecond)
		assert.Equal(t, errorsBefore+1, metricValue(appstats.DiskCheckErrors))
	})
}

func TestMonitorLogsErrorAfterTimedOutFirstCheck(t *testing.T) {
	hook := logtest.NewGlobal()
	defer log.StandardLogger().ReplaceHooks(make(log.LevelHooks))

	hs := newHangingStat()
	hs.set(0, 0, errors.New("stale file handle"))
	hs.hang.Store(true)
	m := NewMonitor(config.DiskPressure{
		Path: "/rec", CheckInterval: 20 * time.Millisecond, EngageFreeMiB: 100, ReleaseFreeMiB: 150,
	}, hs.stat)
	m.OnChange((&changes{}).record)
	m.Start()
	defer m.Stop()

	// The hang outlasts a tick, which logs the timeout at Error.
	assert.Eventually(t, func() bool {
		return hasError(hook, errCheckHung.Error())
	}, time.Second, 5*time.Millisecond)

	hs.hang.Store(false)
	close(hs.gate)
	assert.Eventually(t, func() bool {
		return hasError(hook, "stale file handle")
	}, time.Second, 5*time.Millisecond)
}

// newSlowMonitor checks every 20ms with a stat that always answers after
// 60ms, like a mount slower than the check timeout.
func newSlowMonitor(fs *fakeStat) *Monitor {
	slow := func(path string) (Sample, error) {
		time.Sleep(60 * time.Millisecond)

		return fs.stat(path)
	}
	m := NewMonitor(config.DiskPressure{
		Path: "/rec", CheckInterval: 20 * time.Millisecond, EngageFreeMiB: 100, ReleaseFreeMiB: 150,
	}, slow)
	m.OnChange((&changes{}).record)

	return m
}

func TestMonitorUsesSamplesFromSlowChecks(t *testing.T) {
	hook := logtest.NewGlobal()
	defer log.StandardLogger().ReplaceHooks(make(log.LevelHooks))

	fs := &fakeStat{}
	fs.set(10, 1000, nil)
	m := newSlowMonitor(fs)
	m.Start()
	defer m.Stop()

	// Every check times out, yet each sample still reaches the guard on a
	// later tick, without logging a recovery.
	assert.Eventually(t, m.Active, time.Second, 5*time.Millisecond)
	assert.Never(t, func() bool {
		return hasEntry(hook, log.InfoLevel, "Disk pressure check recovered")
	}, 200*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, 1, countEntries(hook, log.ErrorLevel, "Disk pressure check failed, keeping current state"))
}

func TestMonitorLogsEachFailureKindOncePerOutage(t *testing.T) {
	hook := logtest.NewGlobal()
	defer log.StandardLogger().ReplaceHooks(make(log.LevelHooks))

	fs := &fakeStat{}
	fs.set(0, 0, errors.New("input/output error"))
	m := newSlowMonitor(fs)
	m.Start()

	// Checks alternate between timing out and the late statfs error,
	// which logs each at Error once.
	assert.Never(t, func() bool {
		return countEntries(hook, log.ErrorLevel, "Disk pressure check failed, keeping current state") > 2
	}, 300*time.Millisecond, 5*time.Millisecond)
	m.Stop()
	assert.True(t, hasError(hook, errCheckHung.Error()) || hasError(hook, errCheckTimeout.Error()))
	assert.True(t, hasError(hook, "input/output error"))
}

func TestMonitorBoundsHangingLaterCheck(t *testing.T) {
	hook := logtest.NewGlobal()
	defer log.StandardLogger().ReplaceHooks(make(log.LevelHooks))

	errorsBefore := metricValue(appstats.DiskCheckErrors)
	hs := newHangingStat()
	hs.set(10, 1000, nil)
	ch := &changes{}
	m := NewMonitor(config.DiskPressure{
		Path: "/rec", CheckInterval: 20 * time.Millisecond, EngageFreeMiB: 100, ReleaseFreeMiB: 150,
	}, hs.stat)
	m.OnChange(ch.record)
	m.Start()
	defer m.Stop()

	hs.hang.Store(true)
	<-hs.entered

	// Every tick the hang outlives counts as a failed check, none starts
	// another statfs behind the hung one, and the guard stays engaged.
	assert.Eventually(t, func() bool {
		return metricValue(appstats.DiskCheckErrors) >= errorsBefore+3
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, int32(1), hs.hangs.Load())
	assert.True(t, hasError(hook, errCheckTimeout.Error()))
	assert.True(t, m.Active())

	hs.set(500, 1000, nil)
	hs.hang.Store(false)
	close(hs.gate)
	assert.Eventually(t, func() bool {
		return hasEntry(hook, log.InfoLevel, "Disk pressure check recovered")
	}, time.Second, 5*time.Millisecond)
	m.Stop()
	assert.False(t, m.Active())
	assert.Equal(t, []bool{true, false}, ch.get())
}

func countEntries(hook *logtest.Hook, level log.Level, msg string) int {
	n := 0

	for _, e := range hook.AllEntries() {
		if e.Level == level && e.Message == msg {
			n++
		}
	}

	return n
}

func hasEntry(hook *logtest.Hook, level log.Level, msg string) bool {
	return countEntries(hook, level, msg) > 0
}

// hasError reports whether a failed check was logged at Error with err.
func hasError(hook *logtest.Hook, err string) bool {
	for _, e := range hook.AllEntries() {
		if e.Level == log.ErrorLevel && e.Message == "Disk pressure check failed, keeping current state" &&
			fmt.Sprint(e.Data[log.ErrorKey]) == err {
			return true
		}
	}

	return false
}

func TestMonitorStopBoundsHangingCheck(t *testing.T) {
	hs := newHangingStat()
	hs.set(500, 1000, nil)
	ch := &changes{}
	m := NewMonitor(config.DiskPressure{
		Path: "/rec", CheckInterval: 20 * time.Millisecond, EngageFreeMiB: 100, ReleaseFreeMiB: 150,
	}, hs.stat)
	m.OnChange(ch.record)
	m.Start()

	hs.hang.Store(true)
	<-hs.entered
	assert.True(t, within(time.Second, m.Stop), "Stop waits for a hanging statfs")
	assert.Equal(t, 0.0, metricValue(appstats.DiskPressureEnabled))

	// A check that answers after Stop gave up on it changes nothing.
	hs.set(10, 1000, nil)
	close(hs.gate)
	assert.Never(t, m.Active, 100*time.Millisecond, 5*time.Millisecond)
	assert.Empty(t, ch.get())
}
