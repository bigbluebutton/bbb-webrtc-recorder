package appstats

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

// useFreshRegistry points the default registry at an empty one for the
// test, so RegisterMetrics can run once per test run.
func useFreshRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()

	registry := prometheus.NewRegistry()
	registerer, gatherer := prometheus.DefaultRegisterer, prometheus.DefaultGatherer
	prometheus.DefaultRegisterer, prometheus.DefaultGatherer = registry, registry

	t.Cleanup(func() {
		prometheus.DefaultRegisterer, prometheus.DefaultGatherer = registerer, gatherer
	})

	return registry
}

func TestRegisterMetricsExportsKnownConstraintSeries(t *testing.T) {
	registry := useFreshRegistry(t)
	RegisterMetrics()

	families, err := registry.Gather()
	assert.NoError(t, err)

	series := map[string]int{
		"recorder_constrained_captures_total":       0,
		"recorder_constrained_captures_active":      0,
		"recorder_capture_constraint_changes_total": 0,
		"recorder_disk_pressure_transitions_total":  0,
	}

	for _, family := range families {
		if _, ok := series[family.GetName()]; !ok {
			continue
		}

		for _, metric := range family.GetMetric() {
			value := metric.GetCounter().GetValue() + metric.GetGauge().GetValue()
			assert.InDelta(t, 0, value, 0, family.GetName())
			series[family.GetName()]++
		}
	}

	assert.Equal(t, 1, series["recorder_constrained_captures_total"])
	assert.Equal(t, 1, series["recorder_constrained_captures_active"])
	assert.Equal(t, 2, series["recorder_capture_constraint_changes_total"])
	assert.Equal(t, 2, series["recorder_disk_pressure_transitions_total"])
}

func TestDiskByteGaugesReadNaNUntilFirstCheck(t *testing.T) {
	registry := useFreshRegistry(t)
	RegisterMetrics()

	families, err := registry.Gather()
	assert.NoError(t, err)

	seen := 0

	for _, family := range families {
		switch family.GetName() {
		case "recorder_disk_free_bytes", "recorder_disk_total_bytes":
			seen++

			assert.True(t, math.IsNaN(family.GetMetric()[0].GetGauge().GetValue()), family.GetName())
		}
	}

	assert.Equal(t, 2, seen)
}
