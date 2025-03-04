// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package spanmetricsconnector

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tilinna/clock"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/connector/connectortest"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	conventions "go.opentelemetry.io/collector/semconv/v1.6.1"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/metadata"

	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/spanmetricsconnector/internal/cache"
	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/spanmetricsconnector/mocks"
)

const (
	stringAttrName         = "stringAttrName"
	intAttrName            = "intAttrName"
	doubleAttrName         = "doubleAttrName"
	boolAttrName           = "boolAttrName"
	nullAttrName           = "nullAttrName"
	mapAttrName            = "mapAttrName"
	arrayAttrName          = "arrayAttrName"
	notInSpanAttrName0     = "shouldBeInMetric"
	notInSpanAttrName1     = "shouldNotBeInMetric"
	regionResourceAttrName = "region"
	DimensionsCacheSize    = 2

	sampleRegion          = "us-east-1"
	sampleLatency         = float64(11)
	sampleLatencyDuration = time.Duration(sampleLatency) * time.Millisecond
)

// metricID represents the minimum attributes that uniquely identifies a metric in our tests.
type metricID struct {
	service    string
	name       string
	kind       string
	statusCode string
}

type metricDataPoint interface {
	Attributes() pcommon.Map
}

type serviceSpans struct {
	serviceName string
	spans       []span
}

type span struct {
	name       string
	kind       ptrace.SpanKind
	statusCode ptrace.StatusCode
}

// verifyConsumeMetricsInputCumulative expects one accumulation of metrics, and marked as cumulative
func verifyConsumeMetricsInputCumulative(t testing.TB, input pmetric.Metrics) bool {
	return verifyConsumeMetricsInput(t, input, pmetric.AggregationTemporalityCumulative, 1)
}

func verifyBadMetricsOkay(t testing.TB, input pmetric.Metrics) bool {
	return true // Validating no exception
}

// verifyConsumeMetricsInputDelta expects one accumulation of metrics, and marked as delta
func verifyConsumeMetricsInputDelta(t testing.TB, input pmetric.Metrics) bool {
	return verifyConsumeMetricsInput(t, input, pmetric.AggregationTemporalityDelta, 1)
}

// verifyMultipleCumulativeConsumptions expects the amount of accumulations as kept track of by numCumulativeConsumptions.
// numCumulativeConsumptions acts as a multiplier for the values, since the cumulative metrics are additive.
func verifyMultipleCumulativeConsumptions() func(t testing.TB, input pmetric.Metrics) bool {
	numCumulativeConsumptions := 0
	return func(t testing.TB, input pmetric.Metrics) bool {
		numCumulativeConsumptions++
		return verifyConsumeMetricsInput(t, input, pmetric.AggregationTemporalityCumulative, numCumulativeConsumptions)
	}
}

