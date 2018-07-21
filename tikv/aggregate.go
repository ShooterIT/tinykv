package tikv

import (
	"github.com/juju/errors"
	"github.com/pingcap/tidb/executor/aggfuncs"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
	"golang.org/x/net/context"
)

type aggCtxsMapper map[string][]*aggregation.AggEvaluateContext

var (
	_ executor = &hashAggExec{}
	_ executor = &streamAggExec{}
)

type hashAggExec struct {
	evalCtx           *evalContext
	aggExprs          []aggregation.Aggregation
	aggCtxsMap        aggCtxsMapper
	groupByExprs      []expression.Expression
	relatedColOffsets []int
	row               types.DatumRow
	groups            map[string]struct{}
	groupKeys         [][]byte
	groupKeyRows      [][][]byte
	executed          bool
	currGroupIdx      int
	count             int64

	src executor

	tps []*types.FieldType
}

func (e *hashAggExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *hashAggExec) GetSrcExec() executor {
	return e.src
}

func (e *hashAggExec) ResetCounts() {
	e.src.ResetCounts()
}

func (e *hashAggExec) Counts() []int64 {
	return e.src.Counts()
}

func (e *hashAggExec) innerNext(ctx context.Context) (bool, error) {
	values, err := e.src.Next(ctx)
	if err != nil {
		return false, errors.Trace(err)
	}
	if values == nil {
		return false, nil
	}
	err = e.aggregate(values)
	if err != nil {
		return false, errors.Trace(err)
	}
	return true, nil
}

func (e *hashAggExec) Cursor() ([]byte, bool) {
	panic("don't not use coprocessor streaming API for hash aggregation!")
}

func (e *hashAggExec) Next(ctx context.Context) (value [][]byte, err error) {
	e.count++
	if e.aggCtxsMap == nil {
		e.aggCtxsMap = make(aggCtxsMapper, 0)
	}
	if !e.executed {
		for {
			hasMore, err := e.innerNext(ctx)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if !hasMore {
				break
			}
		}
		e.executed = true
	}

	if e.currGroupIdx >= len(e.groups) {
		return nil, nil
	}
	gk := e.groupKeys[e.currGroupIdx]
	value = make([][]byte, 0, len(e.groupByExprs)+2*len(e.aggExprs))
	aggCtxs := e.getContexts(gk)
	for i, agg := range e.aggExprs {
		partialResults := agg.GetPartialResult(aggCtxs[i])
		for _, result := range partialResults {
			data, err := codec.EncodeValue(e.evalCtx.sc, nil, result)
			if err != nil {
				return nil, errors.Trace(err)
			}
			value = append(value, data)
		}
	}
	value = append(value, e.groupKeyRows[e.currGroupIdx]...)
	e.currGroupIdx++

	return value, nil
}

func (e *hashAggExec) getGroupKey() ([]byte, [][]byte, error) {
	length := len(e.groupByExprs)
	if length == 0 {
		return nil, nil, nil
	}
	bufLen := 0
	row := make([][]byte, 0, length)
	for _, item := range e.groupByExprs {
		v, err := item.Eval(e.row)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		b, err := codec.EncodeValue(e.evalCtx.sc, nil, v)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		bufLen += len(b)
		row = append(row, b)
	}
	buf := make([]byte, 0, bufLen)
	for _, col := range row {
		buf = append(buf, col...)
	}
	return buf, row, nil
}

