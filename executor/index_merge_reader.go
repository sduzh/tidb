// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tidb/util/set"
	"github.com/pingcap/tidb/util/stringutil"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

var (
	_ Executor = &IndexMergeReaderExecutor{}
)

// IndexMergeReaderExecutor accesses a table with multiple index/table scan.
// There are three types of workers:
// 1. partialTableWorker/partialIndexWorker, which are used to fetch the handles
// 2. indexMergeProcessWorker, which is used to do the `Union` operation.
// 3. indexMergeTableScanWorker, which is used to get the table tuples with the given handles.
//
// The execution flow is really like IndexLookUpReader. However, it uses multiple index scans
// or table scans to get the handles:
// 1. use the partialTableWorkers and partialIndexWorkers to fetch the handles (a batch per time)
//    and send them to the indexMergeProcessWorker.
// 2. indexMergeProcessWorker do the `Union` operation for a batch of handles it have got.
//    For every handle in the batch:
//	  1. check whether it has been accessed.
//    2. if not, record it and send it to the indexMergeTableScanWorker.
//    3. if accessed, just ignore it.
type IndexMergeReaderExecutor struct {
	baseExecutor

	table        table.Table
	indexes      []*model.IndexInfo
	descs        []bool
	ranges       [][]*ranger.Range
	dagPBs       []*tipb.DAGRequest
	startTS      uint64
	tableRequest *tipb.DAGRequest
	// columns are only required by union scan.
	columns           []*model.ColumnInfo
	partialStreamings []bool
	tableStreaming    bool
	*dataReaderBuilder
	// All fields above are immutable.

	tblWorkerWg sync.WaitGroup

	workerStarted bool
	keyRanges     [][]kv.KeyRange

	fetchPipe  *countedHandlePipe
	resultPipe *countedHandlePipe
	resultCurr *lookupTableTask
	feedbacks  []*statistics.QueryFeedback

	// memTracker is used to track the memory usage of this executor.
	memTracker *memory.Tracker

	// checkIndexValue is used to check the consistency of the index data.
	*checkIndexValue

	corColInIdxSide bool
	partialPlans    [][]plannercore.PhysicalPlan
	corColInTblSide bool
	tblPlans        []plannercore.PhysicalPlan
	corColInAccess  bool
	idxCols         [][]*expression.Column
	colLens         [][]int
}

// Open implements the Executor Open interface
func (e *IndexMergeReaderExecutor) Open(ctx context.Context) error {
	e.keyRanges = make([][]kv.KeyRange, 0, len(e.partialPlans))
	for i, plan := range e.partialPlans {
		_, ok := plan[0].(*plannercore.PhysicalIndexScan)
		if !ok {
			e.keyRanges = append(e.keyRanges, nil)
			continue
		}
		keyRange, err := distsql.IndexRangesToKVRanges(e.ctx.GetSessionVars().StmtCtx, getPhysicalTableID(e.table), e.indexes[i].ID, e.ranges[i], e.feedbacks[i])
		if err != nil {
			return err
		}
		e.keyRanges = append(e.keyRanges, keyRange)
	}
	return nil
}

func (e *IndexMergeReaderExecutor) startWorkers(ctx context.Context) error {
	var w partialWorker
	var err error
	workers := make([]partialWorker, 0, len(e.keyRanges))
	for i := 0; i < len(e.keyRanges); i++ {
		if e.indexes[i] != nil {
			w, err = e.newPartialIndexWorker(ctx, i, e.keyRanges[i])
		} else {
			w, err = e.newPartialTableWorker(ctx, i)
		}
		if err != nil {
			break
		}
		workers = append(workers, w)
	}
	if err != nil {
		for _, w := range workers {
			w.close(ctx)
		}
		return err
	}

	workPipe := newHandlePipe(1, 1)
	e.fetchPipe = newHandlePipe(len(workers), len(workers))
	e.resultPipe = newHandlePipe(e.ctx.GetSessionVars().IndexLookupConcurrency,
		int(atomic.LoadInt32(&LookupTableTaskChannelSize)))
	e.startIndexMergeTableScanWorker(ctx, workPipe, e.resultPipe)
	e.startIndexMergeProcessWorker(ctx, workPipe, e.fetchPipe)
	for i, w := range workers {
		go func(w partialWorker, id int) {
			w.start(ctx, e.fetchPipe, e.feedbacks[id], e.ctx)
			w.close(ctx)
		}(w, i)
	}
	e.workerStarted = true
	return nil
}