// verifyConsumeMetricsInput verifies the input of the ConsumeMetrics call from this connector.
// This is the best point to verify the computed metrics from spans are as expected.
func verifyConsumeMetricsInput(t testing.TB, input pmetric.Metrics, expectedTemporality pmetric.AggregationTemporality, numCumulativeConsumptions int) bool {
	require.Equal(t, 6, input.DataPointCount(),
		"Should be 3 for each of call count and latency. Each group of 3 data points is made of: "+
			"service-a (server kind) -> service-a (client kind) -> service-b (service kind)",
	)

	rm := input.ResourceMetrics()
	require.Equal(t, 1, rm.Len())

	ilm := rm.At(0).ScopeMetrics()
	require.Equal(t, 1, ilm.Len())
	assert.Equal(t, "spanmetricsconnector", ilm.At(0).Scope().Name())

	m := ilm.At(0).Metrics()
	require.Equal(t, 2, m.Len())

	seenMetricIDs := make(map[metricID]bool)
	// The first 3 data points are for call counts.
	assert.Equal(t, metricNameCalls, m.At(0).Name())
	assert.Equal(t, expectedTemporality, m.At(0).Sum().AggregationTemporality())
	assert.True(t, m.At(0).Sum().IsMonotonic())
	callsDps := m.At(0).Sum().DataPoints()
	require.Equal(t, 3, callsDps.Len())
	for dpi := 0; dpi < 3; dpi++ {
		dp := callsDps.At(dpi)
		assert.Equal(t, int64(numCumulativeConsumptions), dp.IntValue(), "There should only be one metric per Service/name/kind combination")
		assert.NotZero(t, dp.StartTimestamp(), "StartTimestamp should be set")
		assert.NotZero(t, dp.Timestamp(), "Timestamp should be set")
		verifyMetricLabels(dp, t, seenMetricIDs)
	}

	seenMetricIDs = make(map[metricID]bool)
	// The remaining 3 data points are for latency.
	assert.Equal(t, "latency", m.At(1).Name())
	assert.Equal(t, "ms", m.At(1).Unit())
	assert.Equal(t, expectedTemporality, m.At(1).Histogram().AggregationTemporality())
	latencyDps := m.At(1).Histogram().DataPoints()
	require.Equal(t, 3, latencyDps.Len())
	for dpi := 0; dpi < 3; dpi++ {
		dp := latencyDps.At(dpi)
		assert.Equal(t, sampleLatency*float64(numCumulativeConsumptions), dp.Sum(), "Should be a 11ms latency measurement, multiplied by the number of stateful accumulations.")
		assert.NotZero(t, dp.Timestamp(), "Timestamp should be set")

		// Verify bucket counts.

		// The bucket counts should be 1 greater than the explicit bounds as documented in:
		// https://github.com/open-telemetry/opentelemetry-proto/blob/main/opentelemetry/proto/metrics/v1/metrics.proto.
		assert.Equal(t, dp.ExplicitBounds().Len()+1, dp.BucketCounts().Len())

		// Find the bucket index where the 11ms latency should belong in.
		var foundLatencyIndex int
		for foundLatencyIndex = 0; foundLatencyIndex < dp.ExplicitBounds().Len(); foundLatencyIndex++ {
			if dp.ExplicitBounds().At(foundLatencyIndex) > sampleLatency {
				break
			}
		}

		// Then verify that all histogram buckets are empty except for the bucket with the 11ms latency.
		var wantBucketCount uint64
		for bi := 0; bi < dp.BucketCounts().Len(); bi++ {
			wantBucketCount = 0
			if bi == foundLatencyIndex {
				wantBucketCount = uint64(numCumulativeConsumptions)
			}
			assert.Equal(t, wantBucketCount, dp.BucketCounts().At(bi))
		}
		verifyMetricLabels(dp, t, seenMetricIDs)
	}
	return true
}

func verifyMetricLabels(dp metricDataPoint, t testing.TB, seenMetricIDs map[metricID]bool) {
	mID := metricID{}
	wantDimensions := map[string]pcommon.Value{
		stringAttrName:         pcommon.NewValueStr("stringAttrValue"),
		intAttrName:            pcommon.NewValueInt(99),
		doubleAttrName:         pcommon.NewValueDouble(99.99),
		boolAttrName:           pcommon.NewValueBool(true),
		nullAttrName:           pcommon.NewValueEmpty(),
		arrayAttrName:          pcommon.NewValueSlice(),
		mapAttrName:            pcommon.NewValueMap(),
		notInSpanAttrName0:     pcommon.NewValueStr("defaultNotInSpanAttrVal"),
		regionResourceAttrName: pcommon.NewValueStr(sampleRegion),
	}
	dp.Attributes().Range(func(k string, v pcommon.Value) bool {
		switch k {
		case serviceNameKey:
			mID.service = v.Str()
		case spanNameKey:
			mID.name = v.Str()
		case spanKindKey:
			mID.kind = v.Str()
		case statusCodeKey:
			mID.statusCode = v.Str()
		case notInSpanAttrName1:
			assert.Fail(t, notInSpanAttrName1+" should not be in this metric")
		default:
			assert.Equal(t, wantDimensions[k], v)
			delete(wantDimensions, k)
		}
		return true
	})
	assert.Empty(t, wantDimensions, "Did not see all expected dimensions in metric. Missing: ", wantDimensions)

	// Service/name/kind should be a unique metric.
	assert.False(t, seenMetricIDs[mID])
	seenMetricIDs[mID] = true
}

