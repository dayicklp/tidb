// Copyright 2015 PingCAP, Inc.
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
	"bytes"
	"context"
	"fmt"
	"runtime/trace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/ddl/placement"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/parser"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/parser/terror"
	"github.com/pingcap/tidb/planner"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/plugin"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/sessiontxn"
	"github.com/pingcap/tidb/sessiontxn/staleread"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/breakpoint"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/execdetails"
	"github.com/pingcap/tidb/util/hint"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/mathutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/plancodec"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tidb/util/stmtsummary"
	"github.com/pingcap/tidb/util/stringutil"
	"github.com/pingcap/tidb/util/topsql"
	topsqlstate "github.com/pingcap/tidb/util/topsql/state"
	"github.com/prometheus/client_golang/prometheus"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/oracle"
	"github.com/tikv/client-go/v2/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// metrics option
var (
	totalQueryProcHistogramGeneral  = metrics.TotalQueryProcHistogram.WithLabelValues(metrics.LblGeneral)
	totalCopProcHistogramGeneral    = metrics.TotalCopProcHistogram.WithLabelValues(metrics.LblGeneral)
	totalCopWaitHistogramGeneral    = metrics.TotalCopWaitHistogram.WithLabelValues(metrics.LblGeneral)
	totalQueryProcHistogramInternal = metrics.TotalQueryProcHistogram.WithLabelValues(metrics.LblInternal)
	totalCopProcHistogramInternal   = metrics.TotalCopProcHistogram.WithLabelValues(metrics.LblInternal)
	totalCopWaitHistogramInternal   = metrics.TotalCopWaitHistogram.WithLabelValues(metrics.LblInternal)
)

// processinfoSetter is the interface use to set current running process info.
type processinfoSetter interface {
	SetProcessInfo(string, time.Time, byte, uint64)
}

// recordSet wraps an executor, implements sqlexec.RecordSet interface
type recordSet struct {
	fields     []*ast.ResultField
	executor   Executor
	stmt       *ExecStmt
	lastErr    error
	txnStartTS uint64
}

func (a *recordSet) Fields() []*ast.ResultField {
	if len(a.fields) == 0 {
		a.fields = colNames2ResultFields(a.executor.Schema(), a.stmt.OutputNames, a.stmt.Ctx.GetSessionVars().CurrentDB)
	}
	return a.fields
}

func colNames2ResultFields(schema *expression.Schema, names []*types.FieldName, defaultDB string) []*ast.ResultField {
	rfs := make([]*ast.ResultField, 0, schema.Len())
	defaultDBCIStr := model.NewCIStr(defaultDB)
	for i := 0; i < schema.Len(); i++ {
		dbName := names[i].DBName
		if dbName.L == "" && names[i].TblName.L != "" {
			dbName = defaultDBCIStr
		}
		origColName := names[i].OrigColName
		if origColName.L == "" {
			origColName = names[i].ColName
		}
		rf := &ast.ResultField{
			Column:       &model.ColumnInfo{Name: origColName, FieldType: *schema.Columns[i].RetType},
			ColumnAsName: names[i].ColName,
			Table:        &model.TableInfo{Name: names[i].OrigTblName},
			TableAsName:  names[i].TblName,
			DBName:       dbName,
		}
		// This is for compatibility.
		// See issue https://github.com/pingcap/tidb/issues/10513 .
		if len(rf.ColumnAsName.O) > mysql.MaxAliasIdentifierLen {
			rf.ColumnAsName.O = rf.ColumnAsName.O[:mysql.MaxAliasIdentifierLen]
		}
		// Usually the length of O equals the length of L.
		// Add this len judgement to avoid panic.
		if len(rf.ColumnAsName.L) > mysql.MaxAliasIdentifierLen {
			rf.ColumnAsName.L = rf.ColumnAsName.L[:mysql.MaxAliasIdentifierLen]
		}
		rfs = append(rfs, rf)
	}
	return rfs
}

// Next use uses recordSet's executor to get next available chunk for later usage.
// If chunk does not contain any rows, then we update last query found rows in session variable as current found rows.
// The reason we need update is that chunk with 0 rows indicating we already finished current query, we need prepare for
// next query.
// If stmt is not nil and chunk with some rows inside, we simply update last query found rows by the number of row in chunk.
func (a *recordSet) Next(ctx context.Context, req *chunk.Chunk) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err = errors.Errorf("%v", r)
		logutil.Logger(ctx).Error("execute sql panic", zap.String("sql", a.stmt.GetTextToLog()), zap.Stack("stack"))
	}()

	err = a.stmt.next(ctx, a.executor, req)
	if err != nil {
		a.lastErr = err
		return err
	}
	numRows := req.NumRows()
	if numRows == 0 {
		if a.stmt != nil {
			a.stmt.Ctx.GetSessionVars().LastFoundRows = a.stmt.Ctx.GetSessionVars().StmtCtx.FoundRows()
		}
		return nil
	}
	if a.stmt != nil {
		a.stmt.Ctx.GetSessionVars().StmtCtx.AddFoundRows(uint64(numRows))
	}
	return nil
}

// NewChunk create a chunk base on top-level executor's newFirstChunk().
func (a *recordSet) NewChunk(alloc chunk.Allocator) *chunk.Chunk {
	if alloc == nil {
		return newFirstChunk(a.executor)
	}

	base := a.executor.base()
	return alloc.Alloc(base.retFieldTypes, base.initCap, base.maxChunkSize)
}

func (a *recordSet) Close() error {
	err := a.executor.Close()
	a.stmt.CloseRecordSet(a.txnStartTS, a.lastErr)
	return err
}

// OnFetchReturned implements commandLifeCycle#OnFetchReturned
func (a *recordSet) OnFetchReturned() {
	a.stmt.LogSlowQuery(a.txnStartTS, a.lastErr == nil, true)
}

// TelemetryInfo records some telemetry information during execution.
type TelemetryInfo struct {
	UseNonRecursive      bool
	UseRecursive         bool
	UseMultiSchemaChange bool
	PartitionTelemetry   *PartitionTelemetryInfo
}

// PartitionTelemetryInfo records table partition telemetry information during execution.
type PartitionTelemetryInfo struct {
	UseTablePartition              bool
	UseTablePartitionList          bool
	UseTablePartitionRange         bool
	UseTablePartitionHash          bool
	UseTablePartitionRangeColumns  bool
	UseTablePartitionListColumns   bool
	TablePartitionMaxPartitionsNum uint64
}

// ExecStmt implements the sqlexec.Statement interface, it builds a planner.Plan to an sqlexec.Statement.
type ExecStmt struct {
	// GoCtx stores parent go context.Context for a stmt.
	GoCtx context.Context
	// InfoSchema stores a reference to the schema information.
	InfoSchema infoschema.InfoSchema
	// Plan stores a reference to the final physical plan.
	Plan plannercore.Plan
	// Text represents the origin query text.
	Text string

	StmtNode ast.StmtNode

	Ctx sessionctx.Context

	// LowerPriority represents whether to lower the execution priority of a query.
	LowerPriority     bool
	isPreparedStmt    bool
	isSelectForUpdate bool
	retryCount        uint
	retryStartTime    time.Time

	// Phase durations are splited into two parts: 1. trying to lock keys (but
	// failed); 2. the final iteration of the retry loop. Here we use
	// [2]time.Duration to record such info for each phase. The first duration
	// is increased only within the current iteration. When we meet a
	// pessimistic lock error and decide to retry, we add the first duration to
	// the second and reset the first to 0 by calling `resetPhaseDurations`.
	phaseBuildDurations [2]time.Duration
	phaseOpenDurations  [2]time.Duration
	phaseNextDurations  [2]time.Duration
	phaseLockDurations  [2]time.Duration

	// OutputNames will be set if using cached plan
	OutputNames []*types.FieldName
	PsStmt      *plannercore.CachedPrepareStmt
	Ti          *TelemetryInfo
}

// GetStmtNode returns the stmtNode inside Statement
func (a ExecStmt) GetStmtNode() ast.StmtNode {
	return a.StmtNode
}

