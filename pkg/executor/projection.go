// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"fmt"
	"runtime/trace"
	"sync"
	"sync/atomic"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/execdetails"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/memory"
	"go.uber.org/zap"
)

// This file contains the implementation of the physical Projection Operator:
// https://en.wikipedia.org/wiki/Projection_(relational_algebra)
//
// NOTE:
// 1. The number of "projectionWorker" is controlled by the global session
//    variable "tidb_projection_concurrency".
// 2. Unparallel version is used when one of the following situations occurs:
//    a. "tidb_projection_concurrency" is set to 0.
//    b. The estimated input size is smaller than "tidb_max_chunk_size".
//    c. This projection can not be executed vectorially.

type projectionInput struct {
	chk          *chunk.Chunk
	targetWorker *projectionWorker
}

type projectionOutput struct {
	chk  *chunk.Chunk
	done chan error
}

// projectionExecutorContext is the execution context for the `ProjectionExec`
type projectionExecutorContext struct {
	stmtMemTracker             *memory.Tracker
	stmtRuntimeStatsColl       *execdetails.RuntimeStatsColl
	evalCtx                    expression.EvalContext
	enableVectorizedExpression bool
}

func newProjectionExecutorContext(sctx sessionctx.Context) projectionExecutorContext {
	return projectionExecutorContext{
		stmtMemTracker:             sctx.GetSessionVars().StmtCtx.MemTracker,
		stmtRuntimeStatsColl:       sctx.GetSessionVars().StmtCtx.RuntimeStatsColl,
		evalCtx:                    sctx.GetExprCtx().GetEvalCtx(),
		enableVectorizedExpression: sctx.GetSessionVars().EnableVectorizedExpression,
	}
}

// ProjectionExec implements the physical Projection Operator:
// https://en.wikipedia.org/wiki/Projection_(relational_algebra)
type ProjectionExec struct {
	projectionExecutorContext
	exec.BaseExecutorV2

	evaluatorSuit *expression.EvaluatorSuite

	finishCh    chan struct{}
	outputCh    chan *projectionOutput
	fetcher     projectionInputFetcher
	numWorkers  int64
	workers     []*projectionWorker
	childResult *chunk.Chunk

	// parentReqRows indicates how many rows the parent executor is
	// requiring. It is set when parallelExecute() is called and used by the
	// concurrent projectionInputFetcher.
	//
	// NOTE: It should be protected by atomic operations.
	parentReqRows int64

	memTracker *memory.Tracker
	wg         *sync.WaitGroup

	calculateNoDelay bool
	prepared         bool
}

// Open implements the Executor Open interface.
func (e *ProjectionExec) Open(ctx context.Context) error {
	if err := e.BaseExecutorV2.Open(ctx); err != nil {
		return err
	}
	failpoint.Inject("mockProjectionExecBaseExecutorOpenReturnedError", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(errors.New("mock ProjectionExec.baseExecutor.Open returned error"))
		}
	})
	return e.open(ctx)
}

func (e *ProjectionExec) open(_ context.Context) error {
	e.prepared = false
	e.parentReqRows = int64(e.MaxChunkSize())

	if e.memTracker != nil {
		e.memTracker.Reset()
	} else {
		e.memTracker = memory.NewTracker(e.ID(), -1)
	}
	e.memTracker.AttachTo(e.stmtMemTracker)

	// For now a Projection can not be executed vectorially only because it
	// contains "SetVar" or "GetVar" functions, in this scenario this
	// Projection can not be executed parallelly.
	if e.numWorkers > 0 && !e.evaluatorSuit.Vectorizable() {
		e.numWorkers = 0
	}

	if e.isUnparallelExec() {
		e.childResult = exec.TryNewCacheChunk(e.Children(0))
		e.memTracker.Consume(e.childResult.MemoryUsage())
	}

	e.wg = &sync.WaitGroup{}

	return nil
}