// aggregate updates aggregate functions with row.
func (e *hashAggExec) aggregate(value [][]byte) error {
	err := e.evalCtx.decodeRelatedColumnVals(e.relatedColOffsets, value, e.row)
	if err != nil {
		return errors.Trace(err)
	}
	// Get group key.
	gk, gbyKeyRow, err := e.getGroupKey()
	if err != nil {
		return errors.Trace(err)
	}
	if _, ok := e.groups[string(gk)]; !ok {
		e.groups[string(gk)] = struct{}{}
		e.groupKeys = append(e.groupKeys, gk)
		e.groupKeyRows = append(e.groupKeyRows, gbyKeyRow)
	}
	// Update aggregate expressions.
	aggCtxs := e.getContexts(gk)
	for i, agg := range e.aggExprs {
		err = agg.Update(aggCtxs[i], e.evalCtx.sc, e.row)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (e *hashAggExec) getContexts(groupKey []byte) []*aggregation.AggEvaluateContext {
	groupKeyString := string(groupKey)
	aggCtxs, ok := e.aggCtxsMap[groupKeyString]
	if !ok {
		aggCtxs = make([]*aggregation.AggEvaluateContext, 0, len(e.aggExprs))
		for _, agg := range e.aggExprs {
			aggCtxs = append(aggCtxs, agg.CreateContext(e.evalCtx.sc))
		}
		e.aggCtxsMap[groupKeyString] = aggCtxs
	}
	return aggCtxs
}

func (e *hashAggExec) nextChunk(ctx context.Context, chk *chunk.Chunk) error {
	return nil
}

func (e *hashAggExec) fieldTypes() []*types.FieldType {
	return e.tps
}

type streamAggExec struct {
	evalCtx           *evalContext
	aggExprs          []aggregation.Aggregation
	aggCtxs           []*aggregation.AggEvaluateContext
	groupByExprs      []expression.Expression
	relatedColOffsets []int
	row               types.DatumRow
	tmpGroupByRow     types.DatumRow
	currGroupByRow    types.DatumRow
	nextGroupByRow    types.DatumRow
	currGroupByValues [][]byte
	executed          bool
	hasData           bool
	count             int64

	src executor

	seCtx          sessionctx.Context
	tps            []*types.FieldType
	newAggFuncs    []aggfuncs.AggFunc
	partialResults []aggfuncs.PartialResult
	groupRows      []chunk.Row

	childResult *chunk.Chunk
	// isChildReturnEmpty indicates whether the child executor only returns an empty input.
	isChildReturnEmpty bool
	inputIter          *chunk.Iterator4Chunk
	inputRow           chunk.Row
}

func (e *streamAggExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *streamAggExec) GetSrcExec() executor {
	return e.src
}

func (e *streamAggExec) ResetCounts() {
	e.src.ResetCounts()
}

func (e *streamAggExec) Counts() []int64 {
	return e.src.Counts()
}

func (e *streamAggExec) getPartialResult() ([][]byte, error) {
	value := make([][]byte, 0, len(e.groupByExprs)+2*len(e.aggExprs))
	for i, agg := range e.aggExprs {
		partialResults := agg.GetPartialResult(e.aggCtxs[i])
		for _, result := range partialResults {
			data, err := codec.EncodeValue(e.evalCtx.sc, nil, result)
			if err != nil {
				return nil, errors.Trace(err)
			}
			value = append(value, data)
		}
		// Clear the aggregate context.
		e.aggCtxs[i] = agg.CreateContext(e.evalCtx.sc)
	}
	e.currGroupByValues = e.currGroupByValues[:0]
	for _, d := range e.currGroupByRow {
		buf, err := codec.EncodeValue(e.evalCtx.sc, nil, d)
		if err != nil {
			return nil, errors.Trace(err)
		}
		e.currGroupByValues = append(e.currGroupByValues, buf)
	}
	e.currGroupByRow = e.nextGroupByRow.Copy()
	return append(value, e.currGroupByValues...), nil
}

func (e *streamAggExec) meetNewGroup(row [][]byte) (bool, error) {
	if len(e.groupByExprs) == 0 {
		return false, nil
	}

	e.tmpGroupByRow = e.tmpGroupByRow[:0]
	matched, firstGroup := true, false
	if e.nextGroupByRow == nil {
		matched, firstGroup = false, true
	}
	for i, item := range e.groupByExprs {
		d, err := item.Eval(e.row)
		if err != nil {
			return false, errors.Trace(err)
		}
		if matched {
			c, err := d.CompareDatum(e.evalCtx.sc, &e.nextGroupByRow[i])
			if err != nil {
				return false, errors.Trace(err)
			}
			matched = c == 0
		}
		e.tmpGroupByRow = append(e.tmpGroupByRow, d)
	}
	if firstGroup {
		e.currGroupByRow = e.tmpGroupByRow.Copy()
	}
	if matched {
		return false, nil
	}
	e.nextGroupByRow = e.tmpGroupByRow
	return !firstGroup, nil
}

func (e *streamAggExec) Cursor() ([]byte, bool) {
	panic("don't not use coprocessor streaming API for stream aggregation!")
}

func (e *streamAggExec) Next(ctx context.Context) (retRow [][]byte, err error) {
	e.count++
	if e.executed {
		return nil, nil
	}

	for {
		values, err := e.src.Next(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if values == nil {
			e.executed = true
			if !e.hasData && len(e.groupByExprs) > 0 {
				return nil, nil
			}
			return e.getPartialResult()
		}

		e.hasData = true
		err = e.evalCtx.decodeRelatedColumnVals(e.relatedColOffsets, values, e.row)
		if err != nil {
			return nil, errors.Trace(err)
		}
		newGroup, err := e.meetNewGroup(values)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if newGroup {
			retRow, err = e.getPartialResult()
			if err != nil {
				return nil, errors.Trace(err)
			}
		}
		for i, agg := range e.aggExprs {
			err = agg.Update(e.aggCtxs[i], e.evalCtx.sc, e.row)
			if err != nil {
				return nil, errors.Trace(err)
			}
		}
		if newGroup {
			return retRow, nil
		}
	}
}

func (e *streamAggExec) nextChunk(ctx context.Context, chk *chunk.Chunk) error {
	chk.Reset()
	for !e.executed && chk.NumRows() < chunkMaxRows {
		if e.childResult == nil {
			e.childResult = chunk.NewChunkWithCapacity(e.src.fieldTypes(), 1024)
			e.isChildReturnEmpty = true
			e.inputIter = chunk.NewIterator4Chunk(e.childResult)
			e.inputRow = e.inputIter.End()
		}
		err := e.consumeOneGroup(ctx, chk)
		if err != nil {
			e.executed = true
			return errors.Trace(err)
		}
	}
	return nil
}

func (e *streamAggExec) fieldTypes() []*types.FieldType {
	return e.tps
}

func (e *streamAggExec) open(ctx context.Context) error {
	e.childResult = chunk.NewChunkWithCapacity(e.src.fieldTypes(), 1024)
	e.executed = false
	e.isChildReturnEmpty = true
	e.inputIter = chunk.NewIterator4Chunk(e.childResult)
	e.inputRow = e.inputIter.End()
	e.partialResults = make([]aggfuncs.PartialResult, 0, len(e.newAggFuncs))
	for _, newAggFunc := range e.newAggFuncs {
		e.partialResults = append(e.partialResults, newAggFunc.AllocPartialResult())
	}
	return nil
}

func (e *streamAggExec) consumeOneGroup(ctx context.Context, chk *chunk.Chunk) error {
	for !e.executed {
		if err := e.fetchChildIfNecessary(ctx, chk); err != nil {
			return errors.Trace(err)
		}
		for ; e.inputRow != e.inputIter.End(); e.inputRow = e.inputIter.Next() {
			meetNewGroup, err := e.meetNewGroupChunk(e.inputRow)
			if err != nil {
				return errors.Trace(err)
			}
			if meetNewGroup {
				err := e.consumeGroupRows()
				if err != nil {
					return errors.Trace(err)
				}
				err = e.appendResult2Chunk(chk)
				if err != nil {
					return errors.Trace(err)
				}
			}
			e.groupRows = append(e.groupRows, e.inputRow)
			if meetNewGroup {
				e.inputRow = e.inputIter.Next()
				return nil
			}
		}
	}
	return nil
}

func (e *streamAggExec) fetchChildIfNecessary(ctx context.Context, chk *chunk.Chunk) (err error) {
	if e.inputRow != e.inputIter.End() {
		return nil
	}

	// Before fetching a new batch of input, we should consume the last group.
	err = e.consumeGroupRows()
	if err != nil {
		return errors.Trace(err)
	}

	err = e.src.nextChunk(ctx, e.childResult)
	if err != nil {
		return errors.Trace(err)
	}

	// No more data.
	if e.childResult.NumRows() == 0 {
		if !e.isChildReturnEmpty {
			err = e.appendResult2Chunk(chk)
		}
		e.executed = true
		return errors.Trace(err)
	}
	// Reach here, "e.childrenResults[0].NumRows() > 0" is guaranteed.
	e.isChildReturnEmpty = false
	e.inputRow = e.inputIter.Begin()
	return nil
}

// appendResult2Chunk appends result of all the aggregation functions to the
// result chunk, and reset the evaluation context for each aggregation.
func (e *streamAggExec) appendResult2Chunk(chk *chunk.Chunk) error {
	for i := range e.currGroupByRow {
		chk.AppendDatum(i, &e.currGroupByRow[i])
	}
	for i, newAggFunc := range e.newAggFuncs {
		err := newAggFunc.AppendFinalResult2Chunk(e.seCtx, e.partialResults[i], chk)
		if err != nil {
			return errors.Trace(err)
		}
		newAggFunc.ResetPartialResult(e.partialResults[i])
	}
	if len(e.currGroupByRow)+len(e.newAggFuncs) == 0 {
		chk.SetNumVirtualRows(chk.NumRows() + 1)
	}
	return nil
}

func (e *streamAggExec) consumeGroupRows() error {
	if len(e.groupRows) == 0 {
		return nil
	}

	for i, newAggFunc := range e.newAggFuncs {
		err := newAggFunc.UpdatePartialResult(e.seCtx, e.groupRows, e.partialResults[i])
		if err != nil {
			return errors.Trace(err)
		}
	}
	e.groupRows = e.groupRows[:0]
	return nil
}

func (e *streamAggExec) meetNewGroupChunk(row chunk.Row) (bool, error) {
	// meetNewGroup returns a value that represents if the new group is different from last group.
	if len(e.groupByExprs) == 0 {
		return false, nil
	}
	e.tmpGroupByRow = e.tmpGroupByRow[:0]
	matched, firstGroup := true, false
	if len(e.currGroupByRow) == 0 {
		matched, firstGroup = false, true
	}
	for i, item := range e.groupByExprs {
		v, err := item.Eval(row)
		if err != nil {
			return false, errors.Trace(err)
		}
		if matched {
			c, err := v.CompareDatum(e.evalCtx.sc, &e.currGroupByRow[i])
			if err != nil {
				return false, errors.Trace(err)
			}
			matched = c == 0
		}
		e.tmpGroupByRow = append(e.tmpGroupByRow, v)
	}
	if matched {
		return false, nil
	}
	e.currGroupByRow = e.currGroupByRow[:0]
	for _, v := range e.tmpGroupByRow {
		e.currGroupByRow = append(e.currGroupByRow, *((&v).Copy()))
	}
	return !firstGroup, nil
}
