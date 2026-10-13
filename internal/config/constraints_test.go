package config

import (
	"math"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/assert"
)

func validDiskPressure() DiskPressure {
	return (&Config{App: App{Name: "test"}}).GetDefaults().Constraints.DiskPressure
}

func issueFields(issues []Issue) []string {
	fields := make([]string, 0, len(issues))
	for _, issue := range issues {
		fields = append(fields, issue.Field)
	}

	return fields
}

func TestDiskPressureDefaults(t *testing.T) {
	d := validDiskPressure()

	assert.False(t, d.Enable)
	assert.Empty(t, d.Path)
	assert.Equal(t, 15*time.Second, d.CheckInterval)
	assert.Equal(t, int64(20480), d.EngageFreeMiB)
	assert.Equal(t, int64(25600), d.ReleaseFreeMiB)
	assert.Equal(t, 10, d.EngageFreePercent)
	assert.Equal(t, 15, d.ReleaseFreePercent)
	assert.Equal(t, "LOW", d.VideoQuality)
	assert.Equal(t, []string{"camera"}, d.Sources)

	d.Enable = true
	issues := d.Normalize("/var/lib/bbb-webrtc-recorder")

	assert.Empty(t, issues)
	assert.True(t, d.Enable)
	assert.Equal(t, "/var/lib/bbb-webrtc-recorder", d.Path)
	assert.Equal(t, livekit.VideoQuality_LOW, d.Quality())
	assert.Equal(t, []livekit.TrackSource{livekit.TrackSource_CAMERA}, d.TrackSources())
}

