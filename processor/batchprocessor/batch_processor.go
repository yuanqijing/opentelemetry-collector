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

package batchprocessor // import "go.opentelemetry.io/collector/processor/batchprocessor"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
)

// errTooManyBatchers is returned when the MetadataCardinalityLimit has been reached.
var errTooManyBatchers = consumererror.NewPermanent(errors.New("too many batcher metadata-value combinations"))

// batch_processor is a component that accepts spans and metrics, places them
// into batches and sends downstream.
//
// batch_processor implements consumer.Traces and consumer.Metrics
//
// Batches are sent out with any of the following conditions:
// - batch size reaches cfg.SendBatchSize
// - cfg.Timeout is elapsed since the timestamp when the previous batch was sent out.
type batchProcessor struct {
	logger           *zap.Logger
	timeout          time.Duration
	sendBatchSize    int
	sendBatchMaxSize int

	// batchFunc is a factory for new batch objects corresponding
	// with the appropriate signal.
	batchFunc func() batch

	// metadataKeys is the configured list of metadata keys.  When
	// empty, the `singleton` batcher is used.  When non-empty,
	// each distinct combination of metadata keys and values
	// triggers a new batcher, counted in `goroutines`.
	metadataKeys []string

	// metadataLimit is the limiting size of the batchers map.
	metadataLimit int

	shutdownC  chan struct{}
	goroutines sync.WaitGroup

	telemetry *batchProcessorTelemetry

	//  batcherFinder will be either *singletonBatcher or *multiBatcher
	batcherFinder
}

type batcherFinder interface {
	findBatcher(ctx context.Context) (*batcher, error)
	currentMetadataCardinality() int
}

// singleBatcher is used when metadataKeys is empty, to avoid the
// additional lock and map operations used in multiBatcher.
type singleBatcher struct {
	*batcher
}

// multiBatcher is used when metadataKeys is not empty.
type multiBatcher struct {
	*batchProcessor

	lock     sync.Mutex
	batchers map[attribute.Set]*batcher
}

// batcher is a single instance of the batcher logic.  When metadata
// keys are in use, one of these is created per distinct combination
// of values.
type batcher struct {
	// processor refers to this processor, for access to common
	// configuration.
	processor *batchProcessor

	// exportCtx is a context with the metadata key-values
	// corresponding with this batcher set.
	exportCtx context.Context

	// timer informs the batcher send a batch.
	timer *time.Timer

	// newItem is used to receive data items from producers.
	newItem chan any

	// batch is an in-flight data item containing one of the
	// underlying data types.
	batch batch
}

// batch is an interface generalizing the individual signal types.
type batch interface {
	// export the current batch
	export(ctx context.Context, sendBatchMaxSize int, returnBytes bool) (sentBatchSize int, sentBatchBytes int, err error)

	// itemCount returns the size of the current batch
	itemCount() int

	// add item to the current batch
	add(item any)
}

var _ consumer.Traces = (*batchProcessor)(nil)
var _ consumer.Metrics = (*batchProcessor)(nil)
var _ consumer.Logs = (*batchProcessor)(nil)

// newBatchProcessor returns a new batch processor component.
func newBatchProcessor(set processor.CreateSettings, cfg *Config, batchFunc func() batch, useOtel bool) (*batchProcessor, error) {
	// use lower-case, to be consistent with http/2 headers.
	mks := make([]string, len(cfg.MetadataKeys))
	for i, k := range cfg.MetadataKeys {
		mks[i] = strings.ToLower(k)
	}
	sort.Strings(mks)
	bp := &batchProcessor{
		logger: set.Logger,

		sendBatchSize:    int(cfg.SendBatchSize),
		sendBatchMaxSize: int(cfg.SendBatchMaxSize),
		timeout:          cfg.Timeout,
		batchFunc:        batchFunc,
		shutdownC:        make(chan struct{}, 1),
		metadataKeys:     mks,
		metadataLimit:    int(cfg.MetadataCardinalityLimit),
	}
	if len(bp.metadataKeys) == 0 {
		bp.batcherFinder = &singleBatcher{bp.newBatcher(nil)}
	} else {
		bp.batcherFinder = &multiBatcher{
			batchProcessor: bp,
			batchers:       map[attribute.Set]*batcher{},
		}
	}

	bpt, err := newBatchProcessorTelemetry(set, bp.currentMetadataCardinality, useOtel)
	if err != nil {
		return nil, fmt.Errorf("error creating batch processor telemetry: %w", err)
	}
	bp.telemetry = bpt

	return bp, nil
}