func (e *IndexMergeReaderExecutor) startIndexMergeProcessWorker(ctx context.Context, workPipe *countedHandlePipe, fetchPipe *countedHandlePipe) {
	idxMergeProcessWorker := &indexMergeProcessWorker{}
	go func() {
		var err error
		util.WithRecovery(
			func() {
				err = idxMergeProcessWorker.fetchLoop(ctx, fetchPipe, workPipe)
			},
			func(r interface{}) {
				if r != nil {
					err = errors.Errorf("panic in indexMergeProcessWorker: %v", r)
				}
			},
		)
		workPipe.finishWrite(err)
	}()
}

func (e *IndexMergeReaderExecutor) newPartialIndexWorker(ctx context.Context, workID int, keyRange []kv.KeyRange) (partialWorker, error) {
	if e.runtimeStats != nil {
		collExec := true
		e.dagPBs[workID].CollectExecutionSummaries = &collExec
	}

	var builder distsql.RequestBuilder
	kvReq, err := builder.SetKeyRanges(keyRange).
		SetDAGRequest(e.dagPBs[workID]).
		SetStartTS(e.startTS).
		SetDesc(e.descs[workID]).
		SetKeepOrder(false).
		SetStreaming(e.partialStreamings[workID]).
		SetFromSessionVars(e.ctx.GetSessionVars()).
		Build()
	if err != nil {
		return nil, err
	}

	result, err := distsql.SelectWithRuntimeStats(ctx, e.ctx, kvReq, []*types.FieldType{types.NewFieldType(mysql.TypeLonglong)}, e.feedbacks[workID], getPhysicalPlanIDs(e.partialPlans[workID]), e.id)
	if err != nil {
		return nil, err
	}

	result.Fetch(ctx)
	worker := &partialIndexWorker{
		result:       result,
		batchSize:    e.maxChunkSize,
		maxBatchSize: e.ctx.GetSessionVars().IndexLookupSize,
		maxChunkSize: e.maxChunkSize,
	}

	if worker.batchSize > worker.maxBatchSize {
		worker.batchSize = worker.maxBatchSize
	}

	failpoint.Inject("startPartialIndexWorkerErr", func() {
		failpoint.Return(nil, errors.New("inject an error before start partialIndexWorker"))
	})

	return worker, nil
}

func (e *IndexMergeReaderExecutor) buildPartialTableReader(ctx context.Context, workID int) Executor {
	tableReaderExec := &TableReaderExecutor{
		baseExecutor: newBaseExecutor(e.ctx, e.schema, stringutil.MemoizeStr(func() string { return e.id.String() + "_tableReader" })),
		table:        e.table,
		dagPB:        e.dagPBs[workID],
		startTS:      e.startTS,
		streaming:    e.partialStreamings[workID],
		feedback:     statistics.NewQueryFeedback(0, nil, 0, false),
		plans:        e.partialPlans[workID],
		ranges:       e.ranges[workID],
	}
	return tableReaderExec
}

func (e *IndexMergeReaderExecutor) newPartialTableWorker(ctx context.Context, workID int) (partialWorker, error) {
	partialTableReader := e.buildPartialTableReader(ctx, workID)
	err := partialTableReader.Open(ctx)
	if err != nil {
		logutil.Logger(ctx).Error("open Select result failed:", zap.Error(err))
		return nil, err
	}
	tableInfo := e.partialPlans[workID][0].(*plannercore.PhysicalTableScan).Table
	worker := &partialTableWorker{
		batchSize:    e.maxChunkSize,
		maxBatchSize: e.ctx.GetSessionVars().IndexLookupSize,
		maxChunkSize: e.maxChunkSize,
		tableReader:  partialTableReader,
		tableInfo:    tableInfo,
	}

	if worker.batchSize > worker.maxBatchSize {
		worker.batchSize = worker.maxBatchSize
	}
	return worker, nil
}

type partialTableWorker struct {
	batchSize    int
	maxBatchSize int
	maxChunkSize int
	tableReader  Executor
	tableInfo    *model.TableInfo
}

func (w *partialTableWorker) start(ctx context.Context, pipe *countedHandlePipe, feedback *statistics.QueryFeedback, sctx sessionctx.Context) {
	ctx1, cancel := context.WithCancel(ctx)
	err := w.fetchHandlesWithRecovery(ctx1, pipe)
	pipe.finishWrite(err)
	if err != nil {
		feedback.Invalidate()
	}
	cancel()
	sctx.StoreQueryFeedback(feedback)
}

func (w *partialTableWorker) close(ctx context.Context) {
	if w.tableReader == nil {
		return
	}
	if err := w.tableReader.Close(); err != nil {
		logutil.Logger(ctx).Error("close Select result failed:", zap.Error(err))
	}
	w.tableReader = nil
}