// PointGet short path for point exec directly from plan, keep only necessary steps
func (a *ExecStmt) PointGet(ctx context.Context) (*recordSet, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("ExecStmt.PointGet", opentracing.ChildOf(span.Context()))
		span1.LogKV("sql", a.OriginText())
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}
	failpoint.Inject("assertTxnManagerInShortPointGetPlan", func() {
		sessiontxn.RecordAssert(a.Ctx, "assertTxnManagerInShortPointGetPlan", true)
		// stale read should not reach here
		staleread.AssertStmtStaleness(a.Ctx, false)
		sessiontxn.AssertTxnManagerInfoSchema(a.Ctx, a.InfoSchema)
	})

	ctx = a.observeStmtBeginForTopSQL(ctx)
	startTs, err := sessiontxn.GetTxnManager(a.Ctx).GetStmtReadTS()
	if err != nil {
		return nil, err
	}
	a.Ctx.GetSessionVars().StmtCtx.Priority = kv.PriorityHigh

	// try to reuse point get executor
	if a.PsStmt.Executor != nil {
		exec, ok := a.PsStmt.Executor.(*PointGetExecutor)
		if !ok {
			logutil.Logger(ctx).Error("invalid executor type, not PointGetExecutor for point get path")
			a.PsStmt.Executor = nil
		} else {
			// CachedPlan type is already checked in last step
			pointGetPlan := a.PsStmt.PreparedAst.CachedPlan.(*plannercore.PointGetPlan)
			exec.Init(pointGetPlan)
			a.PsStmt.Executor = exec
		}
	}
	if a.PsStmt.Executor == nil {
		b := newExecutorBuilder(a.Ctx, a.InfoSchema, a.Ti)
		newExecutor := b.build(a.Plan)
		if b.err != nil {
			return nil, b.err
		}
		a.PsStmt.Executor = newExecutor
	}
	pointExecutor := a.PsStmt.Executor.(*PointGetExecutor)

	if err = pointExecutor.Open(ctx); err != nil {
		terror.Call(pointExecutor.Close)
		return nil, err
	}
	return &recordSet{
		executor:   pointExecutor,
		stmt:       a,
		txnStartTS: startTs,
	}, nil
}

// OriginText returns original statement as a string.
func (a *ExecStmt) OriginText() string {
	return a.Text
}

// IsPrepared returns true if stmt is a prepare statement.
func (a *ExecStmt) IsPrepared() bool {
	return a.isPreparedStmt
}

// IsReadOnly returns true if a statement is read only.
// If current StmtNode is an ExecuteStmt, we can get its prepared stmt,
// then using ast.IsReadOnly function to determine a statement is read only or not.
func (a *ExecStmt) IsReadOnly(vars *variable.SessionVars) bool {
	return planner.IsReadOnly(a.StmtNode, vars)
}

// RebuildPlan rebuilds current execute statement plan.
// It returns the current information schema version that 'a' is using.
func (a *ExecStmt) RebuildPlan(ctx context.Context) (int64, error) {
	ret := &plannercore.PreprocessorReturn{}
	if err := plannercore.Preprocess(a.Ctx, a.StmtNode, plannercore.InTxnRetry, plannercore.InitTxnContextProvider, plannercore.WithPreprocessorReturn(ret)); err != nil {
		return 0, err
	}

	failpoint.Inject("assertTxnManagerInRebuildPlan", func() {
		if is, ok := a.Ctx.Value(sessiontxn.AssertTxnInfoSchemaAfterRetryKey).(infoschema.InfoSchema); ok {
			a.Ctx.SetValue(sessiontxn.AssertTxnInfoSchemaKey, is)
			a.Ctx.SetValue(sessiontxn.AssertTxnInfoSchemaAfterRetryKey, nil)
		}
		sessiontxn.RecordAssert(a.Ctx, "assertTxnManagerInRebuildPlan", true)
		sessiontxn.AssertTxnManagerInfoSchema(a.Ctx, ret.InfoSchema)
		staleread.AssertStmtStaleness(a.Ctx, ret.IsStaleness)
		if ret.IsStaleness {
			sessiontxn.AssertTxnManagerReadTS(a.Ctx, ret.LastSnapshotTS)
		}
	})

	a.InfoSchema = sessiontxn.GetTxnManager(a.Ctx).GetTxnInfoSchema()
	replicaReadScope := sessiontxn.GetTxnManager(a.Ctx).GetReadReplicaScope()
	if a.Ctx.GetSessionVars().GetReplicaRead().IsClosestRead() && replicaReadScope == kv.GlobalReplicaScope {
		logutil.BgLogger().Warn(fmt.Sprintf("tidb can't read closest replicas due to it haven't %s label", placement.DCLabelKey))
	}
	p, names, err := planner.Optimize(ctx, a.Ctx, a.StmtNode, a.InfoSchema)
	if err != nil {
		return 0, err
	}
	a.OutputNames = names
	a.Plan = p
	a.Ctx.GetSessionVars().StmtCtx.SetPlan(p)
	return a.InfoSchema.SchemaMetaVersion(), nil
}

// IsFastPlan exports for testing.
func IsFastPlan(p plannercore.Plan) bool {
	if proj, ok := p.(*plannercore.PhysicalProjection); ok {
		p = proj.Children()[0]
	}
	switch p.(type) {
	case *plannercore.PointGetPlan:
		return true
	case *plannercore.PhysicalTableDual:
		// Plan of following SQL is PhysicalTableDual:
		// select 1;
		// select @@autocommit;
		return true
	case *plannercore.Set:
		// Plan of following SQL is Set:
		// set @a=1;
		// set @@autocommit=1;
		return true
	}
	return false
}

// Exec builds an Executor from a plan. If the Executor doesn't return result,
// like the INSERT, UPDATE statements, it executes in this function. If the Executor returns
// result, execution is done after this function returns, in the returned sqlexec.RecordSet Next method.
func (a *ExecStmt) Exec(ctx context.Context) (_ sqlexec.RecordSet, err error) {
	defer func() {
		r := recover()
		if r == nil {
			if a.retryCount > 0 {
				metrics.StatementPessimisticRetryCount.Observe(float64(a.retryCount))
			}
			lockKeysCnt := a.Ctx.GetSessionVars().StmtCtx.LockKeysCount
			if lockKeysCnt > 0 {
				metrics.StatementLockKeysCount.Observe(float64(lockKeysCnt))
			}
			return
		}
		if str, ok := r.(string); !ok || !strings.Contains(str, memory.PanicMemoryExceed) {
			panic(r)
		}
		err = errors.Errorf("%v", r)
		logutil.Logger(ctx).Error("execute sql panic", zap.String("sql", a.GetTextToLog()), zap.Stack("stack"))
	}()

	failpoint.Inject("assertStaleTSO", func(val failpoint.Value) {
		if n, ok := val.(int); ok && staleread.IsStmtStaleness(a.Ctx) {
			txnManager := sessiontxn.GetTxnManager(a.Ctx)
			ts, err := txnManager.GetStmtReadTS()
			if err != nil {
				panic(err)
			}
			startTS := oracle.ExtractPhysical(ts) / 1000
			if n != int(startTS) {
				panic(fmt.Sprintf("different tso %d != %d", n, startTS))
			}
		}
	})
	sctx := a.Ctx
	ctx = util.SetSessionID(ctx, sctx.GetSessionVars().ConnectionID)
	if _, ok := a.Plan.(*plannercore.Analyze); ok && sctx.GetSessionVars().InRestrictedSQL {
		oriStats, _ := sctx.GetSessionVars().GetSystemVar(variable.TiDBBuildStatsConcurrency)
		oriScan := sctx.GetSessionVars().DistSQLScanConcurrency()
		oriIndex := sctx.GetSessionVars().IndexSerialScanConcurrency()
		oriIso, _ := sctx.GetSessionVars().GetSystemVar(variable.TxnIsolation)
		terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TiDBBuildStatsConcurrency, "1"))
		sctx.GetSessionVars().SetDistSQLScanConcurrency(1)
		sctx.GetSessionVars().SetIndexSerialScanConcurrency(1)
		terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TxnIsolation, ast.ReadCommitted))
		defer func() {
			terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TiDBBuildStatsConcurrency, oriStats))
			sctx.GetSessionVars().SetDistSQLScanConcurrency(oriScan)
			sctx.GetSessionVars().SetIndexSerialScanConcurrency(oriIndex)
			terror.Log(sctx.GetSessionVars().SetSystemVar(variable.TxnIsolation, oriIso))
		}()
	}

	if sctx.GetSessionVars().StmtCtx.HasMemQuotaHint {
		sctx.GetSessionVars().StmtCtx.MemTracker.SetBytesLimit(sctx.GetSessionVars().StmtCtx.MemQuotaQuery)
	}

	e, err := a.buildExecutor()
	if err != nil {
		return nil, err
	}
	// ExecuteExec will rewrite `a.Plan`, so set plan label should be executed after `a.buildExecutor`.
	ctx = a.observeStmtBeginForTopSQL(ctx)

	breakpoint.Inject(a.Ctx, sessiontxn.BreakPointBeforeExecutorFirstRun)
	if err = a.openExecutor(ctx, e); err != nil {
		terror.Call(e.Close)
		return nil, err
	}

	cmd32 := atomic.LoadUint32(&sctx.GetSessionVars().CommandValue)
	cmd := byte(cmd32)
	var pi processinfoSetter
	if raw, ok := sctx.(processinfoSetter); ok {
		pi = raw
		sql := a.OriginText()
		if simple, ok := a.Plan.(*plannercore.Simple); ok && simple.Statement != nil {
			if ss, ok := simple.Statement.(ast.SensitiveStmtNode); ok {
				// Use SecureText to avoid leak password information.
				sql = ss.SecureText()
			}
		}
		maxExecutionTime := getMaxExecutionTime(sctx)
		// Update processinfo, ShowProcess() will use it.
		pi.SetProcessInfo(sql, time.Now(), cmd, maxExecutionTime)
		if a.Ctx.GetSessionVars().StmtCtx.StmtType == "" {
			a.Ctx.GetSessionVars().StmtCtx.StmtType = GetStmtLabel(a.StmtNode)
		}
	}

	failpoint.Inject("mockDelayInnerSessionExecute", func() {
		var curTxnStartTS uint64
		if cmd != mysql.ComSleep || sctx.GetSessionVars().InTxn() {
			curTxnStartTS = sctx.GetSessionVars().TxnCtx.StartTS
		}
		if sctx.GetSessionVars().SnapshotTS != 0 {
			curTxnStartTS = sctx.GetSessionVars().SnapshotTS
		}
		logutil.BgLogger().Info("Enable mockDelayInnerSessionExecute when execute statement",
			zap.Uint64("startTS", curTxnStartTS))
		time.Sleep(200 * time.Millisecond)
	})

	isPessimistic := sctx.GetSessionVars().TxnCtx.IsPessimistic

	// Special handle for "select for update statement" in pessimistic transaction.
	if isPessimistic && a.isSelectForUpdate {
		return a.handlePessimisticSelectForUpdate(ctx, e)
	}

	if handled, result, err := a.handleNoDelay(ctx, e, isPessimistic); handled || err != nil {
		return result, err
	}

	var txnStartTS uint64
	txn, err := sctx.Txn(false)
	if err != nil {
		return nil, err
	}
	if txn.Valid() {
		txnStartTS = txn.StartTS()
	}

	return &recordSet{
		executor:   e,
		stmt:       a,
		txnStartTS: txnStartTS,
	}, nil
}

