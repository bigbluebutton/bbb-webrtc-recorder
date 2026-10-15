//go:build e2e && linux

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Sample is one series of a Prometheus text exposition.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Metrics is a scrape, one Sample per series.
type Metrics []Sample

// ScrapeMetrics fetches and parses a /metrics endpoint.
func ScrapeMetrics(ctx context.Context, url string) (Metrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	return ParseMetrics(resp.Body)
}

// ParseMetrics parses the Prometheus text format, skipping comments.
func ParseMetrics(r io.Reader) (Metrics, error) {
	var out Metrics

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}

		s, err := parseSample(line)
		if err != nil {
			return nil, fmt.Errorf("metrics line %q: %w", line, err)
		}

		out = append(out, s)
	}

	return out, sc.Err()
}

func parseSample(line string) (Sample, error) {
	s := Sample{Labels: map[string]string{}}

	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return s, fmt.Errorf("no value")
	}

	s.Name, line = line[:i], line[i:]

	if line[0] == '{' {
		line = line[1:]

		for {
			line = strings.TrimLeft(line, " ,")
			if strings.HasPrefix(line, "}") {
				line = line[1:]
				break
			}

			eq := strings.Index(line, "=\"")
			if eq < 0 {
				return s, fmt.Errorf("bad label")
			}

			key := line[:eq]
			line = line[eq+2:]

			var val strings.Builder

			for {
				if line == "" {
					return s, fmt.Errorf("unterminated label value")
				}

				c := line[0]
				line = line[1:]

				if c == '"' {
					break
				}

				if c == '\\' && line != "" {
					switch line[0] {
					case 'n':
						val.WriteByte('\n')
					default:
						val.WriteByte(line[0])
					}

					line = line[1:]

					continue
				}

				val.WriteByte(c)
			}

			s.Labels[key] = val.String()
		}
	}

	fields := strings.Fields(line)
	if len(fields) == 0 {
		return s, fmt.Errorf("no value")
	}

	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, err
	}

	s.Value = v

	return s, nil
}

// Sum adds the values of name's series whose labels include match.
func (m Metrics) Sum(name string, match map[string]string) float64 {
	var sum float64

	for _, s := range m {
		if s.Name == name && labelsInclude(s.Labels, match) {
			sum += s.Value
		}
	}

	return sum
}

// Delta returns m minus base, series by series; a series base lacks counts
// from zero.
func (m Metrics) Delta(base Metrics) Metrics {
	prev := make(map[string]float64, len(base))
	for _, s := range base {
		prev[seriesKey(s)] = s.Value
	}

	out := make(Metrics, 0, len(m))
	for _, s := range m {
		d := s
		d.Value -= prev[seriesKey(s)]
		out = append(out, d)
	}

	return out
}

func labelsInclude(labels, match map[string]string) bool {
	for k, v := range match {
		if labels[k] != v {
			return false
		}
	}

	return true
}

func seriesKey(s Sample) string {
	var b strings.Builder

	b.WriteString(s.Name)

	for _, k := range slices.Sorted(maps.Keys(s.Labels)) {
		fmt.Fprintf(&b, "|%s=%s", k, s.Labels[k])
	}

	return b.String()
}