func (w *partialTableWorker) fetchHandlesWithRecovery(ctx context.Context, pipe *countedHandlePipe) (err error) {
	util.WithRecovery(
		func() { err = w.fetchHandles(ctx, pipe) },
		func(r interface{}) {
			if r != nil {
				err = errors.Errorf("panic in partialTableWorker: %v", r)
			}
		},
	)
	return
}

func (w *partialTableWorker) fetchHandles(ctx context.Context, pipe *countedHandlePipe) error {
	var chk *chunk.Chunk
	handleOffset := -1
	if w.tableInfo.PKIsHandle {
		handleCol := w.tableInfo.GetPkColInfo()
		columns := w.tableInfo.Columns
		for i := 0; i < len(columns); i++ {
			if columns[i].Name.L == handleCol.Name.L {
				handleOffset = i
				break
			}
		}
	} else {
		err := errors.Errorf("cannot find the column for handle")
		return err
	}

	chk = chunk.NewChunkWithCapacity(retTypes(w.tableReader), w.maxChunkSize)
	for {
		handles, _, err := w.extractTaskHandles(ctx, chk, handleOffset)
		if err != nil || len(handles) == 0 {
			return err
		}
		task := &lookupTableTask{handles: handles}
		if err := pipe.write(task); err != nil {
			return err
		}
	}
}

func (w *partialTableWorker) extractTaskHandles(ctx context.Context, chk *chunk.Chunk, handleOffset int) (
	handles []int64, retChk *chunk.Chunk, err error) {
	handles = make([]int64, 0, w.batchSize)
	for len(handles) < w.batchSize {
		chk.SetRequiredRows(w.batchSize-len(handles), w.maxChunkSize)
		err = errors.Trace(w.tableReader.Next(ctx, chk))
		if err != nil {
			return handles, nil, err
		}
		if chk.NumRows() == 0 {
			return handles, retChk, nil
		}
		for i := 0; i < chk.NumRows(); i++ {
			h := chk.GetRow(i).GetInt64(handleOffset)
			handles = append(handles, h)
		}
	}
	w.batchSize *= 2
	if w.batchSize > w.maxBatchSize {
		w.batchSize = w.maxBatchSize
	}
	return handles, retChk, nil
}

func (e *IndexMergeReaderExecutor) startIndexMergeTableScanWorker(ctx context.Context, workPipe *countedHandlePipe,
	resultPipe *countedHandlePipe) {
	lookupConcurrencyLimit := resultPipe.writers
	e.tblWorkerWg.Add(lookupConcurrencyLimit)
	for i := 0; i < lookupConcurrencyLimit; i++ {
		worker := &indexMergeTableScanWorker{
			workPipe:       workPipe,
			buildTblReader: e.buildFinalTableReader,
			tblPlans:       e.tblPlans,
			memTracker:     memory.NewTracker(stringutil.MemoizeStr(func() string { return "TableWorker_" + strconv.Itoa(i) }), -1),
		}
		ctx1, cancel := context.WithCancel(ctx)
		go func() {
			var err error
			util.WithRecovery(
				func() { err = worker.pickAndExecTask(ctx1, resultPipe) },
				func(r interface{}) {
					if r != nil {
						err = errors.Errorf("panic in indexMergeTableScanWorker: %v", r)
					}
				},
			)
			resultPipe.finishWrite(err)
			cancel()
			e.tblWorkerWg.Done()
		}()
	}
}

func (e *IndexMergeReaderExecutor) buildFinalTableReader(ctx context.Context, handles []int64) (Executor, error) {
	tableReaderExec := &TableReaderExecutor{
		baseExecutor: newBaseExecutor(e.ctx, e.schema, stringutil.MemoizeStr(func() string { return e.id.String() + "_tableReader" })),
		table:        e.table,
		dagPB:        e.tableRequest,
		startTS:      e.startTS,
		streaming:    e.tableStreaming,
		feedback:     statistics.NewQueryFeedback(0, nil, 0, false),
		plans:        e.tblPlans,
	}
	tableReader, err := e.dataReaderBuilder.buildTableReaderFromHandles(ctx, tableReaderExec, handles)
	if err != nil {
		logutil.Logger(ctx).Error("build table reader from handles failed", zap.Error(err))
		return nil, err
	}
	return tableReader, nil
}

