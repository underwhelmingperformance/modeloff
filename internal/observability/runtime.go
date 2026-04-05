package observability

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Runtime owns the local OTel providers used by the TUI.
type Runtime struct {
	logger         *slog.Logger
	logBuffer      *LogBuffer
	reader         *sdkmetric.ManualReader
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *sdklog.LoggerProvider
}

// NewRuntime initialises local OpenTelemetry providers and sinks.
func NewRuntime() (*Runtime, error) {
	res := resource.NewSchemaless(
		attribute.String("service.name", "modeloff"),
		attribute.String("service.version", buildVersion()),
	)

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithCardinalityLimit(256),
		sdkmetric.WithView(durationView(MetricOperationDurationMs), durationView(MetricRequestDurationMs)),
	)

	meter := meterProvider.Meter("github.com/laney/modeloff/internal/observability")
	instruments, err := newMetricInstruments(meter)
	if err != nil {
		return nil, err
	}

	logBuffer := NewLogBuffer(1000)
	panelExporter := NewPanelExporter(logBuffer.Ingest(), instruments.droppedLogs)
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(panelExporter)),
	)

	rootSampler := sdktrace.ParentBased(sdktrace.AlwaysSample())
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysRecord(rootSampler)),
		sdktrace.WithSpanProcessor(NewSpanMetricsProcessor(instruments)),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otellogglobal.SetLoggerProvider(loggerProvider)

	logger := slog.New(otelslog.NewHandler(
		"github.com/laney/modeloff",
		otelslog.WithLoggerProvider(loggerProvider),
	))
	slog.SetDefault(logger)

	return &Runtime{
		logger:         logger,
		logBuffer:      logBuffer,
		reader:         reader,
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		loggerProvider: loggerProvider,
	}, nil
}

// Logger returns the default structured logger backed by OTel.
func (r *Runtime) Logger() *slog.Logger {
	return r.logger
}

// LogBuffer returns the in-memory log history.
func (r *Runtime) LogBuffer() *LogBuffer {
	return r.logBuffer
}

// SnapshotMetrics collects a UI snapshot from the OTel manual reader.
func (r *Runtime) SnapshotMetrics(ctx context.Context) (MetricsSnapshot, error) {
	return SnapshotMetrics(ctx, r.reader)
}

// Shutdown flushes providers and stops the local sinks.
func (r *Runtime) Shutdown(ctx context.Context) error {
	var errs []error

	if r.loggerProvider != nil {
		errs = append(errs, r.loggerProvider.Shutdown(ctx))
	}

	if r.tracerProvider != nil {
		errs = append(errs, r.tracerProvider.Shutdown(ctx))
	}

	if r.meterProvider != nil {
		errs = append(errs, r.meterProvider.Shutdown(ctx))
	}

	if r.logBuffer != nil {
		r.logBuffer.Close()
	}

	return errors.Join(errs...)
}

func durationView(name string) sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: name, Kind: sdkmetric.InstrumentKindHistogram},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
				MaxSize:  160,
				MaxScale: 10,
			},
		},
	)
}

func newMetricInstruments(meter metric.Meter) (metricInstruments, error) {
	var instruments metricInstruments
	var err error

	if instruments.operationCalls, err = meter.Int64Counter(MetricOperationCalls); err != nil {
		return metricInstruments{}, err
	}

	if instruments.llmRequests, err = meter.Int64Counter(MetricLLMRequests); err != nil {
		return metricInstruments{}, err
	}

	if instruments.promptTokens, err = meter.Int64Counter(MetricPromptTokens); err != nil {
		return metricInstruments{}, err
	}

	if instruments.completionTokens, err = meter.Int64Counter(MetricCompletionTokens); err != nil {
		return metricInstruments{}, err
	}

	if instruments.reasoningTokens, err = meter.Int64Counter(MetricReasoningTokens); err != nil {
		return metricInstruments{}, err
	}

	if instruments.cachedTokens, err = meter.Int64Counter(MetricCachedTokens); err != nil {
		return metricInstruments{}, err
	}

	if instruments.cacheWriteTokens, err = meter.Int64Counter(MetricCacheWriteTokens); err != nil {
		return metricInstruments{}, err
	}

	if instruments.costCredits, err = meter.Float64Counter(MetricCostCredits); err != nil {
		return metricInstruments{}, err
	}

	if instruments.operationDuration, err = meter.Float64Histogram(MetricOperationDurationMs); err != nil {
		return metricInstruments{}, err
	}

	if instruments.requestDuration, err = meter.Float64Histogram(MetricRequestDurationMs); err != nil {
		return metricInstruments{}, err
	}

	if instruments.droppedLogs, err = meter.Int64Counter(MetricDroppedLogs); err != nil {
		return metricInstruments{}, err
	}

	return instruments, nil
}

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}

	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return "dev"
}
