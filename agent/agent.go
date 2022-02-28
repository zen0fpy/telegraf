package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/models"
	"github.com/influxdata/telegraf/plugins/serializers/influx"
)

// Agent runs a set of plugins.
type Agent struct {
	Config *config.Config
}

// NewAgent returns an Agent for the given Config.
func NewAgent(config *config.Config) (*Agent, error) {
	a := &Agent{
		Config: config,
	}
	return a, nil
}

// inputUnit is a group of input plugins and the shared channel they write to.
//
// ┌───────┐
// │ Input │───┐
// └───────┘   │
// ┌───────┐   │     ______
// │ Input │───┼──▶ ()_____)
// └───────┘   │
// ┌───────┐   │
// │ Input │───┘
// └───────┘
type inputUnit struct {
	dst    chan<- telegraf.Metric
	inputs []*models.RunningInput
}

//  ______     ┌───────────┐     ______
// ()_____)──▶ │ Processor │──▶ ()_____)
//             └───────────┘
type processorUnit struct {
	src       <-chan telegraf.Metric
	dst       chan<- telegraf.Metric
	processor *models.RunningProcessor
}

// aggregatorUnit is a group of Aggregators and their source and sink channels.
// Typically the aggregators write to a processor channel and pass the original
// metrics to the output channel.  The sink channels may be the same channel.
//
//                 ┌────────────┐
//            ┌──▶ │ Aggregator │───┐
//            │    └────────────┘   │
//  ______    │    ┌────────────┐   │     ______
// ()_____)───┼──▶ │ Aggregator │───┼──▶ ()_____)
//            │    └────────────┘   │
//            │    ┌────────────┐   │
//            ├──▶ │ Aggregator │───┘
//            │    └────────────┘
//            │                           ______
//            └────────────────────────▶ ()_____)
type aggregatorUnit struct {
	src         <-chan telegraf.Metric
	aggC        chan<- telegraf.Metric        // 这里多一个aggregator chan用来保存聚合过程数据
	outputC     chan<- telegraf.Metric
	aggregators []*models.RunningAggregator
}

// outputUnit is a group of Outputs and their source channel.  Metrics on the
// channel are written to all outputs.
//
//                            ┌────────┐
//                       ┌──▶ │ Output │
//                       │    └────────┘
//  ______     ┌─────┐   │    ┌────────┐
// ()_____)──▶ │ Fan │───┼──▶ │ Output │
//             └─────┘   │    └────────┘
//                       │    ┌────────┐
//                       └──▶ │ Output │
//                            └────────┘
type outputUnit struct {
	src     <-chan telegraf.Metric
	outputs []*models.RunningOutput
}

// Run starts and runs the Agent until the context is done.
func (a *Agent) Run(ctx context.Context) error {
	log.Printf("I! [agent] Config: Interval:%s, Quiet:%#v, Hostname:%#v, "+
		"Flush Interval:%s",
		a.Config.Agent.Interval.Duration, a.Config.Agent.Quiet,
		a.Config.Agent.Hostname, a.Config.Agent.FlushInterval.Duration)

	log.Printf("D! [agent] Initializing plugins")
	err := a.initPlugins()
	if err != nil {
		return err
	}

	startTime := time.Now()

	// 启动Output
	log.Printf("D! [agent] Connecting outputs")
	next, ou, err := a.startOutputs(ctx, a.Config.Outputs)
	if err != nil {
		return err
	}

	// 启动Aggregators
	var apu []*processorUnit
	var au *aggregatorUnit
	if len(a.Config.Aggregators) != 0 {
		// 注意这里
		aggC := next
		if len(a.Config.AggProcessors) != 0 {
			// 聚合处理器
			aggC, apu, err = a.startProcessors(next, a.Config.AggProcessors)
			if err != nil {
				return err
			}
		}

		// 启动聚合
		next, au, err = a.startAggregators(aggC, next, a.Config.Aggregators)
		if err != nil {
			return err
		}
	}

	// 启动Processors
	var pu []*processorUnit
	if len(a.Config.Processors) != 0 {
		next, pu, err = a.startProcessors(next, a.Config.Processors)
		if err != nil {
			return err
		}
	}

	// 启动Input
	iu, err := a.startInputs(next, a.Config.Inputs)
	if err != nil {
		return err
	}


	// waitgroup 启动一个goroutines
	// 处理outputs、aggregator、processor、inputs等逻辑
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := a.runOutputs(ou)
		if err != nil {
			log.Printf("E! [agent] Error running outputs: %v", err)
		}
	}()




	if au != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runProcessors(apu)
			if err != nil {
				log.Printf("E! [agent] Error running processors: %v", err)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runAggregators(startTime, au)
			if err != nil {
				log.Printf("E! [agent] Error running aggregators: %v", err)
			}
		}()
	}

	if pu != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runProcessors(pu)
			if err != nil {
				log.Printf("E! [agent] Error running processors: %v", err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := a.runInputs(ctx, startTime, iu)
		if err != nil {
			log.Printf("E! [agent] Error running inputs: %v", err)
		}
	}()

	wg.Wait()

	log.Printf("D! [agent] Stopped Successfully")
	return err
}