// Next implements Executor Next interface.
func (e *IndexMergeReaderExecutor) Next(ctx context.Context, req *chunk.Chunk) error {
	if !e.workerStarted {
		if err := e.startWorkers(ctx); err != nil {
			return err
		}
	}

	req.Reset()
	for {
		resultTask, err := e.getResultTask()
		if err != nil {
			return errors.Trace(err)
		}
		if resultTask == nil {
			return nil
		}
		for resultTask.cursor < len(resultTask.rows) {
			req.AppendRow(resultTask.rows[resultTask.cursor])
			resultTask.cursor++
			if req.NumRows() >= e.maxChunkSize {
				return nil
			}
		}
	}
}

func (e *IndexMergeReaderExecutor) getResultTask() (*lookupTableTask, error) {
	if e.resultCurr != nil && e.resultCurr.cursor < len(e.resultCurr.rows) {
		return e.resultCurr, nil
	}
	task, err := e.resultPipe.read()
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, nil
	}
	if err := <-task.doneCh; err != nil {
		return nil, errors.Trace(err)
	}

	// Release the memory usage of last task before we handle a new task.
	if e.resultCurr != nil {
		e.resultCurr.memTracker.Consume(-e.resultCurr.memUsage)
	}
	e.resultCurr = task
	return e.resultCurr, nil
}

// Close implements Exec Close interface.
func (e *IndexMergeReaderExecutor) Close() error {
	if e.workerStarted {
		e.fetchPipe.close()
	}
	e.tblWorkerWg.Wait()
	e.workerStarted = false
	// TODO: how to store e.feedbacks
	return nil
}

type indexMergeProcessWorker struct {
}

func (w *indexMergeProcessWorker) fetchLoop(ctx context.Context, fetchPipe *countedHandlePipe, workPipe *countedHandlePipe) error {

	distinctHandles := set.NewInt64Set()

	for {
		task, err := fetchPipe.read()
		if err != nil || task == nil {
			return err
		}
		fhs := make([]int64, 0, 8)
		for _, h := range task.handles {
			if !distinctHandles.Exist(h) {
				fhs = append(fhs, h)
				distinctHandles.Insert(h)
			}
		}
		if len(fhs) == 0 {
			continue
		}
		task = &lookupTableTask{
			handles: fhs,
			doneCh:  make(chan error, 1),
		}
		if err := workPipe.write(task); err != nil {
			return nil
		}
	}
}

type partialWorker interface {
	start(ctx context.Context, pipe *countedHandlePipe, feedback *statistics.QueryFeedback, sctx sessionctx.Context)
	close(ctx context.Context)
}

type partialIndexWorker struct {
	result distsql.SelectResult

	batchSize    int
	maxBatchSize int
	maxChunkSize int
}

func (w *partialIndexWorker) start(ctx context.Context, pipe *countedHandlePipe, feedback *statistics.QueryFeedback, sctx sessionctx.Context) {
	ctx1, cancel := context.WithCancel(ctx)
	err := w.fetchHandlesWithRecovery(ctx1, pipe)
	pipe.finishWrite(err)
	if err != nil {
		feedback.Invalidate()
	}
	cancel()
	sctx.StoreQueryFeedback(feedback)
}

func (w *partialIndexWorker) close(ctx context.Context) {
	if w.result == nil {
		return
	}
	if err := w.result.Close(); err != nil {
		logutil.Logger(ctx).Error("close Select result failed:", zap.Error(err))
	}
	w.result = nil
}

func (w *partialIndexWorker) fetchHandlesWithRecovery(ctx context.Context, pipe *countedHandlePipe) (err error) {
	util.WithRecovery(
		func() { err = w.fetchHandles(ctx, pipe) },
		func(r interface{}) {
			if r != nil {
				err = errors.Errorf("panic in partialIndexWorker: %v", r)
			}
		},
	)
	return
}

func (w *partialIndexWorker) fetchHandles(ctx context.Context, pipe *countedHandlePipe) error {
	chk := chunk.NewChunkWithCapacity([]*types.FieldType{types.NewFieldType(mysql.TypeLonglong)}, w.maxChunkSize)
	for {
		handles, _, err := w.extractTaskHandles(ctx, chk, w.result)
		if err != nil || len(handles) == 0 {
			return err
		}
		task := &lookupTableTask{handles: handles}
		if err = pipe.write(task); err != nil {
			return err
		}
	}
}