// Next implements the Executor Next interface.
//
// Here we explain the execution flow of the parallel projection implementation.
// There are 3 main components:
//   1. "projectionInputFetcher": Fetch input "Chunk" from child.
//   2. "projectionWorker":       Do the projection work.
//   3. "ProjectionExec.Next":    Return result to parent.
//
// 1. "projectionInputFetcher" gets its input and output resources from its
// "inputCh" and "outputCh" channel, once the input and output resources are
// obtained, it fetches child's result into "input.chk" and:
//   a. Dispatches this input to the worker specified in "input.targetWorker"
//   b. Dispatches this output to the main thread: "ProjectionExec.Next"
//   c. Dispatches this output to the worker specified in "input.targetWorker"
// It is finished and exited once:
//   a. There is no more input from child.
//   b. "ProjectionExec" close the "globalFinishCh"
//
// 2. "projectionWorker" gets its input and output resources from its
// "inputCh" and "outputCh" channel, once the input and output resources are
// abtained, it calculates the projection result use "input.chk" as the input
// and "output.chk" as the output, once the calculation is done, it:
//   a. Sends "nil" or error to "output.done" to mark this input is finished.
//   b. Returns the "input" resource to "projectionInputFetcher.inputCh"
// They are finished and exited once:
//   a. "ProjectionExec" closes the "globalFinishCh"
//
// 3. "ProjectionExec.Next" gets its output resources from its "outputCh" channel.
// After receiving an output from "outputCh", it should wait to receive a "nil"
// or error from "output.done" channel. Once a "nil" or error is received:
//   a. Returns this output to its parent
//   b. Returns the "output" resource to "projectionInputFetcher.outputCh"
/*
  +-----------+----------------------+--------------------------+
  |           |                      |                          |
  |  +--------+---------+   +--------+---------+       +--------+---------+
  |  | projectionWorker |   + projectionWorker |  ...  + projectionWorker |
  |  +------------------+   +------------------+       +------------------+
  |       ^       ^              ^       ^                  ^       ^
  |       |       |              |       |                  |       |
  |    inputCh outputCh       inputCh outputCh           inputCh outputCh
  |       ^       ^              ^       ^                  ^       ^
  |       |       |              |       |                  |       |
  |                              |       |
  |                              |       +----------------->outputCh
  |                              |       |                      |
  |                              |       |                      v
  |                      +-------+-------+--------+   +---------------------+
  |                      | projectionInputFetcher |   | ProjectionExec.Next |
  |                      +------------------------+   +---------+-----------+
  |                              ^       ^                      |
  |                              |       |                      |
  |                           inputCh outputCh                  |
  |                              ^       ^                      |
  |                              |       |                      |
  +------------------------------+       +----------------------+
*/
func (e *ProjectionExec) Next(ctx context.Context, req *chunk.Chunk) error {
	req.GrowAndReset(e.MaxChunkSize())
	if e.isUnparallelExec() {
		return e.unParallelExecute(ctx, req)
	}
	return e.parallelExecute(ctx, req)
}

func (e *ProjectionExec) isUnparallelExec() bool {
	return e.numWorkers <= 0
}

func (e *ProjectionExec) unParallelExecute(ctx context.Context, chk *chunk.Chunk) error {
	// transmit the requiredRows
	e.childResult.SetRequiredRows(chk.RequiredRows(), e.MaxChunkSize())
	mSize := e.childResult.MemoryUsage()
	err := exec.Next(ctx, e.Children(0), e.childResult)
	failpoint.Inject("ConsumeRandomPanic", nil)
	e.memTracker.Consume(e.childResult.MemoryUsage() - mSize)
	if err != nil {
		return err
	}
	if e.childResult.NumRows() == 0 {
		return nil
	}
	err = e.evaluatorSuit.Run(e.evalCtx, e.enableVectorizedExpression, e.childResult, chk)
	return err
}

func (e *ProjectionExec) parallelExecute(ctx context.Context, chk *chunk.Chunk) error {
	atomic.StoreInt64(&e.parentReqRows, int64(chk.RequiredRows()))
	if !e.prepared {
		e.prepare(ctx)
		e.prepared = true
	}

	output, ok := <-e.outputCh
	if !ok {
		return nil
	}

	err := <-output.done
	if err != nil {
		return err
	}
	mSize := output.chk.MemoryUsage()
	chk.SwapColumns(output.chk)
	failpoint.Inject("ConsumeRandomPanic", nil)
	e.memTracker.Consume(output.chk.MemoryUsage() - mSize)
	e.fetcher.outputCh <- output
	return nil
}

