package daemon

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// initTracer wires an OTLP/HTTP trace exporter + a resource from the OTEL_*
// environment (OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES,
// OTEL_EXPORTER_OTLP_ENDPOINT). LabOps observability — see the monitoring repo
// docs/otel-conventions.md. Safe when unconfigured: with no endpoint the
// exporter never delivers and the daemon is unaffected. Returns a shutdown func.
func initTracer(ctx context.Context) (func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}
