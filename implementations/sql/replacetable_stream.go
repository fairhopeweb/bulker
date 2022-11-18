package sql

import (
	"context"
	"errors"
	"fmt"
	"github.com/jitsucom/bulker/base/errorj"
	"github.com/jitsucom/bulker/base/timestamp"
	"github.com/jitsucom/bulker/bulker"
	"github.com/jitsucom/bulker/types"
	"github.com/joomcode/errorx"
)

type ReplaceTableStream struct {
	AbstractTransactionalSQLStream
}

func newReplaceTableStream(id string, p SQLAdapter, tableName string, streamOptions ...bulker.StreamOption) (bulker.BulkerStream, error) {
	ps := ReplaceTableStream{}

	var err error
	ps.AbstractTransactionalSQLStream, err = newAbstractTransactionalStream(id, p, tableName, bulker.ReplaceTable, streamOptions...)
	if err != nil {
		return nil, err
	}
	ps.tmpTableFunc = func(ctx context.Context, tableForObject *Table, batchFile bool) *Table {
		return &Table{
			Name:           fmt.Sprintf("%s_tmp_%s", ps.tableName, timestamp.Now().Format("060102_150405")),
			PrimaryKeyName: tableForObject.PrimaryKeyName,
			//PrimaryKeyName: fmt.Sprintf("%s_%s", tableForObject.PrimaryKeyName, timestamp.Now().Format("060102_150405")),
			PKFields: tableForObject.PKFields,
			Columns:  tableForObject.Columns,
		}
	}
	return &ps, nil
}

func (ps *ReplaceTableStream) Consume(ctx context.Context, object types.Object) (state bulker.State, processedObjects []types.Object, err error) {
	defer func() {
		err = ps.postConsume(err)
		state = ps.state
	}()
	if err = ps.init(ctx); err != nil {
		return
	}
	//type mapping, flattening => table schema
	tableForObject, processedObjects, err := ps.preprocess(object)
	if err != nil {
		ps.updateRepresentationTable(tableForObject)
		return
	}
	if ps.batchFile != nil {
		err = ps.writeToBatchFile(ctx, tableForObject, processedObjects)
	} else {
		err = ps.insert(ctx, tableForObject, processedObjects)
	}
	return
}

func (ps *ReplaceTableStream) Complete(ctx context.Context) (state bulker.State, err error) {
	if ps.state.Status != bulker.Active {
		return ps.state, errors.New("stream is not active")
	}
	defer func() {
		state, err = ps.postComplete(ctx, err)
	}()
	if ps.state.LastError == nil {
		//if at least one object was inserted
		if ps.state.SuccessfulRows > 0 {
			if ps.batchFile != nil {
				if err = ps.flushBatchFile(ctx); err != nil {
					return ps.state, err
				}
			}
			err = ps.tx.ReplaceTable(ctx, ps.tableName, ps.tmpTable, true)
			if errorx.IsOfType(err, errorj.DropError) {
				err = ps.tx.ReplaceTable(ctx, ps.tableName, ps.tmpTable, false)
			}
			if err != nil {
				return ps.state, err
			}
		} else {
			//when no objects were consumed. we need to replace table with empty one.
			//truncation seems like a more straightforward approach.
			//no transaction was opened yet and not needed that is  why we pass ps.sqlAdapter
			var table *Table
			table, err = ps.sqlAdapter.GetTableSchema(ctx, ps.tableName)
			if table.Exists() {
				err = ps.sqlAdapter.TruncateTable(ctx, ps.tableName)
			}
		}
		return
	} else {
		//if was any error - it will trigger transaction rollback in defer func
		err = ps.state.LastError
		return
	}
}