func (a *ExecStmt) handleNoDelay(ctx context.Context, e Executor, isPessimistic bool) (handled bool, rs sqlexec.RecordSet, err error) {
	sc := a.Ctx.GetSessionVars().StmtCtx
	defer func() {
		// If the stmt have no rs like `insert`, The session tracker detachment will be directly
		// done in the `defer` function. If the rs is not nil, the detachment will be done in
		// `rs.Close` in `handleStmt`
		if sc != nil && rs == nil {
			if sc.MemTracker != nil {
				sc.MemTracker.Detach()
			}
			if sc.DiskTracker != nil {
				sc.DiskTracker.Detach()
			}
		}
	}()

	toCheck := e
	isExplainAnalyze := false
	if explain, ok := e.(*ExplainExec); ok {
		if analyze := explain.getAnalyzeExecToExecutedNoDelay(); analyze != nil {
			toCheck = analyze
			isExplainAnalyze = true
		}
	}

	// If the executor doesn't return any result to the client, we execute it without delay.
	if toCheck.Schema().Len() == 0 {
		handled = !isExplainAnalyze
		if isPessimistic {
			return handled, nil, a.handlePessimisticDML(ctx, toCheck)
		}
		r, err := a.handleNoDelayExecutor(ctx, toCheck)
		return handled, r, err
	} else if proj, ok := toCheck.(*ProjectionExec); ok && proj.calculateNoDelay {
		// Currently this is only for the "DO" statement. Take "DO 1, @a=2;" as an example:
		// the Projection has two expressions and two columns in the schema, but we should
		// not return the result of the two expressions.
		r, err := a.handleNoDelayExecutor(ctx, e)
		return true, r, err
	}

	return false, nil, nil
}

func isNoResultPlan(p plannercore.Plan) bool {
	if p.Schema().Len() == 0 {
		return true
	}

	// Currently this is only for the "DO" statement. Take "DO 1, @a=2;" as an example:
	// the Projection has two expressions and two columns in the schema, but we should
	// not return the result of the two expressions.
	switch raw := p.(type) {
	case *plannercore.LogicalProjection:
		if raw.CalculateNoDelay {
			return true
		}
	case *plannercore.PhysicalProjection:
		if raw.CalculateNoDelay {
			return true
		}
	}
	return false
}

// getMaxExecutionTime get the max execution timeout value.
func getMaxExecutionTime(sctx sessionctx.Context) uint64 {
	if sctx.GetSessionVars().StmtCtx.HasMaxExecutionTime {
		return sctx.GetSessionVars().StmtCtx.MaxExecutionTime
	}
	return sctx.GetSessionVars().MaxExecutionTime
}

type chunkRowRecordSet struct {
	rows     []chunk.Row
	idx      int
	fields   []*ast.ResultField
	e        Executor
	execStmt *ExecStmt
}

func (c *chunkRowRecordSet) Fields() []*ast.ResultField {
	return c.fields
}

func (c *chunkRowRecordSet) Next(ctx context.Context, chk *chunk.Chunk) error {
	chk.Reset()
	if !chk.IsFull() && c.idx < len(c.rows) {
		numToAppend := mathutil.Min(len(c.rows)-c.idx, chk.RequiredRows()-chk.NumRows())
		chk.AppendRows(c.rows[c.idx : c.idx+numToAppend])
		c.idx += numToAppend
	}
	return nil
}

func (c *chunkRowRecordSet) NewChunk(alloc chunk.Allocator) *chunk.Chunk {
	if alloc == nil {
		return newFirstChunk(c.e)
	}

	base := c.e.base()
	return alloc.Alloc(base.retFieldTypes, base.initCap, base.maxChunkSize)
}

func (c *chunkRowRecordSet) Close() error {
	c.execStmt.CloseRecordSet(c.execStmt.Ctx.GetSessionVars().TxnCtx.StartTS, nil)
	return nil
}

func (a *ExecStmt) handlePessimisticSelectForUpdate(ctx context.Context, e Executor) (sqlexec.RecordSet, error) {
	if snapshotTS := a.Ctx.GetSessionVars().SnapshotTS; snapshotTS != 0 {
		terror.Log(e.Close())
		return nil, errors.New("can not execute write statement when 'tidb_snapshot' is set")
	}

	for {
		rs, err := a.runPessimisticSelectForUpdate(ctx, e)
		e, err = a.handlePessimisticLockError(ctx, err)
		if err != nil {
			return nil, err
		}
		if e == nil {
			return rs, nil
		}
	}
}

func (a *ExecStmt) runPessimisticSelectForUpdate(ctx context.Context, e Executor) (sqlexec.RecordSet, error) {
	defer func() {
		terror.Log(e.Close())
	}()
	var rows []chunk.Row
	var err error
	req := newFirstChunk(e)
	for {
		err = a.next(ctx, e, req)
		if err != nil {
			// Handle 'write conflict' error.
			break
		}
		if req.NumRows() == 0 {
			fields := colNames2ResultFields(e.Schema(), a.OutputNames, a.Ctx.GetSessionVars().CurrentDB)
			return &chunkRowRecordSet{rows: rows, fields: fields, e: e, execStmt: a}, nil
		}
		iter := chunk.NewIterator4Chunk(req)
		for r := iter.Begin(); r != iter.End(); r = iter.Next() {
			rows = append(rows, r)
		}
		req = chunk.Renew(req, a.Ctx.GetSessionVars().MaxChunkSize)
	}
	return nil, err
}