func (e *ProjectionExec) prepare(ctx context.Context) {
	e.finishCh = make(chan struct{})
	e.outputCh = make(chan *projectionOutput, e.numWorkers)

	// Initialize projectionInputFetcher.
	e.fetcher = projectionInputFetcher{
		proj:           e,
		child:          e.Children(0),
		globalFinishCh: e.finishCh,
		globalOutputCh: e.outputCh,
		inputCh:        make(chan *projectionInput, e.numWorkers),
		outputCh:       make(chan *projectionOutput, e.numWorkers),
	}

	// Initialize projectionWorker.
	e.workers = make([]*projectionWorker, 0, e.numWorkers)
	for i := range e.numWorkers {
		e.workers = append(e.workers, &projectionWorker{
			proj:            e,
			ctx:             e.projectionExecutorContext,
			evaluatorSuit:   e.evaluatorSuit,
			globalFinishCh:  e.finishCh,
			inputGiveBackCh: e.fetcher.inputCh,
			inputCh:         make(chan *projectionInput, 1),
			outputCh:        make(chan *projectionOutput, 1),
		})

		inputChk := exec.NewFirstChunk(e.Children(0))
		failpoint.Inject("ConsumeRandomPanic", nil)
		e.memTracker.Consume(inputChk.MemoryUsage())
		e.fetcher.inputCh <- &projectionInput{
			chk:          inputChk,
			targetWorker: e.workers[i],
		}

		outputChk := exec.NewFirstChunk(e)
		e.memTracker.Consume(outputChk.MemoryUsage())
		e.fetcher.outputCh <- &projectionOutput{
			chk:  outputChk,
			done: make(chan error, 1),
		}
	}

	e.wg.Add(1)
	go e.fetcher.run(ctx)

	for i := range e.workers {
		e.wg.Add(1)
		go e.workers[i].run(ctx)
	}
}

func (e *ProjectionExec) drainInputCh(ch chan *projectionInput) {
	close(ch)
	for item := range ch {
		if item.chk != nil {
			e.memTracker.Consume(-item.chk.MemoryUsage())
		}
	}
}

func (e *ProjectionExec) drainOutputCh(ch chan *projectionOutput) {
	close(ch)
	for item := range ch {
		if item.chk != nil {
			e.memTracker.Consume(-item.chk.MemoryUsage())
		}
	}
}

// Close implements the Executor Close interface.
func (e *ProjectionExec) Close() error {
	// if e.BaseExecutor.Open returns error, e.childResult will be nil, see https://github.com/pingcap/tidb/issues/24210
	// for more information
	if e.isUnparallelExec() && e.childResult != nil {
		e.memTracker.Consume(-e.childResult.MemoryUsage())
		e.childResult = nil
	}
	if e.prepared {
		close(e.finishCh)
		e.wg.Wait() // Wait for fetcher and workers to finish and exit.

		// clear fetcher
		e.drainInputCh(e.fetcher.inputCh)
		e.drainOutputCh(e.fetcher.outputCh)

		// clear workers
		for _, w := range e.workers {
			e.drainInputCh(w.inputCh)
			e.drainOutputCh(w.outputCh)
		}
	}
	if e.BaseExecutorV2.RuntimeStats() != nil {
		runtimeStats := &execdetails.RuntimeStatsWithConcurrencyInfo{}
		if e.isUnparallelExec() {
			runtimeStats.SetConcurrencyInfo(execdetails.NewConcurrencyInfo("Concurrency", 0))
		} else {
			runtimeStats.SetConcurrencyInfo(execdetails.NewConcurrencyInfo("Concurrency", int(e.numWorkers)))
		}
		e.stmtRuntimeStatsColl.RegisterStats(e.ID(), runtimeStats)
	}
	return e.BaseExecutorV2.Close()
}

type projectionInputFetcher struct {
	proj           *ProjectionExec
	child          exec.Executor
	globalFinishCh <-chan struct{}
	globalOutputCh chan<- *projectionOutput

	inputCh  chan *projectionInput
	outputCh chan *projectionOutput
}