func TestDiskPressureNormalize(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(d *DiskPressure)
		fields []string
		severe bool
		check  func(t *testing.T, d DiskPressure)
	}{
		{
			name:   "explicit path is kept",
			mutate: func(d *DiskPressure) { d.Path = "/srv/raw" },
			check:  func(t *testing.T, d DiskPressure) { assert.Equal(t, "/srv/raw", d.Path) },
		},
		{
			name:   "short interval is clamped",
			mutate: func(d *DiskPressure) { d.CheckInterval = 10 * time.Millisecond },
			fields: []string{"checkInterval"},
			check:  func(t *testing.T, d DiskPressure) { assert.Equal(t, time.Second, d.CheckInterval) },
		},
		{
			name: "percents are clamped",
			mutate: func(d *DiskPressure) {
				d.EngageFreePercent = 150
				d.ReleaseFreePercent = 200
			},
			fields: []string{"engageFreePercent", "releaseFreePercent"},
			check: func(t *testing.T, d DiskPressure) {
				assert.Equal(t, 100, d.EngageFreePercent)
				assert.Equal(t, 100, d.ReleaseFreePercent)
			},
		},
		{
			name:   "negative percent disables the criterion",
			mutate: func(d *DiskPressure) { d.EngageFreePercent = -5 },
			fields: []string{"engageFreePercent"},
			check: func(t *testing.T, d DiskPressure) {
				assert.Equal(t, 0, d.EngageFreePercent)
				assert.Equal(t, 0, d.ReleaseFreePercent)
			},
		},
		{
			name: "release below engage is raised",
			mutate: func(d *DiskPressure) {
				d.ReleaseFreeMiB = 100
				d.ReleaseFreePercent = 5
			},
			fields: []string{"releaseFreeMiB", "releaseFreePercent"},
			check: func(t *testing.T, d DiskPressure) {
				assert.Equal(t, int64(20480), d.ReleaseFreeMiB)
				assert.Equal(t, 10, d.ReleaseFreePercent)
			},
		},
		{
			name:   "release of a disabled MiB criterion is zeroed",
			mutate: func(d *DiskPressure) { d.EngageFreeMiB = 0 },
			check: func(t *testing.T, d DiskPressure) {
				assert.True(t, d.Enable)
				assert.Equal(t, int64(0), d.ReleaseFreeMiB)
				assert.Equal(t, 15, d.ReleaseFreePercent)
			},
		},
		{
			name:   "release of a disabled percent criterion is zeroed",
			mutate: func(d *DiskPressure) { d.EngageFreePercent = 0 },
			check: func(t *testing.T, d DiskPressure) {
				assert.True(t, d.Enable)
				assert.Equal(t, 0, d.ReleaseFreePercent)
				assert.Equal(t, int64(25600), d.ReleaseFreeMiB)
			},
		},
		{
			name: "huge MiB levels are clamped",
			mutate: func(d *DiskPressure) {
				d.EngageFreeMiB = math.MaxInt64
				d.ReleaseFreeMiB = math.MaxInt64
			},
			fields: []string{"engageFreeMiB", "releaseFreeMiB"},
			check: func(t *testing.T, d DiskPressure) {
				assert.Equal(t, int64(maxDiskPressureMiB), d.EngageFreeMiB)
				assert.Equal(t, int64(maxDiskPressureMiB), d.ReleaseFreeMiB)
			},
		},
		{
			name:   "quality name is case-insensitive",
			mutate: func(d *DiskPressure) { d.VideoQuality = " medium " },
			check: func(t *testing.T, d DiskPressure) {
				assert.Equal(t, "MEDIUM", d.VideoQuality)
				assert.Equal(t, livekit.VideoQuality_MEDIUM, d.Quality())
			},
		},
		{
			name:   "unknown quality falls back to LOW",
			mutate: func(d *DiskPressure) { d.VideoQuality = "OFF" },
			fields: []string{"videoQuality"},
			severe: true,
			check:  func(t *testing.T, d DiskPressure) { assert.Equal(t, livekit.VideoQuality_LOW, d.Quality()) },
		},
		{
			name:   "unknown and duplicate sources are dropped",
			mutate: func(d *DiskPressure) { d.Sources = []string{"Camera", "microphone", "screen_share", "camera"} },
			fields: []string{"sources"},
			check: func(t *testing.T, d DiskPressure) {
				assert.Equal(t, []string{"camera", "screen_share"}, d.Sources)
				assert.Equal(t,
					[]livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE},
					d.TrackSources())
			},
		},
		{
			name: "no engage criterion disables the guard",
			mutate: func(d *DiskPressure) {
				d.EngageFreeMiB = 0
				d.EngageFreePercent = 0
			},
			fields: []string{"enable"},
			severe: true,
			check:  func(t *testing.T, d DiskPressure) { assert.False(t, d.Enable) },
		},
		{
			name:   "no valid source disables the guard",
			mutate: func(d *DiskPressure) { d.Sources = []string{"microphone"} },
			fields: []string{"sources", "enable"},
			severe: true,
			check:  func(t *testing.T, d DiskPressure) { assert.False(t, d.Enable) },
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDiskPressure()
			d.Enable = true
			tc.mutate(&d)

			issues := d.Normalize("/rec")

			if tc.fields == nil {
				assert.Empty(t, issues)
			} else {
				assert.Equal(t, tc.fields, issueFields(issues))
			}

			severe := false
			for _, issue := range issues {
				severe = severe || issue.Severe
			}

			assert.Equal(t, tc.severe, severe)
			tc.check(t, d)
		})
	}
}

func TestDiskPressureEnvOverride(t *testing.T) {
	t.Setenv("BBBRECORDER_CONSTRAINTS_DISKPRESSURE_ENABLE", "true")
	t.Setenv("BBBRECORDER_CONSTRAINTS_DISKPRESSURE_ENGAGEFREEMIB", "1024")
	t.Setenv("BBBRECORDER_CONSTRAINTS_DISKPRESSURE_CHECKINTERVAL", "30s")
	t.Setenv("BBBRECORDER_CONSTRAINTS_DISKPRESSURE_VIDEOQUALITY", "medium")
	t.Setenv("BBBRECORDER_CONSTRAINTS_DISKPRESSURE_SOURCES", "camera,screen_share")

	app := App{Name: "bbb-webrtc-recorder-env-test"}
	cfg := (&Config{App: app}).GetDefaults()
	cfg.Load(app, "")
	d := cfg.Constraints.DiskPressure

	assert.True(t, d.Enable)
	assert.Equal(t, int64(1024), d.EngageFreeMiB)
	assert.Equal(t, 30*time.Second, d.CheckInterval)
	assert.Equal(t, "medium", d.VideoQuality)
	assert.Equal(t, []string{"camera", "screen_share"}, d.Sources)
	assert.Equal(t, int64(25600), d.ReleaseFreeMiB)
}