func buildBadSampleTrace() ptrace.Traces {
	badTrace := buildSampleTrace()
	span := badTrace.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	now := time.Now()
	// Flipping timestamp for a bad duration
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now.Add(sampleLatencyDuration)))
	return badTrace
}

// buildSampleTrace builds the following trace:
//
//	service-a/ping (server) ->
//	  service-a/ping (client) ->
//	    service-b/ping (server)
func buildSampleTrace() ptrace.Traces {
	traces := ptrace.NewTraces()

	initServiceSpans(
		serviceSpans{
			serviceName: "service-a",
			spans: []span{
				{
					name:       "/ping",
					kind:       ptrace.SpanKindServer,
					statusCode: ptrace.StatusCodeOk,
				},
				{
					name:       "/ping",
					kind:       ptrace.SpanKindClient,
					statusCode: ptrace.StatusCodeOk,
				},
			},
		}, traces.ResourceSpans().AppendEmpty())
	initServiceSpans(
		serviceSpans{
			serviceName: "service-b",
			spans: []span{
				{
					name:       "/ping",
					kind:       ptrace.SpanKindServer,
					statusCode: ptrace.StatusCodeError,
				},
			},
		}, traces.ResourceSpans().AppendEmpty())
	initServiceSpans(serviceSpans{}, traces.ResourceSpans().AppendEmpty())
	return traces
}

func initServiceSpans(serviceSpans serviceSpans, spans ptrace.ResourceSpans) {
	if serviceSpans.serviceName != "" {
		spans.Resource().Attributes().PutStr(conventions.AttributeServiceName, serviceSpans.serviceName)
	}

	spans.Resource().Attributes().PutStr(regionResourceAttrName, sampleRegion)

	ils := spans.ScopeSpans().AppendEmpty()
	for _, span := range serviceSpans.spans {
		initSpan(span, ils.Spans().AppendEmpty())
	}
}

func initSpan(span span, s ptrace.Span) {
	s.SetName(span.name)
	s.SetKind(span.kind)
	s.Status().SetCode(span.statusCode)
	now := time.Now()
	s.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	s.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(sampleLatencyDuration)))

	s.Attributes().PutStr(stringAttrName, "stringAttrValue")
	s.Attributes().PutInt(intAttrName, 99)
	s.Attributes().PutDouble(doubleAttrName, 99.99)
	s.Attributes().PutBool(boolAttrName, true)
	s.Attributes().PutEmpty(nullAttrName)
	s.Attributes().PutEmptyMap(mapAttrName)
	s.Attributes().PutEmptySlice(arrayAttrName)
	s.SetTraceID(pcommon.TraceID([16]byte{byte(42)}))
	s.SetSpanID(pcommon.SpanID([8]byte{byte(42)}))
}

func TestBuildKeySameServiceNameCharSequence(t *testing.T) {
	span0 := ptrace.NewSpan()
	span0.SetName("c")
	buf := &bytes.Buffer{}
	buildKey(buf, "ab", span0, nil, pcommon.NewMap())
	k0 := metricKey(buf.String())
	buf.Reset()
	span1 := ptrace.NewSpan()
	span1.SetName("bc")
	buildKey(buf, "a", span1, nil, pcommon.NewMap())
	k1 := metricKey(buf.String())
	assert.NotEqual(t, k0, k1)
	assert.Equal(t, metricKey("ab\u0000c\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET"), k0)
	assert.Equal(t, metricKey("a\u0000bc\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET"), k1)
}