func (w *partialIndexWorker) extractTaskHandles(ctx context.Context, chk *chunk.Chunk, idxResult distsql.SelectResult) (
	handles []int64, retChk *chunk.Chunk, err error) {
	handleOffset := chk.NumCols() - 1
	handles = make([]int64, 0, w.batchSize)
	for len(handles) < w.batchSize {
		chk.SetRequiredRows(w.batchSize-len(handles), w.maxChunkSize)
		err = errors.Trace(idxResult.Next(ctx, chk))
		if err != nil {
			return handles, nil, err
		}
		if chk.NumRows() == 0 {
			return handles, retChk, nil
		}
		for i := 0; i < chk.NumRows(); i++ {
			h := chk.GetRow(i).GetInt64(handleOffset)
			handles = append(handles, h)
		}
	}
	w.batchSize *= 2
	if w.batchSize > w.maxBatchSize {
		w.batchSize = w.maxBatchSize
	}
	return handles, retChk, nil
}

type indexMergeTableScanWorker struct {
	workPipe       *countedHandlePipe
	buildTblReader func(ctx context.Context, handles []int64) (Executor, error)
	tblPlans       []plannercore.PhysicalPlan

	// memTracker is used to track the memory usage of this executor.
	memTracker *memory.Tracker
}

func (w *indexMergeTableScanWorker) pickAndExecTask(ctx context.Context, resultPipe *countedHandlePipe) error {
	for {
		task, err := w.workPipe.read()
		if err != nil || task == nil {
			return err
		}
		task.doneCh = make(chan error, 1)
		if err = resultPipe.write(task); err != nil {
			return err
		}
		err = w.executeTask(ctx, task)
		task.doneCh <- err
	}
}

func (w *indexMergeTableScanWorker) executeTask(ctx context.Context, task *lookupTableTask) error {
	tableReader, err := w.buildTblReader(ctx, task.handles)
	if err != nil {
		logutil.Logger(ctx).Error("build table reader failed", zap.Error(err))
		return err
	}
	defer terror.Call(tableReader.Close)
	task.memTracker = w.memTracker
	memUsage := int64(cap(task.handles) * 8)
	task.memUsage = memUsage
	task.memTracker.Consume(memUsage)
	handleCnt := len(task.handles)
	task.rows = make([]chunk.Row, 0, handleCnt)
	for {
		chk := newFirstChunk(tableReader)
		err = Next(ctx, tableReader, chk)
		if err != nil {
			logutil.Logger(ctx).Error("table reader fetch next chunk failed", zap.Error(err))
			return err
		}
		if chk.NumRows() == 0 {
			break
		}
		memUsage = chk.MemoryUsage()
		task.memUsage += memUsage
		task.memTracker.Consume(memUsage)
		iter := chunk.NewIterator4Chunk(chk)
		for row := iter.Begin(); row != iter.End(); row = iter.Next() {
			task.rows = append(task.rows, row)
		}
	}

	memUsage = int64(cap(task.rows)) * int64(unsafe.Sizeof(chunk.Row{}))
	task.memUsage += memUsage
	task.memTracker.Consume(memUsage)
	if handleCnt != len(task.rows) && len(w.tblPlans) == 1 {
		return errors.Errorf("handle count %d isn't equal to value count %d", handleCnt, len(task.rows))
	}
	return nil
}

// countedHandlePipe is a wrapper on a channel with fixed number of writers.
// The channel will be closed automatically when all writers finish writing.
type countedHandlePipe struct {
	mtx      sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	writers  int
	err      error
	buff     chan *lookupTableTask
}

var errClosedPipe = errors.New("closed pipe")

func newHandlePipe(writers int, cap int) *countedHandlePipe {
	p := &countedHandlePipe{writers: writers, buff: make(chan *lookupTableTask, cap)}
	p.notEmpty = sync.NewCond(&p.mtx)
	p.notFull = sync.NewCond(&p.mtx)
	return p
}

func (p *countedHandlePipe) write(task *lookupTableTask) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	for p.err == nil && len(p.buff) >= cap(p.buff) {
		p.notFull.Wait()
	}
	if p.err != nil {
		return p.err
	}
	p.buff <- task
	p.notEmpty.Signal()
	return nil
}

func (p *countedHandlePipe) finishWrite(err error) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.err == nil {
		p.err = err
	}
	p.writers--
	if p.writers == 0 && p.err == nil {
		p.err = errClosedPipe
		p.notFull.Broadcast()
		p.notEmpty.Broadcast()
	}
}

func (p *countedHandlePipe) read() (*lookupTableTask, error) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	for len(p.buff) == 0 && p.err == nil {
		p.notEmpty.Wait()
	}
	if len(p.buff) > 0 {
		p.notFull.Signal()
		return <-p.buff, nil
	}
	if p.err == errClosedPipe {
		return nil, nil
	}
	return nil, p.err
}

func (p *countedHandlePipe) close() {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if p.err == nil {
		p.err = errClosedPipe
		p.notFull.Broadcast()
		p.notEmpty.Broadcast()
	}
}
