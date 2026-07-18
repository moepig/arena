package telemetry

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Unit is a CloudWatch metric unit.
type Unit string

// Units used by the arena metric set.
const (
	UnitCount        Unit = "Count"
	UnitMilliseconds Unit = "Milliseconds"
)

// Datum is one metric value in an EMF document.
type Datum struct {
	Name  string
	Unit  Unit
	Value float64
}

// Emitter writes CloudWatch Embedded Metric Format documents, one JSON line
// per Emit, to a writer (stdout → awslogs → CloudWatch metric extraction).
// EMF avoids PutMetricData API-call billing. An attached PromExporter
// observes the same datums in parallel.
type Emitter struct {
	mu   sync.Mutex
	w    io.Writer
	now  func() time.Time
	prom *PromExporter
}

// NewEmitter returns an Emitter writing to w (typically os.Stdout).
func NewEmitter(w io.Writer) *Emitter {
	return &Emitter{w: w, now: time.Now}
}

// WithProm attaches a Prometheus exporter that mirrors every Emit.
func (e *Emitter) WithProm(p *PromExporter) *Emitter {
	e.prom = p
	return e
}

// Emit writes one EMF document: the metric values plus the dimension values
// as top-level fields, declared for extraction under the given namespace.
func (e *Emitter) Emit(namespace string, dimensions map[string]string, data ...Datum) {
	if e == nil || len(data) == 0 {
		return
	}
	e.prom.observe(namespace, dimensions, data...)

	dimNames := make([]string, 0, len(dimensions))
	doc := make(map[string]any, len(dimensions)+len(data)+1)
	for k, v := range dimensions {
		dimNames = append(dimNames, k)
		doc[k] = v
	}
	metrics := make([]map[string]any, 0, len(data))
	for _, d := range data {
		m := map[string]any{"Name": d.Name}
		if d.Unit != "" {
			m["Unit"] = string(d.Unit)
		}
		metrics = append(metrics, m)
		doc[d.Name] = d.Value
	}
	doc["_aws"] = map[string]any{
		"Timestamp": e.now().UnixMilli(),
		"CloudWatchMetrics": []map[string]any{{
			"Namespace":  namespace,
			"Dimensions": [][]string{dimNames},
			"Metrics":    metrics,
		}},
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return // metric loss is acceptable; correctness never depends on EMF
	}
	b = append(b, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.w.Write(b)
}
