package observability

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	helperInstrumentsMu     sync.Mutex
	helperInstrumentsKey    string
	helperMetricInstruments metricInstruments
)

// RecordMemoryToolCall records an executed memory tool call by kind and result.
func RecordMemoryToolCall(ctx context.Context, toolKind string, result string) {
	instruments, err := globalMetricInstruments()
	if err != nil {
		return
	}

	instruments.memoryToolCalls.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrMemoryToolKind, toolKind),
		attribute.String(AttrResult, result),
	))
}

// RecordMemorySearchResults records how many results a memory search returned.
func RecordMemorySearchResults(ctx context.Context, count int) {
	instruments, err := globalMetricInstruments()
	if err != nil {
		return
	}

	instruments.memorySearchResults.Record(ctx, int64(count))
}

// RecordMemorySearchTopScore records the top similarity score for a memory search.
func RecordMemorySearchTopScore(ctx context.Context, score float64) {
	instruments, err := globalMetricInstruments()
	if err != nil {
		return
	}

	instruments.memorySearchTopScore.Record(ctx, score)
}

func globalMetricInstruments() (metricInstruments, error) {
	meterProvider := otel.GetMeterProvider()
	cacheKey := meterProviderCacheKey(meterProvider)

	helperInstrumentsMu.Lock()
	defer helperInstrumentsMu.Unlock()

	if helperInstrumentsKey == cacheKey {
		return helperMetricInstruments, nil
	}

	instruments, err := newMetricInstruments(meterProvider.Meter("github.com/laney/modeloff/internal/observability"))
	if err != nil {
		return metricInstruments{}, err
	}

	helperInstrumentsKey = cacheKey
	helperMetricInstruments = instruments

	return helperMetricInstruments, nil
}

func meterProviderCacheKey(provider metric.MeterProvider) string {
	value := reflect.ValueOf(provider)
	if value.IsValid() && value.Kind() == reflect.Pointer {
		return fmt.Sprintf("%T:%#x", provider, value.Pointer())
	}

	return fmt.Sprintf("%T:%v", provider, provider)
}