func TestBuildKeyWithDimensions(t *testing.T) {
	defaultFoo := pcommon.NewValueStr("bar")
	for _, tc := range []struct {
		name            string
		optionalDims    []dimension
		resourceAttrMap map[string]interface{}
		spanAttrMap     map[string]interface{}
		wantKey         string
	}{
		{
			name:    "nil optionalDims",
			wantKey: "ab\u0000c\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET",
		},
		{
			name: "neither span nor resource contains key, dim provides default",
			optionalDims: []dimension{
				{name: "foo", value: &defaultFoo},
			},
			wantKey: "ab\u0000c\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET\u0000bar",
		},
		{
			name: "neither span nor resource contains key, dim provides no default",
			optionalDims: []dimension{
				{name: "foo"},
			},
			wantKey: "ab\u0000c\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET",
		},
		{
			name: "span attribute contains dimension",
			optionalDims: []dimension{
				{name: "foo"},
			},
			spanAttrMap: map[string]interface{}{
				"foo": 99,
			},
			wantKey: "ab\u0000c\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET\u000099",
		},
		{
			name: "resource attribute contains dimension",
			optionalDims: []dimension{
				{name: "foo"},
			},
			resourceAttrMap: map[string]interface{}{
				"foo": 99,
			},
			wantKey: "ab\u0000c\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET\u000099",
		},
		{
			name: "both span and resource attribute contains dimension, should prefer span attribute",
			optionalDims: []dimension{
				{name: "foo"},
			},
			spanAttrMap: map[string]interface{}{
				"foo": 100,
			},
			resourceAttrMap: map[string]interface{}{
				"foo": 99,
			},
			wantKey: "ab\u0000c\u0000SPAN_KIND_UNSPECIFIED\u0000STATUS_CODE_UNSET\u0000100",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resAttr := pcommon.NewMap()
			assert.NoError(t, resAttr.FromRaw(tc.resourceAttrMap))
			span0 := ptrace.NewSpan()
			assert.NoError(t, span0.Attributes().FromRaw(tc.spanAttrMap))
			span0.SetName("c")
			buf := &bytes.Buffer{}
			buildKey(buf, "ab", span0, tc.optionalDims, resAttr)
			assert.Equal(t, tc.wantKey, buf.String())
		})
	}
}

func TestConnectorUpdateExemplars(t *testing.T) {
	// ----- conditions -------------------------------------------------------
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	traces := buildSampleTrace()
	traceID := traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).TraceID()
	spanID := traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).SpanID()
	key := metricKey("metricKey")
	c, err := newConnector(zaptest.NewLogger(t), cfg, nil)
	c.metricsConsumer = new(consumertest.MetricsSink)
	value := float64(42)

	// ----- call -------------------------------------------------------------
	h := c.getOrCreateHistogram(key, pcommon.NewMap())
	h.observe(value, traceID, spanID)

	// ----- verify -----------------------------------------------------------
	assert.NoError(t, err)
	assert.NotEmpty(t, c.histograms[key].exemplars)

	want := pmetric.NewExemplar()
	want.SetTraceID(traceID)
	want.SetSpanID(spanID)
	want.SetDoubleValue(value)
	assert.Equal(t, want, c.histograms[key].exemplars.At(0))

	// ----- call -------------------------------------------------------------
	c.resetExemplars()

	// ----- verify -----------------------------------------------------------
	assert.NoError(t, err)
	assert.Equal(t, 0, c.histograms[key].exemplars.Len())
}

func TestStart(t *testing.T) {
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)

	createParams := connectortest.NewNopCreateSettings()
	conn, err := factory.CreateTracesToMetrics(context.Background(), createParams, cfg, consumertest.NewNop())
	require.NoError(t, err)

	smc := conn.(*connectorImp)
	ctx := context.Background()
	err = smc.Start(ctx, componenttest.NewNopHost())
	defer func() { sdErr := smc.Shutdown(ctx); require.NoError(t, sdErr) }()
	assert.NoError(t, err)
}