func (a *ExecStmt) handleNoDelayExecutor(ctx context.Context, e Executor) (sqlexec.RecordSet, error) {
	sctx := a.Ctx
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("executor.handleNoDelayExecutor", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}

	var err error
	defer func() {
		terror.Log(e.Close())
		a.logAudit()
	}()

	// Check if "tidb_snapshot" is set for the write executors.
	// In history read mode, we can not do write operations.
	switch e.(type) {
	case *DeleteExec, *InsertExec, *UpdateExec, *ReplaceExec, *LoadDataExec, *DDLExec:
		snapshotTS := sctx.GetSessionVars().SnapshotTS
		if snapshotTS != 0 {
			return nil, errors.New("can not execute write statement when 'tidb_snapshot' is set")
		}
		lowResolutionTSO := sctx.GetSessionVars().LowResolutionTSO
		if lowResolutionTSO {
			return nil, errors.New("can not execute write statement when 'tidb_low_resolution_tso' is set")
		}
	}

	err = a.next(ctx, e, newFirstChunk(e))
	if err != nil {
		return nil, err
	}
	return nil, err
}

func (a *ExecStmt) handlePessimisticDML(ctx context.Context, e Executor) error {
	sctx := a.Ctx
	// Do not active the transaction here.
	// When autocommit = 0 and transaction in pessimistic mode,
	// statements like set xxx = xxx; should not active the transaction.
	txn, err := sctx.Txn(false)
	if err != nil {
		return err
	}
	txnCtx := sctx.GetSessionVars().TxnCtx
	for {
		startPointGetLocking := time.Now()
		_, err = a.handleNoDelayExecutor(ctx, e)
		if !txn.Valid() {
			return err
		}
		if err != nil {
			// It is possible the DML has point get plan that locks the key.
			e, err = a.handlePessimisticLockError(ctx, err)
			if err != nil {
				if ErrDeadlock.Equal(err) {
					metrics.StatementDeadlockDetectDuration.Observe(time.Since(startPointGetLocking).Seconds())
				}
				return err
			}
			continue
		}
		keys, err1 := txn.(pessimisticTxn).KeysNeedToLock()
		if err1 != nil {
			return err1
		}
		keys = txnCtx.CollectUnchangedLockKeys(keys)
		if len(keys) == 0 {
			return nil
		}
		keys = filterTemporaryTableKeys(sctx.GetSessionVars(), keys)
		seVars := sctx.GetSessionVars()
		keys = filterLockTableKeys(seVars.StmtCtx, keys)
		lockCtx, err := newLockCtx(sctx, seVars.LockWaitTimeout, len(keys))
		if err != nil {
			return err
		}
		var lockKeyStats *util.LockKeysDetails
		ctx = context.WithValue(ctx, util.LockKeysDetailCtxKey, &lockKeyStats)
		startLocking := time.Now()
		err = txn.LockKeys(ctx, lockCtx, keys...)
		a.phaseLockDurations[0] += time.Since(startLocking)
		if lockKeyStats != nil {
			seVars.StmtCtx.MergeLockKeysExecDetails(lockKeyStats)
		}
		if err == nil {
			return nil
		}
		e, err = a.handlePessimisticLockError(ctx, err)
		if err != nil {
			// todo: Report deadlock
			if ErrDeadlock.Equal(err) {
				metrics.StatementDeadlockDetectDuration.Observe(time.Since(startLocking).Seconds())
			}
			return err
		}
	}
}

// handlePessimisticLockError updates TS and rebuild executor if the err is write conflict.
func (a *ExecStmt) handlePessimisticLockError(ctx context.Context, lockErr error) (_ Executor, err error) {
	if lockErr == nil {
		return nil, nil
	}
	failpoint.Inject("assertPessimisticLockErr", func() {
		if terror.ErrorEqual(kv.ErrWriteConflict, lockErr) {
			sessiontxn.AddAssertEntranceForLockError(a.Ctx, "errWriteConflict")
		} else if terror.ErrorEqual(kv.ErrKeyExists, lockErr) {
			sessiontxn.AddAssertEntranceForLockError(a.Ctx, "errDuplicateKey")
		}
	})

	defer func() {
		if _, ok := errors.Cause(err).(*tikverr.ErrDeadlock); ok {
			err = ErrDeadlock
		}
	}()

	txnManager := sessiontxn.GetTxnManager(a.Ctx)
	action, err := txnManager.OnStmtErrorForNextAction(sessiontxn.StmtErrAfterPessimisticLock, lockErr)
	if err != nil {
		return nil, err
	}

	if action != sessiontxn.StmtActionRetryReady {
		return nil, lockErr
	}

	if a.retryCount >= config.GetGlobalConfig().PessimisticTxn.MaxRetryCount {
		return nil, errors.New("pessimistic lock retry limit reached")
	}
	a.retryCount++
	a.retryStartTime = time.Now()

	err = txnManager.OnStmtRetry(ctx)
	if err != nil {
		return nil, err
	}

	// Without this line of code, the result will still be correct. But it can ensure that the update time of for update read
	// is determined which is beneficial for testing.
	if _, err = txnManager.GetStmtForUpdateTS(); err != nil {
		return nil, err
	}

	breakpoint.Inject(a.Ctx, sessiontxn.BreakPointOnStmtRetryAfterLockError)

	a.resetPhaseDurations()

	e, err := a.buildExecutor()
	if err != nil {
		return nil, err
	}
	// Rollback the statement change before retry it.
	a.Ctx.StmtRollback()
	a.Ctx.GetSessionVars().StmtCtx.ResetForRetry()
	a.Ctx.GetSessionVars().RetryInfo.ResetOffset()

	failpoint.Inject("assertTxnManagerAfterPessimisticLockErrorRetry", func() {
		sessiontxn.RecordAssert(a.Ctx, "assertTxnManagerAfterPessimisticLockErrorRetry", true)
	})

	if err = a.openExecutor(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

type pessimisticTxn interface {
	kv.Transaction
	// KeysNeedToLock returns the keys need to be locked.
	KeysNeedToLock() ([]kv.Key, error)
}

// buildExecutor build an executor from plan, prepared statement may need additional procedure.
func (a *ExecStmt) buildExecutor() (Executor, error) {
	defer func(start time.Time) { a.phaseBuildDurations[0] += time.Since(start) }(time.Now())
	ctx := a.Ctx
	stmtCtx := ctx.GetSessionVars().StmtCtx
	if _, ok := a.Plan.(*plannercore.Execute); !ok {
		if stmtCtx.Priority == mysql.NoPriority && a.LowerPriority {
			stmtCtx.Priority = kv.PriorityLow
		}
	}
	if _, ok := a.Plan.(*plannercore.Analyze); ok && ctx.GetSessionVars().InRestrictedSQL {
		ctx.GetSessionVars().StmtCtx.Priority = kv.PriorityLow
	}

	b := newExecutorBuilder(ctx, a.InfoSchema, a.Ti)
	e := b.build(a.Plan)
	if b.err != nil {
		return nil, errors.Trace(b.err)
	}

	failpoint.Inject("assertTxnManagerAfterBuildExecutor", func() {
		sessiontxn.RecordAssert(a.Ctx, "assertTxnManagerAfterBuildExecutor", true)
		sessiontxn.AssertTxnManagerInfoSchema(b.ctx, b.is)
	})

	// ExecuteExec is not a real Executor, we only use it to build another Executor from a prepared statement.
	if executorExec, ok := e.(*ExecuteExec); ok {
		err := executorExec.Build(b)
		if err != nil {
			return nil, err
		}
		a.Ctx.SetValue(sessionctx.QueryString, executorExec.stmt.Text())
		a.OutputNames = executorExec.outputNames
		a.isPreparedStmt = true
		a.Plan = executorExec.plan
		a.Ctx.GetSessionVars().StmtCtx.SetPlan(executorExec.plan)
		if executorExec.lowerPriority {
			ctx.GetSessionVars().StmtCtx.Priority = kv.PriorityLow
		}
		e = executorExec.stmtExec
	}
	a.isSelectForUpdate = b.hasLock && (!stmtCtx.InDeleteStmt && !stmtCtx.InUpdateStmt && !stmtCtx.InInsertStmt)
	return e, nil
}

func (a *ExecStmt) openExecutor(ctx context.Context, e Executor) error {
	start := time.Now()
	err := e.Open(ctx)
	a.phaseOpenDurations[0] += time.Since(start)
	return err
}

func (a *ExecStmt) next(ctx context.Context, e Executor, req *chunk.Chunk) error {
	start := time.Now()
	err := Next(ctx, e, req)
	a.phaseNextDurations[0] += time.Since(start)
	return err
}

func (a *ExecStmt) resetPhaseDurations() {
	a.phaseBuildDurations[1] += a.phaseBuildDurations[0]
	a.phaseBuildDurations[0] = 0
	a.phaseOpenDurations[1] += a.phaseOpenDurations[0]
	a.phaseOpenDurations[0] = 0
	a.phaseNextDurations[1] += a.phaseNextDurations[0]
	a.phaseNextDurations[0] = 0
	a.phaseLockDurations[1] += a.phaseLockDurations[0]
	a.phaseLockDurations[0] = 0
}

// QueryReplacer replaces new line and tab for grep result including query string.
var QueryReplacer = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")

func (a *ExecStmt) logAudit() {
	sessVars := a.Ctx.GetSessionVars()
	if sessVars.InRestrictedSQL {
		return
	}

	err := plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
		audit := plugin.DeclareAuditManifest(p.Manifest)
		if audit.OnGeneralEvent != nil {
			cmd := mysql.Command2Str[byte(atomic.LoadUint32(&a.Ctx.GetSessionVars().CommandValue))]
			ctx := context.WithValue(context.Background(), plugin.ExecStartTimeCtxKey, a.Ctx.GetSessionVars().StartTime)
			audit.OnGeneralEvent(ctx, sessVars, plugin.Completed, cmd)
		}
		return nil
	})
	if err != nil {
		log.Error("log audit log failure", zap.Error(err))
	}
}

