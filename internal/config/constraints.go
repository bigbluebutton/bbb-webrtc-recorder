package config

import (
	"slices"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
)

const (
	minDiskPressureCheckInterval = time.Second
	// maxDiskPressureMiB: clamping value for bad configs (1 EiB, ludicrous but
	// within uint64)
	maxDiskPressureMiB = 1 << 40
)

type Constraints struct {
	DiskPressure DiskPressure `yaml:"diskPressure"`
}

// DiskPressure describes the mechanism that caps the video quality of
// LiveKit captures while the recordings filesystem is low on space. A
// criterion set to 0 is disabled.
type DiskPressure struct {
	Enable             bool          `yaml:"enable"`
	Path               string        `yaml:"path"`
	CheckInterval      time.Duration `yaml:"checkInterval"`
	EngageFreeMiB      int64         `yaml:"engageFreeMiB"`
	ReleaseFreeMiB     int64         `yaml:"releaseFreeMiB"`
	EngageFreePercent  int           `yaml:"engageFreePercent"`
	ReleaseFreePercent int           `yaml:"releaseFreePercent"`
	VideoQuality       string        `yaml:"videoQuality"`
	Sources            []string      `yaml:"sources"`
}

// Issue is a configured value that Normalize replaced. Severe
// issues disable a feature or ignore an operator's choice.
type Issue struct {
	Field   string
	Given   any
	Used    any
	Severe  bool
	Message string
}

// Normalize replaces invalid constraints values with safe ones.
// A misconfigured guard is only disabled and not fatal.
func (d *DiskPressure) Normalize(recorderDir string) []Issue {
	var issues []Issue

	fix := func(field string, given, used any, severe bool, msg string) {
		issues = append(issues, Issue{Field: field, Given: given, Used: used, Severe: severe, Message: msg})
	}

	if d.Path == "" {
		d.Path = recorderDir
	}

	if d.CheckInterval < minDiskPressureCheckInterval {
		fix("checkInterval", d.CheckInterval.String(), minDiskPressureCheckInterval.String(), false,
			"Check interval too short, clamped")
		d.CheckInterval = minDiskPressureCheckInterval
	}

	d.EngageFreeMiB = clampMiB("engageFreeMiB", d.EngageFreeMiB, fix)
	d.ReleaseFreeMiB = clampMiB("releaseFreeMiB", d.ReleaseFreeMiB, fix)
	d.EngageFreePercent = clampPercent("engageFreePercent", d.EngageFreePercent, fix)
	d.ReleaseFreePercent = clampPercent("releaseFreePercent", d.ReleaseFreePercent, fix)

	if d.EngageFreeMiB == 0 {
		d.ReleaseFreeMiB = 0
	}

	if d.EngageFreePercent == 0 {
		d.ReleaseFreePercent = 0
	}

	if d.EngageFreeMiB > 0 && d.ReleaseFreeMiB < d.EngageFreeMiB {
		fix("releaseFreeMiB", d.ReleaseFreeMiB, d.EngageFreeMiB, false,
			"Release level below engage level, raised to the engage level")
		d.ReleaseFreeMiB = d.EngageFreeMiB
	}

	if d.EngageFreePercent > 0 && d.ReleaseFreePercent < d.EngageFreePercent {
		fix("releaseFreePercent", d.ReleaseFreePercent, d.EngageFreePercent, false,
			"Release level below engage level, raised to the engage level")
		d.ReleaseFreePercent = d.EngageFreePercent
	}

	quality, ok := livekit.VideoQuality_value[strings.ToUpper(strings.TrimSpace(d.VideoQuality))]
	if !ok || livekit.VideoQuality(quality) == livekit.VideoQuality_OFF {
		fix("videoQuality", d.VideoQuality, livekit.VideoQuality_LOW.String(), true,
			"Unknown video quality, using LOW")
		quality = int32(livekit.VideoQuality_LOW)
	}

	d.VideoQuality = livekit.VideoQuality(quality).String()
	d.Sources = normalizeSources(d.Sources, fix)

	if d.EngageFreeMiB == 0 && d.EngageFreePercent == 0 {
		fix("enable", true, false, true, "No engage criterion set, disk pressure guard disabled")
		d.Enable = false
	}

	if len(d.Sources) == 0 {
		fix("enable", true, false, true, "No valid source set, disk pressure guard disabled")
		d.Enable = false
	}

	return issues
}

// Quality returns the parsed VideoQuality; call it after Normalize.
func (d *DiskPressure) Quality() livekit.VideoQuality {
	return livekit.VideoQuality(livekit.VideoQuality_value[strings.ToUpper(d.VideoQuality)])
}

// TrackSources returns the parsed Sources; call it after Normalize.
func (d *DiskPressure) TrackSources() []livekit.TrackSource {
	sources := make([]livekit.TrackSource, 0, len(d.Sources))

	for _, name := range d.Sources {
		sources = append(sources, livekit.TrackSource(livekit.TrackSource_value[strings.ToUpper(name)]))
	}

	return sources
}

type issueFunc func(field string, given, used any, severe bool, msg string)

func clampMiB(field string, value int64, fix issueFunc) int64 {
	switch {
	case value < 0:
		fix(field, value, 0, false, "Negative level, criterion disabled")

		return 0
	case value > maxDiskPressureMiB:
		fix(field, value, maxDiskPressureMiB, false, "Level above maximum, clamped")

		return maxDiskPressureMiB
	default:
		return value
	}
}

func clampPercent(field string, value int, fix issueFunc) int {
	switch {
	case value < 0:
		fix(field, value, 0, false, "Percent out of range, clamped")

		return 0
	case value > 100:
		fix(field, value, 100, false, "Percent out of range, clamped")

		return 100
	default:
		return value
	}
}

func normalizeSources(names []string, fix issueFunc) []string {
	valid := []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE}
	sources := make([]string, 0, len(names))

	for _, name := range names {
		value, ok := livekit.TrackSource_value[strings.ToUpper(strings.TrimSpace(name))]
		if !ok || !slices.Contains(valid, livekit.TrackSource(value)) {
			fix("sources", name, nil, false, "Not a LiveKit video source, dropped")

			continue
		}

		canonical := strings.ToLower(livekit.TrackSource(value).String())
		if !slices.Contains(sources, canonical) {
			sources = append(sources, canonical)
		}
	}

	return sources
}