func TestConcurrentShutdown(t *testing.T) {
	// Prepare
	ctx := context.Background()
	core, observedLogs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	mockClock := clock.NewMock(time.Now())
	ticker := mockClock.NewTicker(time.Nanosecond)

	// Test
	p := newConnectorImp(new(consumertest.MetricsSink), nil, cumulative, logger, ticker)
	err := p.Start(ctx, componenttest.NewNopHost())
	require.NoError(t, err)

	// Allow goroutines time to start.
	time.Sleep(time.Millisecond)

	// Simulate many goroutines trying to concurrently shutdown.
	var wg sync.WaitGroup
	const concurrency = 1000
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			err := p.Shutdown(ctx)
			require.NoError(t, err)
			wg.Done()
		}()
	}
	wg.Wait()

	// Allow time for log observer to sync all logs emitted.
	// Even though the WaitGroup has been given the "done" signal, there's still a potential race condition
	// between the WaitGroup being unblocked and when the logs will be flushed.
	var allLogs []observer.LoggedEntry
	assert.Eventually(t, func() bool {
		allLogs = observedLogs.All()
		return len(allLogs) > 0
	}, time.Second, time.Millisecond*10)

	// Starting spanmetricsconnector...
	// Shutting down spanmetricsconnector...
	// Stopping ticker.
	assert.Len(t, allLogs, 3)
}

func TestConfigureLatencyBounds(t *testing.T) {
	// Prepare
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.LatencyHistogramBuckets = []time.Duration{
		3 * time.Nanosecond,
		3 * time.Microsecond,
		3 * time.Millisecond,
		3 * time.Second,
	}

	// Test
	c, err := newConnector(zaptest.NewLogger(t), cfg, nil)
	c.metricsConsumer = new(consumertest.MetricsSink)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, c)
	assert.Equal(t, []float64{0.000003, 0.003, 3, 3000}, c.latencyBounds)
}

func TestConnectorCapabilities(t *testing.T) {
	// Prepare
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)

	// Test
	c, err := newConnector(zaptest.NewLogger(t), cfg, nil)
	c.metricsConsumer = new(consumertest.MetricsSink)
	assert.NoError(t, err)
	caps := c.Capabilities()

	// Verify
	assert.NotNil(t, c)
	assert.Equal(t, false, caps.MutatesData)
}

func TestConsumeMetricsErrors(t *testing.T) {
	// Prepare
	fakeErr := fmt.Errorf("consume metrics error")

	core, observedLogs := observer.New(zapcore.ErrorLevel)
	logger := zap.New(core)

	var wg sync.WaitGroup
	mcon := &mocks.MetricsConsumer{}
	mcon.On("ConsumeMetrics", mock.Anything, mock.MatchedBy(func(input pmetric.Metrics) bool {
		wg.Done()
		return true
	})).Return(fakeErr)

	mockClock := clock.NewMock(time.Now())
	ticker := mockClock.NewTicker(time.Nanosecond)
	p := newConnectorImp(mcon, nil, cumulative, logger, ticker)

	ctx := metadata.NewIncomingContext(context.Background(), nil)
	err := p.Start(ctx, componenttest.NewNopHost())
	defer func() { sdErr := p.Shutdown(ctx); require.NoError(t, sdErr) }()
	require.NoError(t, err)

	traces := buildSampleTrace()

	// Test
	err = p.ConsumeTraces(ctx, traces)
	require.NoError(t, err)

	// Trigger flush.
	wg.Add(1)
	mockClock.Add(time.Nanosecond)
	wg.Wait()

	// Allow time for log observer to sync all logs emitted.
	// Even though the WaitGroup has been given the "done" signal, there's still a potential race condition
	// between the WaitGroup being unblocked and when the logs will be flushed.
	var allLogs []observer.LoggedEntry
	assert.Eventually(t, func() bool {
		allLogs = observedLogs.All()
		return len(allLogs) > 0
	}, time.Second, time.Millisecond*10)

	assert.Equal(t, "Failed ConsumeMetrics", allLogs[0].Message)
}