// run gets projectionInputFetcher's input and output resources from its
// "inputCh" and "outputCh" channel, once the input and output resources are
// abtained, it fetches child's result into "input.chk" and:
//
//	a. Dispatches this input to the worker specified in "input.targetWorker"
//	b. Dispatches this output to the main thread: "ProjectionExec.Next"
//	c. Dispatches this output to the worker specified in "input.targetWorker"
//
// It is finished and exited once:
//
//	a. There is no more input from child.
//	b. "ProjectionExec" close the "globalFinishCh"
func (f *projectionInputFetcher) run(ctx context.Context) {
	defer trace.StartRegion(ctx, "ProjectionFetcher").End()
	var output *projectionOutput
	defer func() {
		if r := recover(); r != nil {
			recoveryProjection(output, r)
		}
		close(f.globalOutputCh)
		f.proj.wg.Done()
	}()

	for {
		input, isNil := readProjection[*projectionInput](f.inputCh, f.globalFinishCh)
		if isNil {
			return
		}
		targetWorker := input.targetWorker

		output, isNil = readProjection[*projectionOutput](f.outputCh, f.globalFinishCh)
		if isNil {
			f.proj.memTracker.Consume(-input.chk.MemoryUsage())
			return
		}

		f.globalOutputCh <- output

		requiredRows := atomic.LoadInt64(&f.proj.parentReqRows)
		input.chk.SetRequiredRows(int(requiredRows), f.proj.MaxChunkSize())
		mSize := input.chk.MemoryUsage()
		err := exec.Next(ctx, f.child, input.chk)
		failpoint.Inject("ConsumeRandomPanic", nil)
		f.proj.memTracker.Consume(input.chk.MemoryUsage() - mSize)
		if err != nil || input.chk.NumRows() == 0 {
			output.done <- err
			f.proj.memTracker.Consume(-input.chk.MemoryUsage())
			return
		}

		targetWorker.inputCh <- input
		targetWorker.outputCh <- output
	}
}

type projectionWorker struct {
	proj            *ProjectionExec
	ctx             projectionExecutorContext
	evaluatorSuit   *expression.EvaluatorSuite
	globalFinishCh  <-chan struct{}
	inputGiveBackCh chan<- *projectionInput

	// channel "input" and "output" is :
	// a. initialized by "ProjectionExec.prepare"
	// b. written	  by "projectionInputFetcher.run"
	// c. read    	  by "projectionWorker.run"
	inputCh  chan *projectionInput
	outputCh chan *projectionOutput
}

// run gets projectionWorker's input and output resources from its
// "inputCh" and "outputCh" channel, once the input and output resources are
// abtained, it calculate the projection result use "input.chk" as the input
// and "output.chk" as the output, once the calculation is done, it:
//
//	a. Sends "nil" or error to "output.done" to mark this input is finished.
//	b. Returns the "input" resource to "projectionInputFetcher.inputCh".
//
// It is finished and exited once:
//
//	a. "ProjectionExec" closes the "globalFinishCh".
func (w *projectionWorker) run(ctx context.Context) {
	defer trace.StartRegion(ctx, "ProjectionWorker").End()
	var output *projectionOutput
	defer func() {
		if r := recover(); r != nil {
			recoveryProjection(output, r)
		}
		w.proj.wg.Done()
	}()
	for {
		input, isNil := readProjection[*projectionInput](w.inputCh, w.globalFinishCh)
		if isNil {
			return
		}

		output, isNil = readProjection[*projectionOutput](w.outputCh, w.globalFinishCh)
		if isNil {
			return
		}

		mSize := output.chk.MemoryUsage() + input.chk.MemoryUsage()
		err := w.evaluatorSuit.Run(w.ctx.evalCtx, w.ctx.enableVectorizedExpression, input.chk, output.chk)
		failpoint.Inject("ConsumeRandomPanic", nil)
		w.proj.memTracker.Consume(output.chk.MemoryUsage() + input.chk.MemoryUsage() - mSize)
		output.done <- err

		if err != nil {
			return
		}

		w.inputGiveBackCh <- input
	}
}

func recoveryProjection(output *projectionOutput, r any) {
	if output != nil {
		output.done <- util.GetRecoverError(r)
	}
	logutil.BgLogger().Error("projection executor panicked", zap.String("error", fmt.Sprintf("%v", r)), zap.Stack("stack"))
}

func readProjection[T any](ch <-chan T, finishCh <-chan struct{}) (t T, isNil bool) {
	select {
	case <-finishCh:
		return t, true
	case t, ok := <-ch:
		if !ok {
			return t, true
		}
		return t, false
	}
}