// FormatSQL is used to format the original SQL, e.g. truncating long SQL, appending prepared arguments.
func FormatSQL(sql string) stringutil.StringerFunc {
	return func() string {
		length := len(sql)
		maxQueryLen := variable.QueryLogMaxLen.Load()
		if maxQueryLen <= 0 {
			return QueryReplacer.Replace(sql) // no limit
		}
		if int32(length) > maxQueryLen {
			sql = fmt.Sprintf("%.*q(len:%d)", maxQueryLen, sql, length)
		}
		return QueryReplacer.Replace(sql)
	}
}

const (
	phaseBuildLocking       = "build:locking"
	phaseOpenLocking        = "open:locking"
	phaseNextLocking        = "next:locking"
	phaseLockLocking        = "lock:locking"
	phaseBuildFinal         = "build:final"
	phaseOpenFinal          = "open:final"
	phaseNextFinal          = "next:final"
	phaseLockFinal          = "lock:final"
	phaseCommitPrewrite     = "commit:prewrite"
	phaseCommitCommit       = "commit:commit"
	phaseCommitWaitCommitTS = "commit:wait:commit-ts"
	phaseCommitWaitLatestTS = "commit:wait:latest-ts"
	phaseCommitWaitLatch    = "commit:wait:local-latch"
	phaseCommitWaitBinlog   = "commit:wait:prewrite-binlog"
	phaseWriteResponse      = "write-response"
)

var (
	sessionExecuteRunDurationInternal = metrics.SessionExecuteRunDuration.WithLabelValues(metrics.LblInternal)
	sessionExecuteRunDurationGeneral  = metrics.SessionExecuteRunDuration.WithLabelValues(metrics.LblGeneral)
	totalTiFlashQuerySuccCounter      = metrics.TiFlashQueryTotalCounter.WithLabelValues("", metrics.LblOK)

	// pre-define observers for non-internal queries
	execBuildLocking       = metrics.ExecPhaseDuration.WithLabelValues(phaseBuildLocking, "0")
	execOpenLocking        = metrics.ExecPhaseDuration.WithLabelValues(phaseOpenLocking, "0")
	execNextLocking        = metrics.ExecPhaseDuration.WithLabelValues(phaseNextLocking, "0")
	execLockLocking        = metrics.ExecPhaseDuration.WithLabelValues(phaseLockLocking, "0")
	execBuildFinal         = metrics.ExecPhaseDuration.WithLabelValues(phaseBuildFinal, "0")
	execOpenFinal          = metrics.ExecPhaseDuration.WithLabelValues(phaseOpenFinal, "0")
	execNextFinal          = metrics.ExecPhaseDuration.WithLabelValues(phaseNextFinal, "0")
	execLockFinal          = metrics.ExecPhaseDuration.WithLabelValues(phaseLockFinal, "0")
	execCommitPrewrite     = metrics.ExecPhaseDuration.WithLabelValues(phaseCommitPrewrite, "0")
	execCommitCommit       = metrics.ExecPhaseDuration.WithLabelValues(phaseCommitCommit, "0")
	execCommitWaitCommitTS = metrics.ExecPhaseDuration.WithLabelValues(phaseCommitWaitCommitTS, "0")
	execCommitWaitLatestTS = metrics.ExecPhaseDuration.WithLabelValues(phaseCommitWaitLatestTS, "0")
	execCommitWaitLatch    = metrics.ExecPhaseDuration.WithLabelValues(phaseCommitWaitLatch, "0")
	execCommitWaitBinlog   = metrics.ExecPhaseDuration.WithLabelValues(phaseCommitWaitBinlog, "0")
	execWriteResponse      = metrics.ExecPhaseDuration.WithLabelValues(phaseWriteResponse, "0")
)

func getPhaseDurationObserver(phase string, internal bool) prometheus.Observer {
	if internal {
		return metrics.ExecPhaseDuration.WithLabelValues(phase, "1")
	}
	switch phase {
	case phaseBuildLocking:
		return execBuildLocking
	case phaseOpenLocking:
		return execOpenLocking
	case phaseNextLocking:
		return execNextLocking
	case phaseLockLocking:
		return execLockLocking
	case phaseBuildFinal:
		return execBuildFinal
	case phaseOpenFinal:
		return execOpenFinal
	case phaseNextFinal:
		return execNextFinal
	case phaseLockFinal:
		return execLockFinal
	case phaseCommitPrewrite:
		return execCommitPrewrite
	case phaseCommitCommit:
		return execCommitCommit
	case phaseCommitWaitCommitTS:
		return execCommitWaitCommitTS
	case phaseCommitWaitLatestTS:
		return execCommitWaitLatestTS
	case phaseCommitWaitLatch:
		return execCommitWaitLatch
	case phaseCommitWaitBinlog:
		return execCommitWaitBinlog
	case phaseWriteResponse:
		return execWriteResponse
	default:
		return metrics.ExecPhaseDuration.WithLabelValues(phase, "0")
	}
}

func (a *ExecStmt) observePhaseDurations(internal bool, commitDetails *util.CommitDetails) {
	for _, it := range []struct {
		duration time.Duration
		phase    string
	}{
		{a.phaseBuildDurations[0], phaseBuildFinal},
		{a.phaseBuildDurations[1], phaseBuildLocking},
		{a.phaseOpenDurations[0], phaseOpenFinal},
		{a.phaseOpenDurations[1], phaseOpenLocking},
		{a.phaseNextDurations[0], phaseNextFinal},
		{a.phaseNextDurations[1], phaseNextLocking},
		{a.phaseLockDurations[0], phaseLockFinal},
		{a.phaseLockDurations[1], phaseLockLocking},
	} {
		if it.duration > 0 {
			getPhaseDurationObserver(it.phase, internal).Observe(it.duration.Seconds())
		}
	}
	if commitDetails != nil {
		for _, it := range []struct {
			duration time.Duration
			phase    string
		}{
			{commitDetails.PrewriteTime, phaseCommitPrewrite},
			{commitDetails.CommitTime, phaseCommitCommit},
			{commitDetails.GetCommitTsTime, phaseCommitWaitCommitTS},
			{commitDetails.GetLatestTsTime, phaseCommitWaitLatestTS},
			{commitDetails.LocalLatchTime, phaseCommitWaitLatch},
			{commitDetails.WaitPrewriteBinlogTime, phaseCommitWaitBinlog},
		} {
			if it.duration > 0 {
				getPhaseDurationObserver(it.phase, internal).Observe(it.duration.Seconds())
			}
		}
	}
	if stmtDetailsRaw := a.GoCtx.Value(execdetails.StmtExecDetailKey); stmtDetailsRaw != nil {
		d := stmtDetailsRaw.(*execdetails.StmtExecDetails).WriteSQLRespDuration
		if d > 0 {
			getPhaseDurationObserver(phaseWriteResponse, internal).Observe(d.Seconds())
		}
	}
}