// initPlugins runs the Init function on plugins.
func (a *Agent) initPlugins() error {
	for _, input := range a.Config.Inputs {
		err := input.Init()
		if err != nil {
			return fmt.Errorf("could not initialize input %s: %v",
				input.LogName(), err)
		}
	}
	for _, processor := range a.Config.Processors {
		err := processor.Init()
		if err != nil {
			return fmt.Errorf("could not initialize processor %s: %v",
				processor.Config.Name, err)
		}
	}
	for _, aggregator := range a.Config.Aggregators {
		err := aggregator.Init()
		if err != nil {
			return fmt.Errorf("could not initialize aggregator %s: %v",
				aggregator.Config.Name, err)
		}
	}
	for _, processor := range a.Config.AggProcessors {
		err := processor.Init()
		if err != nil {
			return fmt.Errorf("could not initialize processor %s: %v",
				processor.Config.Name, err)
		}
	}
	for _, output := range a.Config.Outputs {
		err := output.Init()
		if err != nil {
			return fmt.Errorf("could not initialize output %s: %v",
				output.Config.Name, err)
		}
	}
	return nil
}

// 启动Input
func (a *Agent) startInputs(
	dst chan<- telegraf.Metric,
	inputs []*models.RunningInput,
) (*inputUnit, error) {
	log.Printf("D! [agent] Starting service inputs")

	unit := &inputUnit{
		dst: dst,
	}

	for _, input := range inputs {
		if si, ok := input.Input.(telegraf.ServiceInput); ok {
			// Service input plugins are not normally subject to timestamp
			// rounding except for when precision is set on the input plugin.
			//
			// This only applies to the accumulator passed to Start(), the
			// Gather() accumulator does apply rounding according to the
			// precision and interval agent/plugin settings.
			var interval time.Duration
			var precision time.Duration // 一般不支持精度设置
			if input.Config.Precision != 0 {
				precision = input.Config.Precision
			}

			acc := NewAccumulator(input, dst)
			acc.SetPrecision(getPrecision(precision, interval))

			// Input Service启动
			err := si.Start(acc)
			if err != nil {
				stopServiceInputs(unit.inputs)
				return nil, fmt.Errorf("starting input %s: %w", input.LogName(), err)
			}
		}
		unit.inputs = append(unit.inputs, input)
	}

	return unit, nil
}

// runInputs starts and triggers the periodic gather for Inputs.
//
// When the context is done the timers are stopped and this function returns
// after all ongoing Gather calls complete.
func (a *Agent) runInputs(
	ctx context.Context,
	startTime time.Time,
	unit *inputUnit,
) error {
	var wg sync.WaitGroup
	for _, input := range unit.inputs {
		// Overwrite agent interval if this plugin has its own.
		interval := a.Config.Agent.Interval.Duration
		if input.Config.Interval != 0 {
			interval = input.Config.Interval
		}

		// Overwrite agent precision if this plugin has its own.
		precision := a.Config.Agent.Precision.Duration
		if input.Config.Precision != 0 {
			precision = input.Config.Precision
		}

		// Overwrite agent collection_jitter if this plugin has its own.
		jitter := a.Config.Agent.CollectionJitter.Duration
		if input.Config.CollectionJitter != 0 {
			jitter = input.Config.CollectionJitter
		}

		var ticker Ticker
		if a.Config.Agent.RoundInterval {
			ticker = NewAlignedTicker(startTime, interval, jitter)
		} else {
			ticker = NewUnalignedTicker(interval, jitter)
		}
		defer ticker.Stop()

		acc := NewAccumulator(input, unit.dst)
		acc.SetPrecision(getPrecision(precision, interval))

		wg.Add(1)
		go func(input *models.RunningInput) {
			defer wg.Done()
			a.gatherLoop(ctx, acc, input, ticker, interval)
		}(input)
	}

	wg.Wait()

	log.Printf("D! [agent] Stopping service inputs")
	stopServiceInputs(unit.inputs)

	close(unit.dst)
	log.Printf("D! [agent] Input channel closed")

	return nil
}