func TestConsumeTraces(t *testing.T) {
	t.Parallel()

	testcases := []struct {
		name                   string
		aggregationTemporality string
		verifier               func(t testing.TB, input pmetric.Metrics) bool
		traces                 []ptrace.Traces
	}{
		{
			name:                   "Test single consumption, three spans (Cumulative).",
			aggregationTemporality: cumulative,
			verifier:               verifyConsumeMetricsInputCumulative,
			traces:                 []ptrace.Traces{buildSampleTrace()},
		},
		{
			name:                   "Test single consumption, three spans (Delta).",
			aggregationTemporality: delta,
			verifier:               verifyConsumeMetricsInputDelta,
			traces:                 []ptrace.Traces{buildSampleTrace()},
		},
		{
			// More consumptions, should accumulate additively.
			name:                   "Test two consumptions (Cumulative).",
			aggregationTemporality: cumulative,
			verifier:               verifyMultipleCumulativeConsumptions(),
			traces:                 []ptrace.Traces{buildSampleTrace(), buildSampleTrace()},
		},
		{
			// More consumptions, should not accumulate. Therefore, end state should be the same as single consumption case.
			name:                   "Test two consumptions (Delta).",
			aggregationTemporality: delta,
			verifier:               verifyConsumeMetricsInputDelta,
			traces:                 []ptrace.Traces{buildSampleTrace(), buildSampleTrace()},
		},
		{
			// Consumptions with improper timestamps
			name:                   "Test bad consumptions (Delta).",
			aggregationTemporality: cumulative,
			verifier:               verifyBadMetricsOkay,
			traces:                 []ptrace.Traces{buildBadSampleTrace()},
		},
	}

	for _, tc := range testcases {
		// Since parallelism is enabled in these tests, to avoid flaky behavior,
		// instantiate a copy of the test case for t.Run's closure to use.
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Prepare

			mcon := &mocks.MetricsConsumer{}

			var wg sync.WaitGroup
			// Mocked metric exporter will perform validation on metrics, during p.ConsumeMetrics()
			mcon.On("ConsumeMetrics", mock.Anything, mock.MatchedBy(func(input pmetric.Metrics) bool {
				wg.Done()
				return tc.verifier(t, input)
			})).Return(nil)

			defaultNullValue := pcommon.NewValueStr("defaultNullValue")

			mockClock := clock.NewMock(time.Now())
			ticker := mockClock.NewTicker(time.Nanosecond)

			p := newConnectorImp(mcon, &defaultNullValue, tc.aggregationTemporality, zaptest.NewLogger(t), ticker)

			ctx := metadata.NewIncomingContext(context.Background(), nil)
			err := p.Start(ctx, componenttest.NewNopHost())
			defer func() { sdErr := p.Shutdown(ctx); require.NoError(t, sdErr) }()
			require.NoError(t, err)

			for _, traces := range tc.traces {
				// Test
				err = p.ConsumeTraces(ctx, traces)
				assert.NoError(t, err)

				// Trigger flush.
				wg.Add(1)
				mockClock.Add(time.Nanosecond)
				wg.Wait()
			}
		})
	}
}

