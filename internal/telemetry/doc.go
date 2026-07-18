// Package telemetry provides CloudWatch EMF metrics emission (one JSON
// document per data point on stdout, extracted by CloudWatch from the log
// stream, avoiding PutMetricData API-call billing) and OpenTelemetry
// tracing (OTLP/gRPC to a collector sidecar).
package telemetry
