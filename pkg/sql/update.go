// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"fmt"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/pkg/errors"
)

// editNode (Base, Run) is shared between all row updating
// statements (DELETE, UPDATE, INSERT).

// editNodeBase holds the common (prepare+execute) state needed to run
// row-modifying statements.
type editNodeBase struct {
	p          *planner
	rh         *returningHelper
	tableDesc  *sqlbase.TableDescriptor
	autoCommit bool
}

func (p *planner) makeEditNode(
	ctx context.Context, tn *parser.TableName, autoCommit bool, priv privilege.Kind,
) (editNodeBase, error) {
	tableDesc, err := p.session.leases.getTableLease(ctx, p.txn, p.getVirtualTabler(), tn)
	if err != nil {
		return editNodeBase{}, err
	}
	// We don't support update on views, only real tables.
	if !tableDesc.IsTable() {
		return editNodeBase{},
			errors.Errorf("cannot run %s on view %q - views are not updateable", priv, tn)
	}

	if err := p.CheckPrivilege(tableDesc, priv); err != nil {
		return editNodeBase{}, err
	}

	return editNodeBase{
		p:          p,
		tableDesc:  tableDesc,
		autoCommit: autoCommit,
	}, nil
}

// editNodeRun holds the runtime (execute) state needed to run
// row-modifying statements.
type editNodeRun struct {
	rows      planNode
	tw        tableWriter
	resultRow parser.Datums

	explain explainMode
}

func (r *editNodeRun) initEditNode(
	ctx context.Context,
	en *editNodeBase,
	rows planNode,
	re parser.ReturningClause,
	desiredTypes []parser.Type,
) error {
	r.rows = rows

	rh, err := en.p.newReturningHelper(ctx, re, desiredTypes, en.tableDesc.Name, en.tableDesc.Columns)
	if err != nil {
		return err
	}
	en.rh = rh

	return nil
}

func (r *editNodeRun) startEditNode(ctx context.Context, en *editNodeBase, tw tableWriter) error {
	if sqlbase.IsSystemConfigID(en.tableDesc.GetID()) {
		// Mark transaction as operating on the system DB.
		if err := en.p.txn.SetSystemConfigTrigger(); err != nil {
			return err
		}
	}

	r.tw = tw

	return r.rows.Start(ctx)
}

type updateNode struct {
	// The following fields are populated during makePlan.
	editNodeBase
	n             *parser.Update
	updateCols    []sqlbase.ColumnDescriptor
	updateColsIdx map[sqlbase.ColumnID]int // index in updateCols slice
	tw            tableUpdater
	checkHelper   checkHelper
	sourceSlots   []sourceSlot

	run struct {
		// The following fields are populated during Start().
		editNodeRun
	}
}

// This interface abstracts the idea that our update sources can either be
// tuples or scalars. Tuples are for cases such as SET (a, b) = (1, 2) or SET
// (a, b) = (SELECT a + b, a - b), and scalars are for situations like SET a =
// b. One sourceSlot represents how to extract and type-check the results of
// the right-hand side of a single SET statement. We could treat everything as
// tuples, including scalars as tuples of size 1, and eliminate this indirection,
// but that makes the query plan more complex.
type sourceSlot interface {
	// Returns a slice of the values this slot is responsible for, as extracted
	// from the row of results.
	extractValues(resultRow parser.Datums) parser.Datums
	// Compares the types of the results that this slot refers to to the types of
	// the columns those values will be assigned to.
	typeCheck(renderedResult parser.TypedExpr, pmap *parser.PlaceholderInfo) error
}

type tupleSlot struct {
	columns     []sqlbase.ColumnDescriptor
	sourceIndex int
}

func (ts tupleSlot) extractValues(row parser.Datums) parser.Datums {
	return row[ts.sourceIndex].(*parser.DTuple).D
}

func (ts tupleSlot) typeCheck(renderedResult parser.TypedExpr, pmap *parser.PlaceholderInfo) error {
	for i, typ := range renderedResult.ResolvedType().(parser.TTuple) {
		err := sqlbase.CheckColumnType(ts.columns[i], typ, pmap)
		if err != nil {
			return err
		}
	}
	return nil
}

