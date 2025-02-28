package sql

import (
	"context"
	"errors"
	"fmt"
	bulker "github.com/jitsucom/bulker/bulkerlib"
	"github.com/jitsucom/bulker/bulkerlib/types"
	"github.com/jitsucom/bulker/jitsubase/errorj"
	"github.com/jitsucom/bulker/jitsubase/uuid"
)

// TODO: Use real temporary tables
type TransactionalStream struct {
	AbstractTransactionalSQLStream
}

func newTransactionalStream(id string, p SQLAdapter, tableName string, streamOptions ...bulker.StreamOption) (bulker.BulkerStream, error) {
	ps := TransactionalStream{}
	var err error
	ps.AbstractTransactionalSQLStream, err = newAbstractTransactionalStream(id, p, tableName, bulker.Batch, streamOptions...)
	if err != nil {
		return nil, err
	}
	ps.tmpTableFunc = func(ctx context.Context, tableForObject *Table, object types.Object) (table *Table) {
		dstTable := tableForObject
		existingTable, _ := ps.tx.GetTableSchema(ctx, ps.tableName)
		if existingTable.Exists() {
			dstTable = existingTable
			ps.adjustTableColumnTypes(dstTable, tableForObject, object)
		}

		tmpTableName := fmt.Sprintf("jitsu_tmp_%s", uuid.NewLettersNumbers()[:8])
		pkName := ""
		if len(dstTable.PKFields) > 0 {
			pkName = BuildConstraintName(tmpTableName)
		}
		return &Table{
			Name:            tmpTableName,
			Columns:         dstTable.Columns,
			PKFields:        dstTable.PKFields,
			PrimaryKeyName:  pkName,
			Temporary:       true,
			TimestampColumn: tableForObject.TimestampColumn,
		}
	}
	return &ps, nil
}

func (ps *TransactionalStream) init(ctx context.Context) (err error) {
	if ps.inited {
		return nil
	}
	return ps.AbstractTransactionalSQLStream.init(ctx)
}

func (ps *TransactionalStream) Complete(ctx context.Context) (state bulker.State, err error) {
	if ps.state.Status != bulker.Active {
		return ps.state, errors.New("stream is not active")
	}
	defer func() {
		state, err = ps.postComplete(ctx, err)
	}()
	//if at least one object was inserted
	if ps.state.SuccessfulRows > 0 {
		if ps.batchFile != nil {
			if err = ps.flushBatchFile(ctx); err != nil {
				return ps.state, err
			}
		}
		var dstTable *Table
		dstTable, err = ps.sqlAdapter.TableHelper().EnsureTableWithoutCaching(ctx, ps.tx, ps.id, ps.dstTable)
		if err != nil {
			ps.updateRepresentationTable(ps.dstTable)
			return ps.state, errorj.Decorate(err, "failed to ensure destination table")
		}
		ps.dstTable = dstTable
		ps.updateRepresentationTable(ps.dstTable)
		//copy data from tmp table to destination table
		err = ps.tx.CopyTables(ctx, ps.dstTable, ps.tmpTable, ps.merge)
		if err != nil {
			return ps.state, err
		}
		return
	} else {
		//if was any error - it will trigger transaction rollback in defer func
		err = ps.state.LastError
		return
	}
}