func TestMetricKeyCache(t *testing.T) {
	mcon := &mocks.MetricsConsumer{}
	mcon.On("ConsumeMetrics", mock.Anything, mock.Anything).Return(nil)

	defaultNullValue := pcommon.NewValueStr("defaultNullValue")
	p := newConnectorImp(mcon, &defaultNullValue, cumulative, zaptest.NewLogger(t), nil)
	traces := buildSampleTrace()

	// Test
	ctx := metadata.NewIncomingContext(context.Background(), nil)

	// 0 key was cached at beginning
	assert.Zero(t, p.metricKeyToDimensions.Len())

	err := p.ConsumeTraces(ctx, traces)
	// Validate
	require.NoError(t, err)
	// 2 key was cached, 1 key was evicted and cleaned after the processing
	assert.Eventually(t, func() bool {
		return p.metricKeyToDimensions.Len() == DimensionsCacheSize
	}, 10*time.Second, time.Millisecond*100)

	// consume another batch of traces
	err = p.ConsumeTraces(ctx, traces)
	require.NoError(t, err)

	// 2 key was cached, other keys were evicted and cleaned after the processing
	assert.Eventually(t, func() bool {
		return p.metricKeyToDimensions.Len() == DimensionsCacheSize
	}, 10*time.Second, time.Millisecond*100)
}

func BenchmarkConnectorConsumeTraces(b *testing.B) {
	// Prepare
	mcon := &mocks.MetricsConsumer{}
	mcon.On("ConsumeMetrics", mock.Anything, mock.Anything).Return(nil)

	defaultNullValue := pcommon.NewValueStr("defaultNullValue")
	conn := newConnectorImp(mcon, &defaultNullValue, cumulative, zaptest.NewLogger(b), nil)

	traces := buildSampleTrace()

	// Test
	ctx := metadata.NewIncomingContext(context.Background(), nil)
	for n := 0; n < b.N; n++ {
		assert.NoError(b, conn.ConsumeTraces(ctx, traces))
	}
}

func newConnectorImp(mcon consumer.Metrics, defaultNullValue *pcommon.Value, temporality string, logger *zap.Logger, ticker *clock.Ticker) *connectorImp {
	defaultNotInSpanAttrVal := pcommon.NewValueStr("defaultNotInSpanAttrVal")
	// use size 2 for LRU cache for testing purpose
	metricKeyToDimensions, err := cache.NewCache[metricKey, pcommon.Map](DimensionsCacheSize)
	if err != nil {
		panic(err)
	}
	return &connectorImp{
		logger:          logger,
		config:          Config{AggregationTemporality: temporality},
		metricsConsumer: mcon,

		startTimestamp: pcommon.NewTimestampFromTime(time.Now()),
		histograms:     make(map[metricKey]*histogram),
		latencyBounds:  defaultLatencyHistogramBucketsMs,
		dimensions: []dimension{
			// Set nil defaults to force a lookup for the attribute in the span.
			{stringAttrName, nil},
			{intAttrName, nil},
			{doubleAttrName, nil},
			{boolAttrName, nil},
			{mapAttrName, nil},
			{arrayAttrName, nil},
			{nullAttrName, defaultNullValue},
			// Add a default value for an attribute that doesn't exist in a span
			{notInSpanAttrName0, &defaultNotInSpanAttrVal},
			// Leave the default value unset to test that this dimension should not be added to the metric.
			{notInSpanAttrName1, nil},
			// Add a resource attribute to test "process" attributes like IP, host, region, cluster, etc.
			{regionResourceAttrName, nil},
		},
		keyBuf:                new(bytes.Buffer),
		metricKeyToDimensions: metricKeyToDimensions,
		ticker:                ticker,
		done:                  make(chan struct{}),
	}
}