type scalarSlot struct {
	column      sqlbase.ColumnDescriptor
	sourceIndex int
}

func (ss scalarSlot) extractValues(row parser.Datums) parser.Datums {
	return parser.Datums{row[ss.sourceIndex]}
}

func (ss scalarSlot) typeCheck(renderedResult parser.TypedExpr, pmap *parser.PlaceholderInfo) error {
	typ := renderedResult.ResolvedType()
	return sqlbase.CheckColumnType(ss.column, typ, pmap)
}

// Update updates columns for a selection of rows from a table.
// Privileges: UPDATE and SELECT on table. We currently always use a select statement.
//   Notes: postgres requires UPDATE. Requires SELECT with WHERE clause with table.
//          mysql requires UPDATE. Also requires SELECT with WHERE clause with table.
// TODO(guanqun): need to support CHECK in UPDATE
func (p *planner) Update(
	ctx context.Context, n *parser.Update, desiredTypes []parser.Type, autoCommit bool,
) (planNode, error) {
	tracing.AnnotateTrace()

	tn, err := p.getAliasedTableName(n.Table)
	if err != nil {
		return nil, err
	}

	en, err := p.makeEditNode(ctx, tn, autoCommit, privilege.UPDATE)
	if err != nil {
		return nil, err
	}

	setExprs := make([]*parser.UpdateExpr, len(n.Exprs))
	for i, expr := range n.Exprs {
		// Replace the sub-query nodes.
		newExpr, err := p.replaceSubqueries(ctx, expr.Expr, len(expr.Names))
		if err != nil {
			return nil, err
		}
		setExprs[i] = &parser.UpdateExpr{Tuple: expr.Tuple, Expr: newExpr, Names: expr.Names}
	}

	// Determine which columns we're inserting into.
	names, err := p.namesForExprs(setExprs)
	if err != nil {
		return nil, err
	}

	updateCols, err := p.processColumns(en.tableDesc, names)
	if err != nil {
		return nil, err
	}

	defaultExprs, err := sqlbase.MakeDefaultExprs(updateCols, &p.parser, &p.evalCtx)
	if err != nil {
		return nil, err
	}

	var requestedCols []sqlbase.ColumnDescriptor
	if _, retExprs := n.Returning.(*parser.ReturningExprs); retExprs || len(en.tableDesc.Checks) > 0 {
		// TODO(dan): This could be made tighter, just the rows needed for RETURNING
		// exprs.
		requestedCols = en.tableDesc.Columns
	}

	fkTables := sqlbase.TablesNeededForFKs(*en.tableDesc, sqlbase.CheckUpdates)
	if err := p.fillFKTableMap(ctx, fkTables); err != nil {
		return nil, err
	}
	ru, err := sqlbase.MakeRowUpdater(p.txn, en.tableDesc, fkTables, updateCols, requestedCols, sqlbase.RowUpdaterDefault)
	if err != nil {
		return nil, err
	}
	tw := tableUpdater{ru: ru, autoCommit: autoCommit}

	tracing.AnnotateTrace()

	// Generate the list of select targets. We need to select all of the columns
	// plus we select all of the update expressions in case those expressions
	// reference columns (e.g. "UPDATE t SET v = v + 1").
	targets := sqlbase.ColumnsSelectors(ru.FetchCols)
	sourceSlots := make([]sourceSlot, 0, len(setExprs))
	targetColumnIndex := 0
	// Remember the index where the targets for exprs start.
	exprTargetIdx := len(targets)
	desiredTypesFromSelect := make([]parser.Type, len(targets), len(targets)+len(setExprs))
	for i := range targets {
		desiredTypesFromSelect[i] = parser.TypeAny
	}
	for setIndex, setExpr := range setExprs {
		if setExpr.Tuple {
			desiredTupleType := make(parser.TTuple, 0, len(setExpr.Names))
			for j := range setExpr.Names {
				desiredTupleType = append(desiredTupleType, updateCols[targetColumnIndex+j].Type.ToDatumType())
			}
			tupleSize := -1

			if t, ok := setExpr.Expr.(*parser.Tuple); ok {
				// The user assigned an explicit set of values to the columns. We can't
				// treat this case the same as when we have a subquery (and just evaluate
				// the tuple) because when assigning a literal tuple like this it's valid
				// to assign DEFAULT to some of the columns, which is not valid generally.
				tupleSize = len(t.Exprs)

				tupleQuery := make([]parser.Expr, 0, len(t.Exprs))
				for i, e := range t.Exprs {
					e = fillDefault(e, targetColumnIndex+i, defaultExprs)
					tupleQuery = append(tupleQuery, e)
				}
				targets = append(targets, parser.SelectExpr{Expr: &parser.Tuple{Exprs: tupleQuery}})
			} else {
				// There's no explicit tuple being assigned, so there's a subquery, and we
				// need to make sure that subquery returns a tuple.
				typedExpr, err := setExpr.Expr.TypeCheck(&p.semaCtx, desiredTupleType)
				if err != nil {
					return nil, err
				}
				if t, ok := typedExpr.ResolvedType().(parser.TTuple); ok {
					tupleSize = len(t)
					targets = append(targets, parser.SelectExpr{Expr: setExpr.Expr})
				}
			}

			if tupleSize > 0 {
				sourceSlots = append(sourceSlots, tupleSlot{
					columns:     updateCols[targetColumnIndex : targetColumnIndex+tupleSize],
					sourceIndex: setIndex,
				})

				// We determine what we expect the type of the values to be based on the columns
				// being assigned to.
				desiredTupleType := make(parser.TTuple, 0, tupleSize)
				for j := 0; j < tupleSize; j++ {
					typ := updateCols[targetColumnIndex].Type.ToDatumType()
					desiredTupleType = append(desiredTupleType, typ)
					targetColumnIndex++
				}
				desiredTypesFromSelect = append(desiredTypesFromSelect, desiredTupleType)
			} else {
				return nil, fmt.Errorf("cannot use this expression to assign multiple columns: %s", setExpr.Expr)
			}
		} else {
			typ := updateCols[targetColumnIndex].Type.ToDatumType()
			e := fillDefault(setExpr.Expr, targetColumnIndex, defaultExprs)
			targets = append(targets, parser.SelectExpr{Expr: e})
			sourceSlots = append(sourceSlots, scalarSlot{
				column:      updateCols[targetColumnIndex],
				sourceIndex: setIndex,
			})
			desiredTypesFromSelect = append(desiredTypesFromSelect, typ)
			targetColumnIndex++
		}
	}

	rows, err := p.SelectClause(ctx, &parser.SelectClause{
		Exprs: targets,
		From:  &parser.From{Tables: []parser.TableExpr{n.Table}},
		Where: n.Where,
	}, nil, nil, desiredTypesFromSelect, publicAndNonPublicColumns)
	if err != nil {
		return nil, err
	}

	// Placeholders have their types populated in the above Select if they are part
	// of an expression ("SET a = 2 + $1") in the type check step where those
	// types are inferred. For the simpler case ("SET a = $1"), populate them
	// using checkColumnType. This step also verifies that the expression
	// types match the column types.
	sel := rows.(*renderNode)
	sourceResults := sel.render[exprTargetIdx:]
	for i, sourceSlot := range sourceSlots {
		err = sourceSlot.typeCheck(sourceResults[i], &p.semaCtx.Placeholders)
		if err != nil {
			return nil, err
		}
	}

	updateColsIdx := make(map[sqlbase.ColumnID]int, len(ru.UpdateCols))
	for i, col := range ru.UpdateCols {
		updateColsIdx[col.ID] = i
	}

	un := &updateNode{
		n:             n,
		editNodeBase:  en,
		updateCols:    ru.UpdateCols,
		updateColsIdx: updateColsIdx,
		tw:            tw,
		sourceSlots:   sourceSlots,
	}
	if err := un.checkHelper.init(ctx, p, tn, en.tableDesc); err != nil {
		return nil, err
	}
	if err := un.run.initEditNode(
		ctx, &un.editNodeBase, rows, n.Returning, desiredTypes); err != nil {
		return nil, err
	}
	return un, nil
}