// FinishExecuteStmt is used to record some information after `ExecStmt` execution finished:
// 1. record slow log if needed.
// 2. record summary statement.
// 3. record execute duration metric.
// 4. update the `PrevStmt` in session variable.
// 5. reset `DurationParse` in session variable.
func (a *ExecStmt) FinishExecuteStmt(txnTS uint64, err error, hasMoreResults bool) {
	sessVars := a.Ctx.GetSessionVars()
	execDetail := sessVars.StmtCtx.GetExecDetails()
	// Attach commit/lockKeys runtime stats to executor runtime stats.
	if (execDetail.CommitDetail != nil || execDetail.LockKeysDetail != nil) && sessVars.StmtCtx.RuntimeStatsColl != nil {
		statsWithCommit := &execdetails.RuntimeStatsWithCommit{
			Commit:   execDetail.CommitDetail,
			LockKeys: execDetail.LockKeysDetail,
		}
		sessVars.StmtCtx.RuntimeStatsColl.RegisterStats(a.Plan.ID(), statsWithCommit)
	}
	// Record related SLI metrics.
	if execDetail.CommitDetail != nil && execDetail.CommitDetail.WriteSize > 0 {
		a.Ctx.GetTxnWriteThroughputSLI().AddTxnWriteSize(execDetail.CommitDetail.WriteSize, execDetail.CommitDetail.WriteKeys)
	}
	if execDetail.ScanDetail != nil && execDetail.ScanDetail.ProcessedKeys > 0 && sessVars.StmtCtx.AffectedRows() > 0 {
		// Only record the read keys in write statement which affect row more than 0.
		a.Ctx.GetTxnWriteThroughputSLI().AddReadKeys(execDetail.ScanDetail.ProcessedKeys)
	}
	succ := err == nil
	if a.Plan != nil {
		// If this statement has a Plan, the StmtCtx.plan should have been set when it comes here,
		// but we set it again in case we missed some code paths.
		sessVars.StmtCtx.SetPlan(a.Plan)
	}
	// `LowSlowQuery` and `SummaryStmt` must be called before recording `PrevStmt`.
	a.LogSlowQuery(txnTS, succ, hasMoreResults)
	a.SummaryStmt(succ)
	a.observeStmtFinishedForTopSQL()
	if sessVars.StmtCtx.IsTiFlash.Load() {
		if succ {
			totalTiFlashQuerySuccCounter.Inc()
		} else {
			metrics.TiFlashQueryTotalCounter.WithLabelValues(metrics.ExecuteErrorToLabel(err), metrics.LblError).Inc()
		}
	}
	sessVars.PrevStmt = FormatSQL(a.GetTextToLog())

	a.observePhaseDurations(sessVars.InRestrictedSQL, execDetail.CommitDetail)
	executeDuration := time.Since(sessVars.StartTime) - sessVars.DurationCompile
	if sessVars.InRestrictedSQL {
		sessionExecuteRunDurationInternal.Observe(executeDuration.Seconds())
	} else {
		sessionExecuteRunDurationGeneral.Observe(executeDuration.Seconds())
	}
	// Reset DurationParse due to the next statement may not need to be parsed (not a text protocol query).
	sessVars.DurationParse = 0
	// Clean the stale read flag when statement execution finish
	sessVars.StmtCtx.IsStaleness = false

	if sessVars.StmtCtx.ReadFromTableCache {
		metrics.ReadFromTableCacheCounter.Inc()
	}
}

// CloseRecordSet will finish the execution of current statement and do some record work
func (a *ExecStmt) CloseRecordSet(txnStartTS uint64, lastErr error) {
	a.FinishExecuteStmt(txnStartTS, lastErr, false)
	a.logAudit()
	// Detach the Memory and disk tracker for the previous stmtCtx from GlobalMemoryUsageTracker and GlobalDiskUsageTracker
	if stmtCtx := a.Ctx.GetSessionVars().StmtCtx; stmtCtx != nil {
		if stmtCtx.DiskTracker != nil {
			stmtCtx.DiskTracker.Detach()
		}
		if stmtCtx.MemTracker != nil {
			stmtCtx.MemTracker.Detach()
		}
	}
}

// LogSlowQuery is used to print the slow query in the log files.
func (a *ExecStmt) LogSlowQuery(txnTS uint64, succ bool, hasMoreResults bool) {
	sessVars := a.Ctx.GetSessionVars()
	stmtCtx := sessVars.StmtCtx
	level := log.GetLevel()
	cfg := config.GetGlobalConfig()
	costTime := time.Since(sessVars.StartTime) + sessVars.DurationParse
	threshold := time.Duration(atomic.LoadUint64(&cfg.Instance.SlowThreshold)) * time.Millisecond
	enable := cfg.Instance.EnableSlowLog.Load()
	// if the level is Debug, or trace is enabled, print slow logs anyway
	force := level <= zapcore.DebugLevel || trace.IsEnabled()
	if (!enable || costTime < threshold) && !force {
		return
	}
	sql := FormatSQL(a.GetTextToLog())
	_, digest := stmtCtx.SQLDigest()

	var indexNames string
	if len(stmtCtx.IndexNames) > 0 {
		// remove duplicate index.
		idxMap := make(map[string]struct{})
		buf := bytes.NewBuffer(make([]byte, 0, 4))
		buf.WriteByte('[')
		for _, idx := range stmtCtx.IndexNames {
			_, ok := idxMap[idx]
			if ok {
				continue
			}
			idxMap[idx] = struct{}{}
			if buf.Len() > 1 {
				buf.WriteByte(',')
			}
			buf.WriteString(idx)
		}
		buf.WriteByte(']')
		indexNames = buf.String()
	}
	flat := getFlatPlan(stmtCtx)
	var stmtDetail execdetails.StmtExecDetails
	stmtDetailRaw := a.GoCtx.Value(execdetails.StmtExecDetailKey)
	if stmtDetailRaw != nil {
		stmtDetail = *(stmtDetailRaw.(*execdetails.StmtExecDetails))
	}
	var tikvExecDetail util.ExecDetails
	tikvExecDetailRaw := a.GoCtx.Value(util.ExecDetailsKey)
	if tikvExecDetailRaw != nil {
		tikvExecDetail = *(tikvExecDetailRaw.(*util.ExecDetails))
	}
	execDetail := stmtCtx.GetExecDetails()
	copTaskInfo := stmtCtx.CopTasksDetails()
	statsInfos := plannercore.GetStatsInfoFromFlatPlan(flat)
	memMax := stmtCtx.MemTracker.MaxConsumed()
	diskMax := stmtCtx.DiskTracker.MaxConsumed()
	_, planDigest := getPlanDigest(stmtCtx)

	binaryPlan := ""
	if variable.GenerateBinaryPlan.Load() {
		binaryPlan = getBinaryPlan(a.Ctx)
		binaryPlan = variable.SlowLogBinaryPlanPrefix + binaryPlan + variable.SlowLogPlanSuffix
	}

	resultRows := GetResultRowsCount(stmtCtx, a.Plan)

	slowItems := &variable.SlowQueryLogItems{
		TxnTS:             txnTS,
		SQL:               sql.String(),
		Digest:            digest.String(),
		TimeTotal:         costTime,
		TimeParse:         sessVars.DurationParse,
		TimeCompile:       sessVars.DurationCompile,
		TimeOptimize:      sessVars.DurationOptimization,
		TimeWaitTS:        sessVars.DurationWaitTS,
		IndexNames:        indexNames,
		StatsInfos:        statsInfos,
		CopTasks:          copTaskInfo,
		ExecDetail:        execDetail,
		MemMax:            memMax,
		DiskMax:           diskMax,
		Succ:              succ,
		Plan:              getPlanTree(stmtCtx),
		PlanDigest:        planDigest.String(),
		BinaryPlan:        binaryPlan,
		Prepared:          a.isPreparedStmt,
		HasMoreResults:    hasMoreResults,
		PlanFromCache:     sessVars.FoundInPlanCache,
		PlanFromBinding:   sessVars.FoundInBinding,
		RewriteInfo:       sessVars.RewritePhaseInfo,
		KVTotal:           time.Duration(atomic.LoadInt64(&tikvExecDetail.WaitKVRespDuration)),
		PDTotal:           time.Duration(atomic.LoadInt64(&tikvExecDetail.WaitPDRespDuration)),
		BackoffTotal:      time.Duration(atomic.LoadInt64(&tikvExecDetail.BackoffDuration)),
		WriteSQLRespTotal: stmtDetail.WriteSQLRespDuration,
		ResultRows:        resultRows,
		ExecRetryCount:    a.retryCount,
		IsExplicitTxn:     sessVars.TxnCtx.IsExplicit,
		IsWriteCacheTable: stmtCtx.WaitLockLeaseTime > 0,
	}
	if a.retryCount > 0 {
		slowItems.ExecRetryTime = costTime - sessVars.DurationParse - sessVars.DurationCompile - time.Since(a.retryStartTime)
	}
	if _, ok := a.StmtNode.(*ast.CommitStmt); ok {
		slowItems.PrevStmt = sessVars.PrevStmt.String()
	}
	slowLog := sessVars.SlowLogFormat(slowItems)
	if trace.IsEnabled() {
		trace.Log(a.GoCtx, "details", slowLog)
	}
	logutil.SlowQueryLogger.Warn(slowLog)
	if costTime >= threshold {
		if sessVars.InRestrictedSQL {
			totalQueryProcHistogramInternal.Observe(costTime.Seconds())
			totalCopProcHistogramInternal.Observe(execDetail.TimeDetail.ProcessTime.Seconds())
			totalCopWaitHistogramInternal.Observe(execDetail.TimeDetail.WaitTime.Seconds())
		} else {
			totalQueryProcHistogramGeneral.Observe(costTime.Seconds())
			totalCopProcHistogramGeneral.Observe(execDetail.TimeDetail.ProcessTime.Seconds())
			totalCopWaitHistogramGeneral.Observe(execDetail.TimeDetail.WaitTime.Seconds())
		}
		var userString string
		if sessVars.User != nil {
			userString = sessVars.User.String()
		}
		var tableIDs string
		if len(stmtCtx.TableIDs) > 0 {
			tableIDs = strings.Replace(fmt.Sprintf("%v", stmtCtx.TableIDs), " ", ",", -1)
		}
		domain.GetDomain(a.Ctx).LogSlowQuery(&domain.SlowQueryInfo{
			SQL:        sql.String(),
			Digest:     digest.String(),
			Start:      sessVars.StartTime,
			Duration:   costTime,
			Detail:     stmtCtx.GetExecDetails(),
			Succ:       succ,
			ConnID:     sessVars.ConnectionID,
			TxnTS:      txnTS,
			User:       userString,
			DB:         sessVars.CurrentDB,
			TableIDs:   tableIDs,
			IndexNames: indexNames,
			Internal:   sessVars.InRestrictedSQL,
		})
	}
}