// newBatcher gets or creates a batcher corresponding with attrs.
func (bp *batchProcessor) newBatcher(md map[string][]string) *batcher {
	exportCtx := client.NewContext(context.Background(), client.Info{
		Metadata: client.NewMetadata(md),
	})
	b := &batcher{
		processor: bp,
		newItem:   make(chan any, runtime.NumCPU()),
		exportCtx: exportCtx,
		batch:     bp.batchFunc(),
	}
	b.processor.goroutines.Add(1)
	go b.start()
	return b
}

func (bp *batchProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

// Start is invoked during service startup.
func (bp *batchProcessor) Start(context.Context, component.Host) error {
	bp.goroutines.Add(1)
	return nil
}

// Shutdown is invoked during service shutdown.
func (bp *batchProcessor) Shutdown(context.Context) error {
	// Done corresponds with the initial Add(1) in Start.
	bp.goroutines.Done()

	close(bp.shutdownC)

	// Wait until all goroutines are done.
	bp.goroutines.Wait()
	return nil
}

func (b *batcher) start() {
	defer b.processor.goroutines.Done()

	// timerCh ensures we only block when there is a
	// timer, since <- from a nil channel is blocking.
	var timerCh <-chan time.Time
	if b.processor.timeout != 0 && b.processor.sendBatchSize != 0 {
		b.timer = time.NewTimer(b.processor.timeout)
		timerCh = b.timer.C
	}
	for {
		select {
		case <-b.processor.shutdownC:
		DONE:
			for {
				select {
				case item := <-b.newItem:
					b.processItem(item)
				default:
					break DONE
				}
			}
			// This is the close of the channel
			if b.batch.itemCount() > 0 {
				// TODO: Set a timeout on sendTraces or
				// make it cancellable using the context that Shutdown gets as a parameter
				b.sendItems(triggerTimeout)
			}
			return
		case item := <-b.newItem:
			if item == nil {
				continue
			}
			b.processItem(item)
		case <-timerCh:
			if b.batch.itemCount() > 0 {
				b.sendItems(triggerTimeout)
			}
			b.resetTimer()
		}
	}
}

func (b *batcher) processItem(item any) {
	b.batch.add(item)
	sent := false
	for b.batch.itemCount() > 0 && (!b.hasTimer() || b.batch.itemCount() >= b.processor.sendBatchSize) {
		sent = true
		b.sendItems(triggerBatchSize)
	}

	if sent {
		b.stopTimer()
		b.resetTimer()
	}
}

func (b *batcher) hasTimer() bool {
	return b.timer != nil
}

func (b *batcher) stopTimer() {
	if b.hasTimer() && !b.timer.Stop() {
		<-b.timer.C
	}
}

func (b *batcher) resetTimer() {
	if b.hasTimer() {
		b.timer.Reset(b.processor.timeout)
	}
}

func (b *batcher) sendItems(trigger trigger) {
	sent, bytes, err := b.batch.export(b.exportCtx, b.processor.sendBatchMaxSize, b.processor.telemetry.detailed)
	if err != nil {
		b.processor.logger.Warn("Sender failed", zap.Error(err))
	} else {
		b.processor.telemetry.record(trigger, int64(sent), int64(bytes))
	}
}

func (sb *singleBatcher) findBatcher(_ context.Context) (*batcher, error) {
	return sb.batcher, nil
}

func (mb *multiBatcher) findBatcher(ctx context.Context) (*batcher, error) {
	// Get each metadata key value, form the corresponding
	// attribute set for use as a map lookup key.
	info := client.FromContext(ctx)
	md := map[string][]string{}
	var attrs []attribute.KeyValue
	for _, k := range mb.metadataKeys {
		// Lookup the value in the incoming metadata, copy it
		// into the outgoing metadata, and create a unique
		// value for the attributeSet.
		vs := info.Metadata.Get(k)
		md[k] = vs
		if len(vs) == 1 {
			attrs = append(attrs, attribute.String(k, vs[0]))
		} else {
			attrs = append(attrs, attribute.StringSlice(k, vs))
		}
	}
	aset := attribute.NewSet(attrs...)

	mb.lock.Lock()
	defer mb.lock.Unlock()

	b, ok := mb.batchers[aset]
	if ok {
		return b, nil
	}

	if limit := mb.metadataLimit; limit != 0 && len(mb.batchers) >= limit {
		return nil, errTooManyBatchers
	}

	// aset.ToSlice() returns the sorted, deduplicated,
	// and name-downcased list of attributes.
	b = mb.newBatcher(md)
	mb.batchers[aset] = b
	return b, nil
}

func (sb *singleBatcher) currentMetadataCardinality() int {
	return 1
}

func (mb *multiBatcher) currentMetadataCardinality() int {
	mb.lock.Lock()
	defer mb.lock.Unlock()
	return len(mb.batchers)
}

// ConsumeTraces implements TracesProcessor
func (bp *batchProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	b, err := bp.findBatcher(ctx)
	if err != nil {
		return err
	}
	b.newItem <- td
	return nil
}

// ConsumeMetrics implements MetricsProcessor
func (bp *batchProcessor) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	b, err := bp.findBatcher(ctx)
	if err != nil {
		return nil
	}
	b.newItem <- md
	return nil
}

