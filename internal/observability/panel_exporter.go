package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// PanelField is a compact attribute shown in the logging panel.
type PanelField struct {
	Key   string
	Value string
}

// PanelEntry is a rendered log record for the TUI.
type PanelEntry struct {
	Timestamp time.Time
	Level     string
	Message   string
	Scope     string
	TraceID   string
	SpanID    string
	Fields    []PanelField
}

// PanelExporter feeds log records from the OTel log pipeline into the
// local log buffer.
type PanelExporter struct {
	ingest      chan<- PanelEntry
	droppedLogs metric.Int64Counter
}

// NewPanelExporter creates an exporter that forwards records into ingest.
func NewPanelExporter(ingest chan<- PanelEntry, counter metric.Int64Counter) *PanelExporter {
	return &PanelExporter{
		ingest:      ingest,
		droppedLogs: counter,
	}
}

// Export forwards records into the in-memory log buffer without blocking.
func (e *PanelExporter) Export(ctx context.Context, records []sdklog.Record) error {
	for _, record := range records {
		entry := panelEntryFromRecord(record)

		select {
		case e.ingest <- entry:
		default:
			if e.droppedLogs != nil {
				e.droppedLogs.Add(ctx, 1)
			}
		}
	}

	return nil
}

// Shutdown performs no work for the in-memory exporter.
func (*PanelExporter) Shutdown(context.Context) error {
	return nil
}

// ForceFlush performs no work for the in-memory exporter.
func (*PanelExporter) ForceFlush(context.Context) error {
	return nil
}

func panelEntryFromRecord(record sdklog.Record) PanelEntry {
	entry := PanelEntry{
		Timestamp: record.Timestamp(),
		Level:     record.SeverityText(),
		Message:   valueString(record.Body()),
		Scope:     record.InstrumentationScope().Name,
		TraceID:   record.TraceID().String(),
		SpanID:    record.SpanID().String(),
	}

	if entry.Timestamp.IsZero() {
		entry.Timestamp = record.ObservedTimestamp()
	}

	if entry.Level == "" {
		entry.Level = fmt.Sprint(record.Severity())
	}

	record.WalkAttributes(func(kv log.KeyValue) bool {
		entry.Fields = append(entry.Fields, PanelField{
			Key:   string(kv.Key),
			Value: valueString(kv.Value),
		})

		return true
	})

	return entry
}

func valueString(v log.Value) string {
	switch v.Kind() {
	case log.KindBool:
		return fmt.Sprint(v.AsBool())
	case log.KindFloat64:
		return fmt.Sprint(v.AsFloat64())
	case log.KindInt64:
		return fmt.Sprint(v.AsInt64())
	case log.KindString:
		return v.AsString()
	case log.KindBytes:
		return fmt.Sprint(v.AsBytes())
	case log.KindSlice:
		return fmt.Sprint(v.AsSlice())
	case log.KindMap:
		return fmt.Sprint(v.AsMap())
	default:
		return ""
	}
}