func (u *updateNode) Start(ctx context.Context) error {
	if err := u.run.startEditNode(ctx, &u.editNodeBase, &u.tw); err != nil {
		return err
	}
	return u.run.tw.init(u.p.txn)
}

func (u *updateNode) Close(ctx context.Context) {
	u.run.rows.Close(ctx)
}

func (u *updateNode) Next(ctx context.Context) (bool, error) {
	next, err := u.run.rows.Next(ctx)
	if !next {
		if err == nil {
			// We're done. Finish the batch.
			err = u.tw.finalize(ctx)
		}
		return false, err
	}

	if u.run.explain == explainDebug {
		return true, nil
	}

	tracing.AnnotateTrace()

	entireRow := u.run.rows.Values()

	// Our updated value expressions occur immediately after the plain
	// columns in the output.
	oldValues := entireRow[:len(u.tw.ru.FetchCols)]

	updateValues := make(parser.Datums, 0, len(oldValues))
	sources := entireRow[len(u.tw.ru.FetchCols):]
	for _, slot := range u.sourceSlots {
		updateValues = append(updateValues, slot.extractValues(sources)...)
	}

	u.checkHelper.loadRow(u.tw.ru.FetchColIDtoRowIndex, oldValues, false)
	u.checkHelper.loadRow(u.updateColsIdx, updateValues, true)
	if err := u.checkHelper.check(&u.p.evalCtx); err != nil {
		return false, err
	}

	// Ensure that the values honor the specified column widths.
	for i := range updateValues {
		if err := sqlbase.CheckValueWidth(u.tw.ru.UpdateCols[i], updateValues[i]); err != nil {
			return false, err
		}
	}

	// Update the row values.
	for i, col := range u.tw.ru.UpdateCols {
		val := updateValues[i]
		if !col.Nullable && val == parser.DNull {
			return false, sqlbase.NewNonNullViolationError(col.Name)
		}
	}

	newValues, err := u.tw.row(ctx, append(oldValues, updateValues...))
	if err != nil {
		return false, err
	}

	resultRow, err := u.rh.cookResultRow(newValues)
	if err != nil {
		return false, err
	}
	u.run.resultRow = resultRow

	return true, nil
}