func TestConnectorConsumeTracesEvictedCacheKey(t *testing.T) {
	// Prepare
	traces0 := ptrace.NewTraces()

	initServiceSpans(
		serviceSpans{
			// This should be moved to the evicted list of the LRU cache once service-c is added
			// since the cache size is configured to 2.
			serviceName: "service-a",
			spans: []span{
				{
					name:       "/ping",
					kind:       ptrace.SpanKindServer,
					statusCode: ptrace.StatusCodeOk,
				},
			},
		}, traces0.ResourceSpans().AppendEmpty())
	initServiceSpans(
		serviceSpans{
			serviceName: "service-b",
			spans: []span{
				{
					name:       "/ping",
					kind:       ptrace.SpanKindServer,
					statusCode: ptrace.StatusCodeError,
				},
			},
		}, traces0.ResourceSpans().AppendEmpty())
	initServiceSpans(
		serviceSpans{
			serviceName: "service-c",
			spans: []span{
				{
					name:       "/ping",
					kind:       ptrace.SpanKindServer,
					statusCode: ptrace.StatusCodeError,
				},
			},
		}, traces0.ResourceSpans().AppendEmpty())

	// This trace does not have service-a, and may not result in an attempt to publish metrics for
	// service-a because service-a may be removed from the metricsKeyCache's evicted list.
	traces1 := ptrace.NewTraces()

	initServiceSpans(
		serviceSpans{
			serviceName: "service-b",
			spans: []span{
				{
					name:       "/ping",
					kind:       ptrace.SpanKindServer,
					statusCode: ptrace.StatusCodeError,
				},
			},
		}, traces1.ResourceSpans().AppendEmpty())
	initServiceSpans(
		serviceSpans{
			serviceName: "service-c",
			spans: []span{
				{
					name:       "/ping",
					kind:       ptrace.SpanKindServer,
					statusCode: ptrace.StatusCodeError,
				},
			},
		}, traces1.ResourceSpans().AppendEmpty())

	mcon := &mocks.MetricsConsumer{}

	wantDataPointCounts := []int{
		6, // (calls + latency) * (service-a + service-b + service-c)
		4, // (calls + latency) * (service-b + service-c)
	}

	// Ensure the assertion that wantDataPointCounts is performed only after all ConsumeMetrics
	// invocations are complete.
	var wg sync.WaitGroup
	wg.Add(len(wantDataPointCounts))

	// Mocked metric exporter will perform validation on metrics, during p.ConsumeMetrics()
	mcon.On("ConsumeMetrics", mock.Anything, mock.MatchedBy(func(input pmetric.Metrics) bool {
		defer wg.Done()

		// Verify
		require.NotEmpty(t, wantDataPointCounts)

		// GreaterOrEqual is particularly necessary for the second assertion where we
		// expect 4 data points; but could also be 6 due to the non-deterministic nature
		// of the p.histograms map which, through the act of "Get"ting a cached key, will
		// lead to updating its recent-ness.
		require.GreaterOrEqual(t, input.DataPointCount(), wantDataPointCounts[0])
		wantDataPointCounts = wantDataPointCounts[1:] // Dequeue

		return true
	})).Return(nil)

	defaultNullValue := pcommon.NewValueStr("defaultNullValue")
	mockClock := clock.NewMock(time.Now())
	ticker := mockClock.NewTicker(time.Nanosecond)

	// Note: default dimension key cache size is 2.
	p := newConnectorImp(mcon, &defaultNullValue, cumulative, zaptest.NewLogger(t), ticker)

	ctx := metadata.NewIncomingContext(context.Background(), nil)
	err := p.Start(ctx, componenttest.NewNopHost())
	defer func() { sdErr := p.Shutdown(ctx); require.NoError(t, sdErr) }()
	require.NoError(t, err)

	for _, traces := range []ptrace.Traces{traces0, traces1} {
		// Test
		err = p.ConsumeTraces(ctx, traces)
		require.NoError(t, err)

		// Allow time for metrics aggregation to complete.
		time.Sleep(time.Millisecond)

		// Trigger flush.
		mockClock.Add(time.Nanosecond)

		// Allow time for metrics flush to complete.
		time.Sleep(time.Millisecond)
	}

	wg.Wait()
	assert.Empty(t, wantDataPointCounts)
}

func TestBuildMetricName(t *testing.T) {
	tests := []struct {
		namespace  string
		metricName string
		expected   string
	}{
		{"", "metric", "metric"},
		{"ns", "metric", "ns.metric"},
		{"longer_namespace", "metric", "longer_namespace.metric"},
	}

	for _, test := range tests {
		actual := buildMetricName(test.namespace, test.metricName)
		assert.Equal(t, test.expected, actual)
	}
}