// ConsumeLogs implements LogsProcessor
func (bp *batchProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	b, err := bp.findBatcher(ctx)
	if err != nil {
		return nil
	}
	b.newItem <- ld
	return nil
}

// newBatchTracesProcessor creates a new batch processor that batches traces by size or with timeout
func newBatchTracesProcessor(set processor.CreateSettings, next consumer.Traces, cfg *Config, useOtel bool) (*batchProcessor, error) {
	return newBatchProcessor(set, cfg, func() batch { return newBatchTraces(next) }, useOtel)
}

// newBatchMetricsProcessor creates a new batch processor that batches metrics by size or with timeout
func newBatchMetricsProcessor(set processor.CreateSettings, next consumer.Metrics, cfg *Config, useOtel bool) (*batchProcessor, error) {
	return newBatchProcessor(set, cfg, func() batch { return newBatchMetrics(next) }, useOtel)
}

// newBatchLogsProcessor creates a new batch processor that batches logs by size or with timeout
func newBatchLogsProcessor(set processor.CreateSettings, next consumer.Logs, cfg *Config, useOtel bool) (*batchProcessor, error) {
	return newBatchProcessor(set, cfg, func() batch { return newBatchLogs(next) }, useOtel)
}

type batchTraces struct {
	nextConsumer consumer.Traces
	traceData    ptrace.Traces
	spanCount    int
	sizer        ptrace.Sizer
}

func newBatchTraces(nextConsumer consumer.Traces) *batchTraces {
	return &batchTraces{nextConsumer: nextConsumer, traceData: ptrace.NewTraces(), sizer: &ptrace.ProtoMarshaler{}}
}

// add updates current batchTraces by adding new TraceData object
func (bt *batchTraces) add(item any) {
	td := item.(ptrace.Traces)
	newSpanCount := td.SpanCount()
	if newSpanCount == 0 {
		return
	}

	bt.spanCount += newSpanCount
	td.ResourceSpans().MoveAndAppendTo(bt.traceData.ResourceSpans())
}