// namesForExprs expands names in the tuples and subqueries in exprs.
func (p *planner) namesForExprs(exprs parser.UpdateExprs) (parser.UnresolvedNames, error) {
	var names parser.UnresolvedNames
	for _, expr := range exprs {
		if expr.Tuple {
			n := -1
			switch t := expr.Expr.(type) {
			case *subquery:
				if tup, ok := t.typ.(parser.TTuple); ok {
					n = len(tup)
				}
			case *parser.Tuple:
				n = len(t.Exprs)
			case *parser.DTuple:
				n = len(t.D)
			}
			if n < 0 {
				return nil, errors.Errorf("unsupported tuple assignment: %T", expr.Expr)
			}
			if len(expr.Names) != n {
				return nil, fmt.Errorf("number of columns (%d) does not match number of values (%d)",
					len(expr.Names), n)
			}
		}
		names = append(names, expr.Names...)
	}
	return names, nil
}

func fillDefault(
	expr parser.Expr, index int, defaultExprs []parser.TypedExpr,
) parser.Expr {
	switch expr.(type) {
	case parser.DefaultVal:
		return defaultExprs[index]
	}
	return expr
}

func (u *updateNode) Columns() ResultColumns {
	return u.rh.columns
}

func (u *updateNode) Values() parser.Datums {
	return u.run.resultRow
}

func (u *updateNode) MarkDebug(mode explainMode) {
	if mode != explainDebug {
		panic(fmt.Sprintf("unknown debug mode %d", mode))
	}
	u.run.explain = mode
	u.run.rows.MarkDebug(mode)
}

func (u *updateNode) DebugValues() debugValues {
	return u.run.rows.DebugValues()
}

func (u *updateNode) Ordering() orderingInfo { return orderingInfo{} }