// GetResultRowsCount gets the count of the statement result rows.
func GetResultRowsCount(stmtCtx *stmtctx.StatementContext, p plannercore.Plan) int64 {
	runtimeStatsColl := stmtCtx.RuntimeStatsColl
	if runtimeStatsColl == nil {
		return 0
	}
	rootPlanID := p.ID()
	if !runtimeStatsColl.ExistsRootStats(rootPlanID) {
		return 0
	}
	rootStats := runtimeStatsColl.GetRootStats(rootPlanID)
	return rootStats.GetActRows()
}

// getFlatPlan generates a FlatPhysicalPlan from the plan stored in stmtCtx.plan,
// then stores it in stmtCtx.flatPlan.
func getFlatPlan(stmtCtx *stmtctx.StatementContext) *plannercore.FlatPhysicalPlan {
	pp := stmtCtx.GetPlan()
	if pp == nil {
		return nil
	}
	if flat := stmtCtx.GetFlatPlan(); flat != nil {
		f := flat.(*plannercore.FlatPhysicalPlan)
		return f
	}
	p := pp.(plannercore.Plan)
	flat := plannercore.FlattenPhysicalPlan(p, false)
	if flat != nil {
		stmtCtx.SetFlatPlan(flat)
		return flat
	}
	return nil
}

func getBinaryPlan(sCtx sessionctx.Context) string {
	stmtCtx := sCtx.GetSessionVars().StmtCtx
	binaryPlan := stmtCtx.GetBinaryPlan()
	if len(binaryPlan) > 0 {
		return binaryPlan
	}
	flat := getFlatPlan(stmtCtx)
	binaryPlan = plannercore.BinaryPlanStrFromFlatPlan(sCtx, flat)
	stmtCtx.SetBinaryPlan(binaryPlan)
	return binaryPlan
}

// getPlanTree will try to get the select plan tree if the plan is select or the select plan of delete/update/insert statement.
func getPlanTree(stmtCtx *stmtctx.StatementContext) string {
	cfg := config.GetGlobalConfig()
	if atomic.LoadUint32(&cfg.Instance.RecordPlanInSlowLog) == 0 {
		return ""
	}
	planTree, _ := getEncodedPlan(stmtCtx, false)
	if len(planTree) == 0 {
		return planTree
	}
	return variable.SlowLogPlanPrefix + planTree + variable.SlowLogPlanSuffix
}

// getPlanDigest will try to get the select plan tree if the plan is select or the select plan of delete/update/insert statement.
func getPlanDigest(stmtCtx *stmtctx.StatementContext) (string, *parser.Digest) {
	normalized, planDigest := stmtCtx.GetPlanDigest()
	if len(normalized) > 0 && planDigest != nil {
		return normalized, planDigest
	}
	flat := getFlatPlan(stmtCtx)
	normalized, planDigest = plannercore.NormalizeFlatPlan(flat)
	stmtCtx.SetPlanDigest(normalized, planDigest)
	return normalized, planDigest
}

// getEncodedPlan gets the encoded plan, and generates the hint string if indicated.
func getEncodedPlan(stmtCtx *stmtctx.StatementContext, genHint bool) (encodedPlan, hintStr string) {
	var hintSet bool
	encodedPlan = stmtCtx.GetEncodedPlan()
	hintStr, hintSet = stmtCtx.GetPlanHint()
	if len(encodedPlan) > 0 && (!genHint || hintSet) {
		return
	}
	flat := getFlatPlan(stmtCtx)
	if len(encodedPlan) == 0 {
		encodedPlan = plannercore.EncodeFlatPlan(flat)
		stmtCtx.SetEncodedPlan(encodedPlan)
	}
	if genHint {
		hints := plannercore.GenHintsFromFlatPlan(flat)
		for _, tableHint := range stmtCtx.OriginalTableHints {
			// some hints like 'memory_quota' cannot be extracted from the PhysicalPlan directly,
			// so we have to iterate all hints from the customer and keep some other necessary hints.
			switch tableHint.HintName.L {
			case "memory_quota", "use_toja", "no_index_merge", "max_execution_time",
				plannercore.HintAggToCop, plannercore.HintIgnoreIndex,
				plannercore.HintReadFromStorage, plannercore.HintLimitToCop:
				hints = append(hints, tableHint)
			}
		}

		hintStr = hint.RestoreOptimizerHints(hints)
		stmtCtx.SetPlanHint(hintStr)
	}
	return
}

