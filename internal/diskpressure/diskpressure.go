// Package diskpressure watches free space on the recordings filesystem
// and reports when it crosses the configured engage and release levels.
package diskpressure

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	bytesPerMiB     = 1024 * 1024
	maxCheckTimeout = 5 * time.Second
)

var (
	errZeroTotal    = errors.New("filesystem reports a total size of zero")
	errCheckTimeout = errors.New("filesystem check timed out")
	errCheckHung    = fmt.Errorf("%w: the previous check is still running", errCheckTimeout)
)

type Sample struct {
	FreeBytes  uint64
	TotalBytes uint64
}

func (s Sample) FreePercent() float64 {
	if s.TotalBytes == 0 {
		return 0
	}

	return float64(s.FreeBytes) / float64(s.TotalBytes) * 100
}

type StatFunc func(path string) (Sample, error)

// Statfs counts free space as the blocks available to unprivileged users.
func Statfs(path string) (Sample, error) {
	var fsStat unix.Statfs_t

	if err := unix.Statfs(path, &fsStat); err != nil {
		return Sample{}, err
	}

	bsize := blockSize(&fsStat)

	return Sample{FreeBytes: fsStat.Bavail * bsize, TotalBytes: fsStat.Blocks * bsize}, nil
}

// ChangeFunc runs on each pressure transition (engage/release).
type ChangeFunc func(active bool, sample Sample)

type Monitor struct {
	cfg      config.DiskPressure
	stat     StatFunc
	onChange ChangeFunc
	active   atomic.Bool

	// Owned by the goroutine running checks.
	failing   bool
	stalled   bool
	timedOut  bool
	engagedAt time.Time
	// hung receives the result of a timed-out check once it returns.
	hung chan statResult

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewMonitor takes a config already passed through Normalize.
func NewMonitor(cfg config.DiskPressure, stat StatFunc) *Monitor {
	return &Monitor{
		cfg:  cfg,
		stat: stat,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// OnChange must be called before Start.
func (m *Monitor) OnChange(fn ChangeFunc) {
	m.onChange = fn
}

func (m *Monitor) Active() bool {
	return m.active.Load()
}

// Start runs the first check before returning, so the state is known
// before any capture starts, then checks on every interval until Stop.
// A first check still running after checkTimeout counts as failed and the
// pressure mechanism starts disengaged.
func (m *Monitor) Start() {
	r := m.timedStat()
	logger := log.WithFields(log.Fields{
		"path":               m.cfg.Path,
		"checkInterval":      m.cfg.CheckInterval.String(),
		"engageFreeMiB":      m.cfg.EngageFreeMiB,
		"releaseFreeMiB":     m.cfg.ReleaseFreeMiB,
		"engageFreePercent":  m.cfg.EngageFreePercent,
		"releaseFreePercent": m.cfg.ReleaseFreePercent,
		"videoQuality":       m.cfg.VideoQuality,
		"sources":            m.cfg.Sources,
	})

	if r.err == nil {
		logger = logger.WithFields(log.Fields{
			"freeBytes":   r.sample.FreeBytes,
			"freePercent": formatPercent(r.sample.FreePercent()),
		})
	}

	logger.Info("Disk pressure guard started")
	appstats.SetDiskPressureEnabled(true)

	if errors.Is(r.err, errCheckTimeout) {
		m.timedOut = true
		appstats.OnDiskCheckError()
		log.WithFields(log.Fields{
			"path":    m.cfg.Path,
			"timeout": m.checkTimeout().String(),
		}).Warn("Disk pressure first check timed out, starting disengaged")
	} else {
		m.evaluate(r)
	}

	go m.run()
}

func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		close(m.stop)

		select {
		case <-m.done:
		case <-time.After(m.checkTimeout()):
			log.WithField("path", m.cfg.Path).Warn("Disk pressure check still running, stopping without it")
		}

		appstats.SetDiskPressureEnabled(false)
	})
}

func (m *Monitor) checkTimeout() time.Duration {
	return min(m.cfg.CheckInterval, maxCheckTimeout)
}

type statResult struct {
	sample Sample
	err    error
	late   bool
}

// timedStat abandons a check that outlives checkTimeout and leaves its
// result to run, so a mount slower than the timeout still yields samples.
// Until that check returns, timedStat fails without calling stat, so a
// hung mount holds one blocked goroutine rather than one per tick.
func (m *Monitor) timedStat() statResult {
	if m.hung != nil {
		return statResult{err: errCheckHung}
	}

	results := make(chan statResult, 1)

	go func() {
		sample, err := m.stat(m.cfg.Path)
		results <- statResult{sample: sample, err: err}
	}()

	timer := time.NewTimer(m.checkTimeout())
	defer timer.Stop()

	select {
	case r := <-results:
		return r
	case <-timer.C:
		m.hung = results

		return statResult{err: errCheckTimeout}
	}
}