// testStartInputs is a variation of startInputs for use in --test and --once
// mode.  It differs by logging Start errors and returning only plugins
// successfully started.
func (a *Agent) testStartInputs(
	dst chan<- telegraf.Metric,
	inputs []*models.RunningInput,
) (*inputUnit, error) {
	log.Printf("D! [agent] Starting service inputs")

	unit := &inputUnit{
		dst: dst,
	}

	for _, input := range inputs {
		if si, ok := input.Input.(telegraf.ServiceInput); ok {
			// Service input plugins are not subject to timestamp rounding.
			// This only applies to the accumulator passed to Start(), the
			// Gather() accumulator does apply rounding according to the
			// precision agent setting.
			acc := NewAccumulator(input, dst)
			acc.SetPrecision(time.Nanosecond)

			err := si.Start(acc)
			if err != nil {
				log.Printf("E! [agent] Starting input %s: %v", input.LogName(), err)
			}

		}

		unit.inputs = append(unit.inputs, input)
	}

	return unit, nil
}

// testRunInputs is a variation of runInputs for use in --test and --once mode.
// Instead of using a ticker to run the inputs they are called once immediately.
func (a *Agent) testRunInputs(
	ctx context.Context,
	wait time.Duration,
	unit *inputUnit,
) error {
	var wg sync.WaitGroup

	nul := make(chan telegraf.Metric)
	go func() {
		for range nul {
		}
	}()

	for _, input := range unit.inputs {
		wg.Add(1)
		go func(input *models.RunningInput) {
			defer wg.Done()

			// Overwrite agent interval if this plugin has its own.
			interval := a.Config.Agent.Interval.Duration
			if input.Config.Interval != 0 {
				interval = input.Config.Interval
			}

			// Overwrite agent precision if this plugin has its own.
			precision := a.Config.Agent.Precision.Duration
			if input.Config.Precision != 0 {
				precision = input.Config.Precision
			}

			// Run plugins that require multiple gathers to calculate rate
			// and delta metrics twice.
			switch input.Config.Name {
			case "cpu", "mongodb", "procstat":
				nulAcc := NewAccumulator(input, nul)
				nulAcc.SetPrecision(getPrecision(precision, interval))
				if err := input.Input.Gather(nulAcc); err != nil {
					nulAcc.AddError(err)
				}

				time.Sleep(500 * time.Millisecond)
			}

			acc := NewAccumulator(input, unit.dst)
			acc.SetPrecision(getPrecision(precision, interval))

			if err := input.Input.Gather(acc); err != nil {
				acc.AddError(err)
			}
		}(input)
	}
	wg.Wait()

	internal.SleepContext(ctx, wait)

	log.Printf("D! [agent] Stopping service inputs")
	stopServiceInputs(unit.inputs)

	close(unit.dst)
	log.Printf("D! [agent] Input channel closed")
	return nil
}

// stopServiceInputs stops all service inputs.
func stopServiceInputs(inputs []*models.RunningInput) {
	for _, input := range inputs {
		if si, ok := input.Input.(telegraf.ServiceInput); ok {
			si.Stop()
		}
	}
}