// SummaryStmt collects statements for information_schema.statements_summary
func (a *ExecStmt) SummaryStmt(succ bool) {
	sessVars := a.Ctx.GetSessionVars()
	var userString string
	if sessVars.User != nil {
		userString = sessVars.User.Username
	}

	// Internal SQLs must also be recorded to keep the consistency of `PrevStmt` and `PrevStmtDigest`.
	if !stmtsummary.StmtSummaryByDigestMap.Enabled() || ((sessVars.InRestrictedSQL || len(userString) == 0) && !stmtsummary.StmtSummaryByDigestMap.EnabledInternal()) {
		sessVars.SetPrevStmtDigest("")
		return
	}
	// Ignore `PREPARE` statements, but record `EXECUTE` statements.
	if _, ok := a.StmtNode.(*ast.PrepareStmt); ok {
		return
	}
	stmtCtx := sessVars.StmtCtx
	// Make sure StmtType is filled even if succ is false.
	if stmtCtx.StmtType == "" {
		stmtCtx.StmtType = GetStmtLabel(a.StmtNode)
	}
	normalizedSQL, digest := stmtCtx.SQLDigest()
	costTime := time.Since(sessVars.StartTime) + sessVars.DurationParse
	charset, collation := sessVars.GetCharsetInfo()

	var prevSQL, prevSQLDigest string
	if _, ok := a.StmtNode.(*ast.CommitStmt); ok {
		// If prevSQLDigest is not recorded, it means this `commit` is the first SQL once stmt summary is enabled,
		// so it's OK just to ignore it.
		if prevSQLDigest = sessVars.GetPrevStmtDigest(); len(prevSQLDigest) == 0 {
			return
		}
		prevSQL = sessVars.PrevStmt.String()
	}
	sessVars.SetPrevStmtDigest(digest.String())

	// No need to encode every time, so encode lazily.
	planGenerator := func() (string, string) {
		return getEncodedPlan(stmtCtx, !sessVars.InRestrictedSQL)
	}
	var binPlanGen func() string
	if variable.GenerateBinaryPlan.Load() {
		binPlanGen = func() string {
			binPlan := getBinaryPlan(a.Ctx)
			return binPlan
		}
	}
	// Generating plan digest is slow, only generate it once if it's 'Point_Get'.
	// If it's a point get, different SQLs leads to different plans, so SQL digest
	// is enough to distinguish different plans in this case.
	var planDigest string
	var planDigestGen func() string
	if a.Plan.TP() == plancodec.TypePointGet {
		planDigestGen = func() string {
			_, planDigest := getPlanDigest(stmtCtx)
			return planDigest.String()
		}
	} else {
		_, tmp := getPlanDigest(stmtCtx)
		planDigest = tmp.String()
	}

	execDetail := stmtCtx.GetExecDetails()
	copTaskInfo := stmtCtx.CopTasksDetails()
	memMax := stmtCtx.MemTracker.MaxConsumed()
	diskMax := stmtCtx.DiskTracker.MaxConsumed()
	sql := a.GetTextToLog()
	var stmtDetail execdetails.StmtExecDetails
	stmtDetailRaw := a.GoCtx.Value(execdetails.StmtExecDetailKey)
	if stmtDetailRaw != nil {
		stmtDetail = *(stmtDetailRaw.(*execdetails.StmtExecDetails))
	}
	var tikvExecDetail util.ExecDetails
	tikvExecDetailRaw := a.GoCtx.Value(util.ExecDetailsKey)
	if tikvExecDetailRaw != nil {
		tikvExecDetail = *(tikvExecDetailRaw.(*util.ExecDetails))
	}

	if stmtCtx.WaitLockLeaseTime > 0 {
		if execDetail.BackoffSleep == nil {
			execDetail.BackoffSleep = make(map[string]time.Duration)
		}
		execDetail.BackoffSleep["waitLockLeaseForCacheTable"] = stmtCtx.WaitLockLeaseTime
		execDetail.BackoffTime += stmtCtx.WaitLockLeaseTime
		execDetail.TimeDetail.WaitTime += stmtCtx.WaitLockLeaseTime
	}

	resultRows := GetResultRowsCount(stmtCtx, a.Plan)

	stmtExecInfo := &stmtsummary.StmtExecInfo{
		SchemaName:          strings.ToLower(sessVars.CurrentDB),
		OriginalSQL:         sql,
		Charset:             charset,
		Collation:           collation,
		NormalizedSQL:       normalizedSQL,
		Digest:              digest.String(),
		PrevSQL:             prevSQL,
		PrevSQLDigest:       prevSQLDigest,
		PlanGenerator:       planGenerator,
		BinaryPlanGenerator: binPlanGen,
		PlanDigest:          planDigest,
		PlanDigestGen:       planDigestGen,
		User:                userString,
		TotalLatency:        costTime,
		ParseLatency:        sessVars.DurationParse,
		CompileLatency:      sessVars.DurationCompile,
		StmtCtx:             stmtCtx,
		CopTasks:            copTaskInfo,
		ExecDetail:          &execDetail,
		MemMax:              memMax,
		DiskMax:             diskMax,
		StartTime:           sessVars.StartTime,
		IsInternal:          sessVars.InRestrictedSQL,
		Succeed:             succ,
		PlanInCache:         sessVars.FoundInPlanCache,
		PlanInBinding:       sessVars.FoundInBinding,
		ExecRetryCount:      a.retryCount,
		StmtExecDetails:     stmtDetail,
		ResultRows:          resultRows,
		TiKVExecDetails:     tikvExecDetail,
		Prepared:            a.isPreparedStmt,
	}
	if a.retryCount > 0 {
		stmtExecInfo.ExecRetryTime = costTime - sessVars.DurationParse - sessVars.DurationCompile - time.Since(a.retryStartTime)
	}
	stmtsummary.StmtSummaryByDigestMap.AddStatement(stmtExecInfo)
}

// GetTextToLog return the query text to log.
func (a *ExecStmt) GetTextToLog() string {
	var sql string
	sessVars := a.Ctx.GetSessionVars()
	if sessVars.EnableRedactLog {
		sql, _ = sessVars.StmtCtx.SQLDigest()
	} else if sensitiveStmt, ok := a.StmtNode.(ast.SensitiveStmtNode); ok {
		sql = sensitiveStmt.SecureText()
	} else {
		sql = sessVars.StmtCtx.OriginalSQL + sessVars.PreparedParams.String()
	}
	return sql
}

func (a *ExecStmt) observeStmtBeginForTopSQL(ctx context.Context) context.Context {
	vars := a.Ctx.GetSessionVars()
	sc := vars.StmtCtx
	normalizedSQL, sqlDigest := sc.SQLDigest()
	normalizedPlan, planDigest := getPlanDigest(sc)
	var sqlDigestByte, planDigestByte []byte
	if sqlDigest != nil {
		sqlDigestByte = sqlDigest.Bytes()
	}
	if planDigest != nil {
		planDigestByte = planDigest.Bytes()
	}
	stats := a.Ctx.GetStmtStats()
	if !topsqlstate.TopSQLEnabled() {
		// To reduce the performance impact on fast plan.
		// Drop them does not cause notable accuracy issue in TopSQL.
		if IsFastPlan(a.Plan) {
			return ctx
		}
		// Always attach the SQL and plan info uses to catch the running SQL when Top SQL is enabled in execution.
		if stats != nil {
			stats.OnExecutionBegin(sqlDigestByte, planDigestByte)
			// This is a special logic prepared for TiKV's SQLExecCount.
			sc.KvExecCounter = stats.CreateKvExecCounter(sqlDigestByte, planDigestByte)
		}
		return topsql.AttachSQLAndPlanInfo(ctx, sqlDigest, planDigest)
	}

	if stats != nil {
		stats.OnExecutionBegin(sqlDigestByte, planDigestByte)
		// This is a special logic prepared for TiKV's SQLExecCount.
		sc.KvExecCounter = stats.CreateKvExecCounter(sqlDigestByte, planDigestByte)
	}

	isSQLRegistered := sc.IsSQLRegistered.Load()
	if !isSQLRegistered {
		topsql.RegisterSQL(normalizedSQL, sqlDigest, vars.InRestrictedSQL)
	}
	sc.IsSQLAndPlanRegistered.Store(true)
	if len(normalizedPlan) == 0 {
		return ctx
	}
	topsql.RegisterPlan(normalizedPlan, planDigest)
	return topsql.AttachSQLAndPlanInfo(ctx, sqlDigest, planDigest)
}

func (a *ExecStmt) observeStmtFinishedForTopSQL() {
	vars := a.Ctx.GetSessionVars()
	if vars == nil {
		return
	}
	if stats := a.Ctx.GetStmtStats(); stats != nil && topsqlstate.TopSQLEnabled() {
		sqlDigest, planDigest := a.getSQLPlanDigest()
		execDuration := time.Since(vars.StartTime) + vars.DurationParse
		stats.OnExecutionFinished(sqlDigest, planDigest, execDuration)
	}
}

func (a *ExecStmt) getSQLPlanDigest() ([]byte, []byte) {
	var sqlDigest, planDigest []byte
	vars := a.Ctx.GetSessionVars()
	if _, d := vars.StmtCtx.SQLDigest(); d != nil {
		sqlDigest = d.Bytes()
	}
	if _, d := vars.StmtCtx.GetPlanDigest(); d != nil {
		planDigest = d.Bytes()
	}
	return sqlDigest, planDigest
}