func (m *Monitor) run() {
	defer close(m.done)

	ticker := time.NewTicker(m.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case r := <-m.hung:
			m.hung = nil
			r.late = true
			m.evaluate(r)
		case <-ticker.C:
			m.check()
		}
	}
}

func (m *Monitor) check() {
	m.evaluate(m.timedStat())
}

func (m *Monitor) evaluate(r statResult) {
	select {
	case <-m.stop:
		// Stop may have stopped waiting for this check; its late result
		// must not change the guard during shutdown.
		return
	default:
	}

	sample, err := r.sample, r.err

	if err == nil && sample.TotalBytes == 0 {
		err = errZeroTotal
	}

	if err != nil {
		// Its timeout already counted it.
		if !r.late {
			appstats.OnDiskCheckError()
		}

		m.onCheckError(err)

		return
	}

	// A late sample still drives the guard, but checks have not recovered
	// until one finishes within the timeout again.
	if !r.late && (m.failing || m.stalled || m.timedOut) {
		m.failing, m.stalled, m.timedOut = false, false, false
		log.WithField("path", m.cfg.Path).Info("Disk pressure check recovered")
	}

	appstats.OnDiskCheck(sample.FreeBytes, sample.TotalBytes)
	log.WithFields(log.Fields{
		"path":       m.cfg.Path,
		"freeBytes":  sample.FreeBytes,
		"totalBytes": sample.TotalBytes,
	}).Trace("Disk pressure check")

	if m.Active() {
		if m.releaseCleared(sample) {
			m.release(sample)
		}
	} else if tripped := m.engageTripped(sample); len(tripped) > 0 {
		m.engage(sample, tripped)
	}
}

// onCheckError keeps the current state (ie space, active, ...)
func (m *Monitor) onCheckError(err error) {
	logger := log.WithField("path", m.cfg.Path).WithError(err)

	logged := &m.failing
	if errors.Is(err, errCheckTimeout) {
		logged = &m.stalled
	}

	if *logged {
		logger.Debug("Disk pressure check failed again, keeping current state")

		return
	}

	*logged = true
	logger.WithField("active", m.Active()).Error("Disk pressure check failed, keeping current state")
}

// engageTripped returns the disk pressure activation criterions a disk
// observation sample trips as a slice of their config field names.
// An empty slice means not engaged.
func (m *Monitor) engageTripped(sample Sample) []string {
	var tripped []string

	if m.cfg.EngageFreeMiB > 0 && sample.FreeBytes < mibToBytes(m.cfg.EngageFreeMiB) {
		tripped = append(tripped, "engageFreeMiB")
	}

	if m.cfg.EngageFreePercent > 0 && sample.FreePercent() < float64(m.cfg.EngageFreePercent) {
		tripped = append(tripped, "engageFreePercent")
	}

	return tripped
}

// releaseCleared reports whether the sample clears every release level
// whose engage criterion is enabled.
func (m *Monitor) releaseCleared(sample Sample) bool {
	if m.cfg.EngageFreeMiB > 0 && sample.FreeBytes < mibToBytes(m.cfg.ReleaseFreeMiB) {
		return false
	}

	if m.cfg.EngageFreePercent > 0 && sample.FreePercent() < float64(m.cfg.ReleaseFreePercent) {
		return false
	}

	return true
}

func (m *Monitor) engage(sample Sample, tripped []string) {
	m.active.Store(true)
	m.engagedAt = time.Now()
	appstats.OnDiskPressureChange(true)
	m.notify(true, sample)

	log.WithFields(log.Fields{
		"path":         m.cfg.Path,
		"freeBytes":    sample.FreeBytes,
		"freePercent":  formatPercent(sample.FreePercent()),
		"tripped":      tripped,
		"videoQuality": m.cfg.VideoQuality,
		"sources":      m.cfg.Sources,
	}).Warnf("Disk pressure guard engaged, capping %s video of LiveKit captures to %s",
		strings.Join(m.cfg.Sources, ","), m.cfg.VideoQuality)
}

func (m *Monitor) release(sample Sample) {
	m.active.Store(false)
	underPressure := time.Since(m.engagedAt).Round(time.Second)
	appstats.OnDiskPressureChange(false)
	m.notify(false, sample)

	log.WithFields(log.Fields{
		"path":               m.cfg.Path,
		"freeBytes":          sample.FreeBytes,
		"freePercent":        formatPercent(sample.FreePercent()),
		"releaseFreeMiB":     m.cfg.ReleaseFreeMiB,
		"releaseFreePercent": m.cfg.ReleaseFreePercent,
		"underPressure":      underPressure.String(),
	}).Infof("Disk pressure guard released after %s, restoring LiveKit captures", underPressure)
}

func (m *Monitor) notify(active bool, sample Sample) {
	if m.onChange != nil {
		m.onChange(active, sample)
	}
}

func mibToBytes(mib int64) uint64 {
	return uint64(mib) * bytesPerMiB //nolint:gosec // Normalize rejects negative levels
}

func formatPercent(p float64) float64 {
	return math.Round(p*100) / 100
}