// gather runs an input's gather function periodically until the context is
// done.
func (a *Agent) gatherLoop(
	ctx context.Context,
	acc telegraf.Accumulator,
	input *models.RunningInput,
	ticker Ticker,
	interval time.Duration,
) {
	defer panicRecover(input)

	for {
		select {
		case <-ticker.Elapsed():
			err := a.gatherOnce(acc, input, ticker, interval)
			if err != nil {
				acc.AddError(err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// gatherOnce runs the input's Gather function once, logging a warning each
// interval it fails to complete before.
func (a *Agent) gatherOnce(
	acc telegraf.Accumulator,
	input *models.RunningInput,
	ticker Ticker,
	interval time.Duration,
) error {
	done := make(chan error)
	go func() {
		done <- input.Gather(acc)
	}()

	// Only warn after interval seconds, even if the interval is started late.
	// Intervals can start late if the previous interval went over or due to
	// clock changes.
	slowWarning := time.NewTicker(interval)
	defer slowWarning.Stop()

	for {
		select {
		case err := <-done:
			return err
		case <-slowWarning.C:
			log.Printf("W! [%s] Collection took longer than expected; not complete after interval of %s",
				input.LogName(), interval)
		case <-ticker.Elapsed():
			log.Printf("D! [%s] Previous collection has not completed; scheduled collection skipped",
				input.LogName())
		}
	}
}

// startProcessors sets up the processor chain and calls Start on all
// processors.  If an error occurs any started processors are Stopped.
func (a *Agent) startProcessors(
	dst chan<- telegraf.Metric,  // 如果是AggrProcessor, dst来自Output用来接收Aggregators的chan
	processors models.RunningProcessors,
) (chan<- telegraf.Metric, []*processorUnit, error) {
	var units []*processorUnit

	// 逆向处理
	// Sort from last to first
	sort.SliceStable(processors, func(i, j int) bool {
		return processors[i].Config.Order > processors[j].Config.Order
	})


	// 多个处理器处理顺序
	var src chan telegraf.Metric
	for _, processor := range processors {
		src = make(chan telegraf.Metric, 100)
		// 累加器, 指标处理
		acc := NewAccumulator(processor, dst)


		err := processor.Start(acc)
		if err != nil {
			for _, u := range units {
				u.processor.Stop()
				close(u.dst)
			}
			return nil, nil, fmt.Errorf("starting processor %s: %w", processor.LogName(), err)
		}

		units = append(units, &processorUnit{
			src:       src,
			dst:       dst,
			processor: processor,
		})

		// 注意这里
		// Processor逆向排序过的，后面src=>前一个dst
		dst = src
	}

	return src, units, nil
}

// runProcessors begins processing metrics and runs until the source channel is
// closed and all metrics have been written.
func (a *Agent) runProcessors(
	units []*processorUnit,
) error {
	var wg sync.WaitGroup
	for _, unit := range units {
		wg.Add(1)
		go func(unit *processorUnit) {
			defer wg.Done()

			acc := NewAccumulator(unit.processor, unit.dst)
			for m := range unit.src {
				if err := unit.processor.Add(m, acc); err != nil {
					acc.AddError(err)
					m.Drop()
				}
			}
			unit.processor.Stop()
			close(unit.dst)
			log.Printf("D! [agent] Processor channel closed")
		}(unit)
	}
	wg.Wait()

	return nil
}

// startAggregators sets up the aggregator unit and returns the source channel.
// 初始化聚合器
func (a *Agent) startAggregators(
	aggC chan<- telegraf.Metric,
	outputC chan<- telegraf.Metric,
	aggregators []*models.RunningAggregator,

) (chan<- telegraf.Metric, *aggregatorUnit, error) {
	src := make(chan telegraf.Metric, 100)
	unit := &aggregatorUnit{
		src:         src,
		aggC:        aggC,
		outputC:     outputC,
		aggregators: aggregators,
	}
	return src, unit, nil
}

// runAggregators beings aggregating metrics and runs until the source channel
// is closed and all metrics have been written.
func (a *Agent) runAggregators(
	startTime time.Time,
	unit *aggregatorUnit,
) error {
	ctx, cancel := context.WithCancel(context.Background())

	// Before calling Add, initialize the aggregation window.  This ensures
	// that any metric created after start time will be aggregated.
	for _, agg := range a.Config.Aggregators {
		since, until := updateWindow(startTime, a.Config.Agent.RoundInterval, agg.Period())
		agg.UpdateWindow(since, until)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for metric := range unit.src {
			var dropOriginal bool
			for _, agg := range a.Config.Aggregators {
				if ok := agg.Add(metric); ok {
					dropOriginal = true
				}
			}

			if !dropOriginal {
				unit.outputC <- metric // keep original.
			} else {
				metric.Drop()
			}
		}
		cancel()
	}()

	for _, agg := range a.Config.Aggregators {
		wg.Add(1)
		go func(agg *models.RunningAggregator) {
			defer wg.Done()

			interval := a.Config.Agent.Interval.Duration
			precision := a.Config.Agent.Precision.Duration

			acc := NewAccumulator(agg, unit.aggC)
			acc.SetPrecision(getPrecision(precision, interval))
			a.push(ctx, agg, acc)
		}(agg)
	}

	wg.Wait()

	// In the case that there are no processors, both aggC and outputC are the
	// same channel.  If there are processors, we close the aggC and the
	// processor chain will close the outputC when it finishes processing.
	close(unit.aggC)
	log.Printf("D! [agent] Aggregator channel closed")

	return nil
}

func updateWindow(start time.Time, roundInterval bool, period time.Duration) (time.Time, time.Time) {
	var until time.Time
	if roundInterval {
		until = internal.AlignTime(start, period)
		if until == start {
			until = internal.AlignTime(start.Add(time.Nanosecond), period)
		}
	} else {
		until = start.Add(period)
	}

	since := until.Add(-period)

	return since, until
}

// push runs the push for a single aggregator every period.
func (a *Agent) push(
	ctx context.Context,
	aggregator *models.RunningAggregator,
	acc telegraf.Accumulator,
) {
	for {
		// Ensures that Push will be called for each period, even if it has
		// already elapsed before this function is called.  This is guaranteed
		// because so long as only Push updates the EndPeriod.  This method
		// also avoids drift by not using a ticker.
		until := time.Until(aggregator.EndPeriod())

		select {
		case <-time.After(until):
			aggregator.Push(acc)
			break
		case <-ctx.Done():
			aggregator.Push(acc)
			return
		}
	}
}

// startOutputs calls Connect on all outputs and returns the source channel.
// If an error occurs calling Connect all stared plugins have Close called.
func (a *Agent) startOutputs(
	ctx context.Context,
	outputs []*models.RunningOutput,
) (chan<- telegraf.Metric, *outputUnit, error) {
	// 存储100个来源指标的通道
	src := make(chan telegraf.Metric, 100)
	unit := &outputUnit{src: src}

	// 和远端Output Receiver建立连接
	for _, output := range outputs {
		err := a.connectOutput(ctx, output)

		// 建立连接失败，退出
		if err != nil {
			for _, output := range unit.outputs {
				output.Close()
			}
			return nil, nil, fmt.Errorf("connecting output %s: %w", output.LogName(), err)
		}

		unit.outputs = append(unit.outputs, output)
	}
	// 返回用来接收chan 和output unit
	return src, unit, nil
}


// 这里就是发起一个tcp连接，或者长连接
// 注意: 这里只有在开始插件启动时候,连接一次，后面失败会导致指标没有发送
// connectOutputs connects to all outputs.
func (a *Agent) connectOutput(ctx context.Context, output *models.RunningOutput) error {
	log.Printf("D! [agent] Attempting connection to [%s]", output.LogName())
	err := output.Output.Connect()
	if err != nil {
		log.Printf("E! [agent] Failed to connect to [%s], retrying in 15s, "+
			"error was '%s'", output.LogName(), err)

		// 连接失败后，等待15s后才连接
		err := internal.SleepContext(ctx, 15*time.Second)
		if err != nil {
			return err
		}

		// 第二次失败,退出不在重试
		err = output.Output.Connect()
		if err != nil {
			return fmt.Errorf("Error connecting to output %q: %w", output.LogName(), err)
		}
	}
	log.Printf("D! [agent] Successfully connected to %s", output.LogName())
	return nil
}

// runOutputs begins processing metrics and returns until the source channel is
// closed and all metrics have been written.  On shutdown metrics will be
// written one last time and dropped if unsuccessful.
func (a *Agent) runOutputs(
	unit *outputUnit,
) error {
	var wg sync.WaitGroup

	// 扇出:   one chan -> multi output
	// 用wg 实现
	// Start flush loop

	interval := a.Config.Agent.FlushInterval.Duration     // 刷新频率
	jitter := a.Config.Agent.FlushJitter.Duration         // 抖动

	// 控制goroutine退出
	ctx, cancel := context.WithCancel(context.Background())

	for _, output := range unit.outputs {
		interval := interval

		// Overwrite agent flush_interval if this plugin has its own.
		if output.Config.FlushInterval != 0 {
			interval = output.Config.FlushInterval
		}

		jitter := jitter
		// Overwrite agent flush_jitter if this plugin has its own.
		if output.Config.FlushJitter != 0 {
			jitter = output.Config.FlushJitter
		}

		// 启动一个goroutine
		// 如果output多的话，goroutines启动也就多了
		// 如果保证影响到业务
		wg.Add(1)
		go func(output *models.RunningOutput) {
			defer wg.Done()

			// 定时器
			ticker := NewRollingTicker(interval, jitter)
			defer ticker.Stop()

			a.flushLoop(ctx, output, ticker)
		}(output)
	}


	// 遍历aggregator src,  添加到output中
	for metric := range unit.src {
		for i, output := range unit.outputs {
			if i == len(a.Config.Outputs)-1 {
				output.AddMetric(metric)
			} else {
				output.AddMetric(metric.Copy())
			}
		}
	}

	log.Println("I! [agent] Hang on, flushing any cached metrics before shutdown")
	cancel()
	wg.Wait()

	return nil
}

// flushLoop runs an output's flush function periodically until the context is
// done.
func (a *Agent) flushLoop(
	ctx context.Context,
	output *models.RunningOutput,
	ticker Ticker,
) {
	logError := func(err error) {
		if err != nil {
			log.Printf("E! [agent] Error writing to %s: %v", output.LogName(), err)
		}
	}

	// watch for flush requests
	// 停止时候退出刷新
	flushRequested := make(chan os.Signal, 1)
	watchForFlushSignal(flushRequested)
	defer stopListeningForFlushSignal(flushRequested)

	// ticker时间就刷新一次
	for {
		// Favor shutdown over other methods.
		select {
		// 取消就退出
		case <-ctx.Done():
			logError(a.flushOnce(output, ticker, output.Write))
			return
		default:
		}

		select {
		case <-ctx.Done():
			logError(a.flushOnce(output, ticker, output.Write))
			return
		case <-ticker.Elapsed():  // 到点保存指标
			logError(a.flushOnce(output, ticker, output.Write))
		case <-flushRequested:    // 收到保存指标请求的信号
			logError(a.flushOnce(output, ticker, output.Write))
		case <-output.BatchReady: // 到达批量写入数目
			// Favor the ticker over batch ready
			select {
			case <-ticker.Elapsed(): // 也到定时写入时间
				logError(a.flushOnce(output, ticker, output.Write))
			default:
				logError(a.flushOnce(output, ticker, output.WriteBatch))
			}
		}
	}
}

// flushOnce runs the output's Write function once, logging a warning each
// interval it fails to complete before.
// 刷新一次指标
func (a *Agent) flushOnce(
	output *models.RunningOutput,
	ticker Ticker,
	writeFunc func() error,
) error {
	done := make(chan error)
	// 这里有启动一个goroutine保存指标
	go func() {
		done <- writeFunc()
	}()


	// 写入成功后保存 记录读取字节数
	for {
		select {
		case err := <-done:
			output.LogBufferStatus()
			return err
		case <-ticker.Elapsed(): // 定时到点
			log.Printf("W! [agent] [%q] did not complete within its flush interval",
				output.LogName())
			output.LogBufferStatus()
		}
	}
}

// Test runs the inputs, processors and aggregators for a single gather and
// writes the metrics to stdout.
func (a *Agent) Test(ctx context.Context, wait time.Duration) error {
	src := make(chan telegraf.Metric, 100)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s := influx.NewSerializer()
		s.SetFieldSortOrder(influx.SortFields)

		for metric := range src {
			octets, err := s.Serialize(metric)
			if err == nil {
				fmt.Print("> ", string(octets))
			}
			metric.Reject()
		}
	}()

	err := a.test(ctx, wait, src)
	if err != nil {
		return err
	}

	wg.Wait()

	if models.GlobalGatherErrors.Get() != 0 {
		return fmt.Errorf("input plugins recorded %d errors", models.GlobalGatherErrors.Get())
	}
	return nil
}

// Test runs the agent and performs a single gather sending output to the
// outputF.  After gathering pauses for the wait duration to allow service
// inputs to run.
func (a *Agent) test(ctx context.Context, wait time.Duration, outputC chan<- telegraf.Metric) error {
	log.Printf("D! [agent] Initializing plugins")
	err := a.initPlugins()
	if err != nil {
		return err
	}

	startTime := time.Now()

	next := outputC

	var apu []*processorUnit
	var au *aggregatorUnit
	if len(a.Config.Aggregators) != 0 {
		procC := next
		if len(a.Config.AggProcessors) != 0 {
			procC, apu, err = a.startProcessors(next, a.Config.AggProcessors)
			if err != nil {
				return err
			}
		}

		next, au, err = a.startAggregators(procC, next, a.Config.Aggregators)
		if err != nil {
			return err
		}
	}

	var pu []*processorUnit
	if len(a.Config.Processors) != 0 {
		next, pu, err = a.startProcessors(next, a.Config.Processors)
		if err != nil {
			return err
		}
	}

	iu, err := a.testStartInputs(next, a.Config.Inputs)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	if au != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runProcessors(apu)
			if err != nil {
				log.Printf("E! [agent] Error running processors: %v", err)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runAggregators(startTime, au)
			if err != nil {
				log.Printf("E! [agent] Error running aggregators: %v", err)
			}
		}()
	}

	if pu != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runProcessors(pu)
			if err != nil {
				log.Printf("E! [agent] Error running processors: %v", err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := a.testRunInputs(ctx, wait, iu)
		if err != nil {
			log.Printf("E! [agent] Error running inputs: %v", err)
		}
	}()

	wg.Wait()

	log.Printf("D! [agent] Stopped Successfully")

	return nil
}

// Once runs the full agent for a single gather.
func (a *Agent) Once(ctx context.Context, wait time.Duration) error {
	err := a.once(ctx, wait)
	if err != nil {
		return err
	}

	if models.GlobalGatherErrors.Get() != 0 {
		return fmt.Errorf("input plugins recorded %d errors", models.GlobalGatherErrors.Get())
	}

	unsent := 0
	for _, output := range a.Config.Outputs {
		unsent += output.BufferLength()
	}
	if unsent != 0 {
		return fmt.Errorf("output plugins unable to send %d metrics", unsent)
	}
	return nil
}

// On runs the agent and performs a single gather sending output to the
// outputF.  After gathering pauses for the wait duration to allow service
// inputs to run.
func (a *Agent) once(ctx context.Context, wait time.Duration) error {
	log.Printf("D! [agent] Initializing plugins")
	err := a.initPlugins()
	if err != nil {
		return err
	}

	startTime := time.Now()

	log.Printf("D! [agent] Connecting outputs")
	next, ou, err := a.startOutputs(ctx, a.Config.Outputs)
	if err != nil {
		return err
	}

	var apu []*processorUnit
	var au *aggregatorUnit
	if len(a.Config.Aggregators) != 0 {
		procC := next
		if len(a.Config.AggProcessors) != 0 {
			procC, apu, err = a.startProcessors(next, a.Config.AggProcessors)
			if err != nil {
				return err
			}
		}

		next, au, err = a.startAggregators(procC, next, a.Config.Aggregators)
		if err != nil {
			return err
		}
	}

	var pu []*processorUnit
	if len(a.Config.Processors) != 0 {
		next, pu, err = a.startProcessors(next, a.Config.Processors)
		if err != nil {
			return err
		}
	}

	iu, err := a.testStartInputs(next, a.Config.Inputs)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := a.runOutputs(ou)
		if err != nil {
			log.Printf("E! [agent] Error running outputs: %v", err)
		}
	}()

	if au != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runProcessors(apu)
			if err != nil {
				log.Printf("E! [agent] Error running processors: %v", err)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runAggregators(startTime, au)
			if err != nil {
				log.Printf("E! [agent] Error running aggregators: %v", err)
			}
		}()
	}

	if pu != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := a.runProcessors(pu)
			if err != nil {
				log.Printf("E! [agent] Error running processors: %v", err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := a.testRunInputs(ctx, wait, iu)
		if err != nil {
			log.Printf("E! [agent] Error running inputs: %v", err)
		}
	}()

	wg.Wait()

	log.Printf("D! [agent] Stopped Successfully")

	return nil
}

// Returns the rounding precision for metrics.
func getPrecision(precision, interval time.Duration) time.Duration {
	if precision > 0 {
		return precision
	}

	switch {
	case interval >= time.Second:
		return time.Second
	case interval >= time.Millisecond:
		return time.Millisecond
	case interval >= time.Microsecond:
		return time.Microsecond
	default:
		return time.Nanosecond
	}
}

// panicRecover displays an error if an input panics.
func panicRecover(input *models.RunningInput) {
	if err := recover(); err != nil {
		trace := make([]byte, 2048)
		runtime.Stack(trace, true)
		log.Printf("E! FATAL: [%s] panicked: %s, Stack:\n%s",
			input.LogName(), err, trace)
		log.Println("E! PLEASE REPORT THIS PANIC ON GITHUB with " +
			"stack trace, configuration, and OS information: " +
			"https://github.com/influxdata/telegraf/issues/new/choose")
	}
}