func (bt *batchTraces) export(ctx context.Context, sendBatchMaxSize int, returnBytes bool) (int, int, error) {
	var req ptrace.Traces
	var sent int
	var bytes int
	if sendBatchMaxSize > 0 && bt.itemCount() > sendBatchMaxSize {
		req = splitTraces(sendBatchMaxSize, bt.traceData)
		bt.spanCount -= sendBatchMaxSize
		sent = sendBatchMaxSize
	} else {
		req = bt.traceData
		sent = bt.spanCount
		bt.traceData = ptrace.NewTraces()
		bt.spanCount = 0
	}
	if returnBytes {
		bytes = bt.sizer.TracesSize(req)
	}
	return sent, bytes, bt.nextConsumer.ConsumeTraces(ctx, req)
}

func (bt *batchTraces) itemCount() int {
	return bt.spanCount
}

type batchMetrics struct {
	nextConsumer   consumer.Metrics
	metricData     pmetric.Metrics
	dataPointCount int
	sizer          pmetric.Sizer
}

func newBatchMetrics(nextConsumer consumer.Metrics) *batchMetrics {
	return &batchMetrics{nextConsumer: nextConsumer, metricData: pmetric.NewMetrics(), sizer: &pmetric.ProtoMarshaler{}}
}

func (bm *batchMetrics) export(ctx context.Context, sendBatchMaxSize int, returnBytes bool) (int, int, error) {
	var req pmetric.Metrics
	var sent int
	var bytes int
	if sendBatchMaxSize > 0 && bm.dataPointCount > sendBatchMaxSize {
		req = splitMetrics(sendBatchMaxSize, bm.metricData)
		bm.dataPointCount -= sendBatchMaxSize
		sent = sendBatchMaxSize
	} else {
		req = bm.metricData
		sent = bm.dataPointCount
		bm.metricData = pmetric.NewMetrics()
		bm.dataPointCount = 0
	}
	if returnBytes {
		bytes = bm.sizer.MetricsSize(req)
	}
	return sent, bytes, bm.nextConsumer.ConsumeMetrics(ctx, req)
}

func (bm *batchMetrics) itemCount() int {
	return bm.dataPointCount
}

func (bm *batchMetrics) add(item any) {
	md := item.(pmetric.Metrics)

	newDataPointCount := md.DataPointCount()
	if newDataPointCount == 0 {
		return
	}
	bm.dataPointCount += newDataPointCount
	md.ResourceMetrics().MoveAndAppendTo(bm.metricData.ResourceMetrics())
}

type batchLogs struct {
	nextConsumer consumer.Logs
	logData      plog.Logs
	logCount     int
	sizer        plog.Sizer
}

func newBatchLogs(nextConsumer consumer.Logs) *batchLogs {
	return &batchLogs{nextConsumer: nextConsumer, logData: plog.NewLogs(), sizer: &plog.ProtoMarshaler{}}
}

func (bl *batchLogs) export(ctx context.Context, sendBatchMaxSize int, returnBytes bool) (int, int, error) {
	var req plog.Logs
	var sent int
	var bytes int

	if sendBatchMaxSize > 0 && bl.logCount > sendBatchMaxSize {
		req = splitLogs(sendBatchMaxSize, bl.logData)
		bl.logCount -= sendBatchMaxSize
		sent = sendBatchMaxSize
	} else {
		req = bl.logData
		sent = bl.logCount
		bl.logData = plog.NewLogs()
		bl.logCount = 0
	}
	if returnBytes {
		bytes = bl.sizer.LogsSize(req)
	}
	return sent, bytes, bl.nextConsumer.ConsumeLogs(ctx, req)
}

func (bl *batchLogs) itemCount() int {
	return bl.logCount
}

func (bl *batchLogs) add(item any) {
	ld := item.(plog.Logs)

	newLogsCount := ld.LogRecordCount()
	if newLogsCount == 0 {
		return
	}
	bl.logCount += newLogsCount
	ld.ResourceLogs().MoveAndAppendTo(bl.logData.ResourceLogs())
}
