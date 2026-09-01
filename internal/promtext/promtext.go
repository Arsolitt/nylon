// Package promtext implements a minimal Prometheus text-format writer shared
// by the daemon and nylon-lb /metrics endpoints.
package promtext

import (
	"fmt"
	"io"
	"sort"
	"strconv"
)

type Writer struct {
	w    io.Writer
	seen map[string]struct{}
}

// New returns a Writer that emits Prometheus text format to w.
func New(w io.Writer) *Writer {
	return &Writer{w: w, seen: make(map[string]struct{})}
}

// Metric writes one sample; HELP/TYPE lines are emitted once per name.
func (m *Writer) Metric(name, help, metricType string, labels map[string]string, value float64) {
	if _, ok := m.seen[name]; !ok {
		_, _ = fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metricType)
		m.seen[name] = struct{}{}
	}
	_, _ = io.WriteString(m.w, name)
	if len(labels) != 0 {
		keys := make([]string, 0, len(labels))
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		_, _ = io.WriteString(m.w, "{")
		for i, key := range keys {
			if i != 0 {
				_, _ = io.WriteString(m.w, ",")
			}
			_, _ = fmt.Fprintf(m.w, `%s=%s`, key, strconv.Quote(labels[key]))
		}
		_, _ = io.WriteString(m.w, "}")
	}
	_, _ = fmt.Fprintf(m.w, " %s\n", strconv.FormatFloat(value, 'f', -1, 64))
}
