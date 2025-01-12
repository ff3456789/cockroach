// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package mutations

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/stats"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
)

var (
	// StatisticsMutator adds ALTER TABLE INJECT STATISTICS statements.
	StatisticsMutator MultiStatementMutation = statisticsMutator

	// ForeignKeyMutator adds ALTER TABLE ADD FOREIGN KEY statements.
	ForeignKeyMutator MultiStatementMutation = foreignKeyMutator

	// ColumnFamilyMutator modifies a CREATE TABLE statement without any FAMILY
	// definitions to have random FAMILY definitions.
	ColumnFamilyMutator StatementMutator = sqlbase.ColumnFamilyMutator
)

// StatementMutator defines a func that can change a statement.
type StatementMutator func(rng *rand.Rand, stmt tree.Statement) (changed bool)

// MultiStatementMutation defines a func that can return a list of new and/or mutated statements.
type MultiStatementMutation func(rng *rand.Rand, stmts []tree.Statement) (mutated []tree.Statement, changed bool)

// Mutate implements the Mutator interface.
func (sm StatementMutator) Mutate(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	for _, stmt := range stmts {
		sc := sm(rng, stmt)
		changed = changed || sc
	}
	return stmts, changed
}

// Mutate implements the Mutator interface.
func (msm MultiStatementMutation) Mutate(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	return msm(rng, stmts)
}

// Apply executes all mutators on stmts. It returns the (possibly mutated and
// changed in place) statements and a boolean indicating whether any changes
// were made.
func Apply(
	rng *rand.Rand, stmts []tree.Statement, mutators ...sqlbase.Mutator,
) (mutated []tree.Statement, changed bool) {
	var mc bool
	for _, m := range mutators {
		stmts, mc = m.Mutate(rng, stmts)
		changed = changed || mc
	}
	return stmts, changed
}

// ApplyString executes all mutators on input.
func ApplyString(
	rng *rand.Rand, input string, mutators ...sqlbase.Mutator,
) (output string, changed bool) {
	parsed, err := parser.Parse(input)
	if err != nil {
		return input, false
	}

	stmts := make([]tree.Statement, len(parsed))
	for i, p := range parsed {
		stmts[i] = p.AST
	}

	stmts, changed = Apply(rng, stmts, mutators...)
	if !changed {
		return input, false
	}

	var sb strings.Builder
	for _, s := range stmts {
		sb.WriteString(s.String())
		sb.WriteString(";\n")
	}
	return sb.String(), true
}

// randNonNegInt returns a random non-negative integer. It attempts to
// distribute it over powers of 10.
func randNonNegInt(rng *rand.Rand) int64 {
	var v int64
	if n := rng.Intn(20); n == 0 {
		// v == 0
	} else if n <= 10 {
		v = rng.Int63n(10) + 1
		for i := 0; i < n; i++ {
			v *= 10
		}
	} else {
		v = rng.Int63()
	}
	return v
}

func statisticsMutator(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	for _, stmt := range stmts {
		create, ok := stmt.(*tree.CreateTable)
		if !ok {
			continue
		}
		alter := &tree.AlterTable{
			Table: create.Table.ToUnresolvedObjectName(),
		}
		rowCount := randNonNegInt(rng)
		cols := map[tree.Name]*tree.ColumnTableDef{}
		colStats := map[tree.Name]*stats.JSONStatistic{}
		makeHistogram := func(col *tree.ColumnTableDef) {
			// If an index appeared before a column definition, col
			// can be nil.
			if col == nil {
				return
			}
			n := rng.Intn(10)
			seen := map[string]bool{}
			h := stats.HistogramData{
				ColumnType: *col.Type,
			}
			for i := 0; i < n; i++ {
				upper := sqlbase.RandDatumWithNullChance(rng, col.Type, 0)
				if upper == tree.DNull {
					continue
				}
				enc, err := sqlbase.EncodeTableKey(nil, upper, encoding.Ascending)
				if err != nil {
					panic(err)
				}
				if es := string(enc); seen[es] {
					continue
				} else {
					seen[es] = true
				}
				numRange := randNonNegInt(rng)
				var distinctRange float64
				// distinctRange should be <= numRange.
				switch rng.Intn(3) {
				case 0:
					// 0
				case 1:
					distinctRange = float64(numRange)
				default:
					distinctRange = rng.Float64() * float64(numRange)
				}

				h.Buckets = append(h.Buckets, stats.HistogramData_Bucket{
					NumEq:         randNonNegInt(rng),
					NumRange:      numRange,
					DistinctRange: distinctRange,
					UpperBound:    enc,
				})
			}
			sort.Slice(h.Buckets, func(i, j int) bool {
				return bytes.Compare(h.Buckets[i].UpperBound, h.Buckets[j].UpperBound) < 0
			})
			stat := colStats[col.Name]
			if err := stat.SetHistogram(&h); err != nil {
				panic(err)
			}
		}
		for _, def := range create.Defs {
			switch def := def.(type) {
			case *tree.ColumnTableDef:
				var nullCount, distinctCount uint64
				if rowCount > 0 {
					if def.Nullable.Nullability != tree.NotNull {
						nullCount = uint64(rng.Int63n(rowCount))
					}
					distinctCount = uint64(rng.Int63n(rowCount))
				}
				cols[def.Name] = def
				colStats[def.Name] = &stats.JSONStatistic{
					Name:          "__auto__",
					CreatedAt:     "2000-01-01 00:00:00+00:00",
					RowCount:      uint64(rowCount),
					Columns:       []string{def.Name.String()},
					DistinctCount: distinctCount,
					NullCount:     nullCount,
				}
				if def.Unique || def.PrimaryKey {
					makeHistogram(def)
				}
			case *tree.IndexTableDef:
				makeHistogram(cols[def.Columns[0].Column])
			case *tree.UniqueConstraintTableDef:
				makeHistogram(cols[def.Columns[0].Column])
			}
		}
		if len(colStats) > 0 {
			var allStats []*stats.JSONStatistic
			for _, cs := range colStats {
				allStats = append(allStats, cs)
			}
			b, err := json.Marshal(allStats)
			if err != nil {
				// Should not happen.
				panic(err)
			}
			alter.Cmds = append(alter.Cmds, &tree.AlterTableInjectStats{
				Stats: tree.NewDString(string(b)),
			})
			stmts = append(stmts, alter)
			changed = true
		}
	}
	return stmts, changed
}

func foreignKeyMutator(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	// Find columns in the tables.
	cols := map[tree.TableName][]*tree.ColumnTableDef{}
	byName := map[tree.TableName]*tree.CreateTable{}

	// Keep track of referencing columns since we have a limitation that a
	// column can only be used by one FK.
	usedCols := map[tree.TableName]map[tree.Name]bool{}

	// Keep track of table dependencies to prevent circular dependencies.
	dependsOn := map[tree.TableName]map[tree.TableName]bool{}

	var tables []*tree.CreateTable
	for _, stmt := range stmts {
		table, ok := stmt.(*tree.CreateTable)
		if !ok {
			continue
		}
		tables = append(tables, table)
		byName[table.Table] = table
		usedCols[table.Table] = map[tree.Name]bool{}
		dependsOn[table.Table] = map[tree.TableName]bool{}
		for _, def := range table.Defs {
			switch def := def.(type) {
			case *tree.ColumnTableDef:
				cols[table.Table] = append(cols[table.Table], def)
			}
		}
	}

	toNames := func(cols []*tree.ColumnTableDef) tree.NameList {
		names := make(tree.NameList, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return names
	}

	// We cannot mutate the table definitions themselves because 1) we
	// don't know the order of dependencies (i.e., table 1 could reference
	// table 4 which doesn't exist yet) and relatedly 2) we don't prevent
	// circular dependencies. Instead, add new ALTER TABLE commands to the
	// end of a list of statements.

	// Create some FKs.
	for rng.Intn(2) == 0 {
		// Choose a random table.
		table := tables[rng.Intn(len(tables))]
		// Choose a random column subset.
		var fkCols []*tree.ColumnTableDef
		for _, c := range cols[table.Table] {
			if usedCols[table.Table][c.Name] {
				continue
			}
			fkCols = append(fkCols, c)
		}
		if len(fkCols) == 0 {
			continue
		}
		rng.Shuffle(len(fkCols), func(i, j int) {
			fkCols[i], fkCols[j] = fkCols[j], fkCols[i]
		})
		// Pick some randomly short prefix. I'm sure there's a closed
		// form solution to this with a single call to rng.Intn but I'm
		// not sure what to search for.
		i := 1
		for len(fkCols) > i && rng.Intn(2) == 0 {
			i++
		}
		fkCols = fkCols[:i]

		// Check if a table has the needed column types.
	LoopTable:
		for refTable, refCols := range cols {
			// Prevent circular and self references because
			// generating valid INSERTs could become impossible or
			// difficult algorithmically.
			if refTable == table.Table || len(refCols) < len(fkCols) {
				continue
			}

			{
				// To prevent circular references, find all transitive
				// dependencies of refTable and make sure none of them
				// are table.
				stack := []tree.TableName{refTable}
				for i := 0; i < len(stack); i++ {
					curTable := stack[i]
					if curTable == table.Table {
						// table was trying to add a dependency
						// to refTable, but refTable already
						// depends on table (directly or
						// indirectly).
						continue LoopTable
					}
					for t := range dependsOn[curTable] {
						stack = append(stack, t)
					}
				}
			}

			// We found a table with enough columns. Check if it
			// has some columns that are needed types. In order
			// to not use columns multiple times, keep track of
			// available columns.
			availCols := append([]*tree.ColumnTableDef(nil), refCols...)
			var usingCols []*tree.ColumnTableDef
			for len(availCols) > 0 && len(usingCols) < len(fkCols) {
				fkCol := fkCols[len(usingCols)]
				found := false
				for refI, refCol := range availCols {
					if fkCol.Type.Equivalent(refCol.Type) {
						usingCols = append(usingCols, refCol)
						availCols = append(availCols[:refI], availCols[refI+1:]...)
						found = true
						break
					}
				}
				if !found {
					continue LoopTable
				}
			}
			// If we didn't find enough columns, try another table.
			if len(usingCols) != len(fkCols) {
				continue
			}

			// Found a suitable table.
			// TODO(mjibson): prevent the creation of unneeded
			// unique indexes. One may already exist with the
			// correct prefix.
			ref := byName[refTable]
			refColumns := make(tree.IndexElemList, len(usingCols))
			for i, c := range usingCols {
				refColumns[i].Column = c.Name
			}
			for _, c := range fkCols {
				usedCols[table.Table][c.Name] = true
			}
			dependsOn[table.Table][ref.Table] = true
			ref.Defs = append(ref.Defs, &tree.UniqueConstraintTableDef{
				IndexTableDef: tree.IndexTableDef{
					Columns: refColumns,
				},
			})

			match := tree.MatchSimple
			// TODO(mjibson): Set match once #42498 is fixed.
			var actions tree.ReferenceActions
			if rng.Intn(2) == 0 {
				actions.Delete = randAction(rng, table)
			}
			if rng.Intn(2) == 0 {
				actions.Update = randAction(rng, table)
			}
			stmts = append(stmts, &tree.AlterTable{
				Table: table.Table.ToUnresolvedObjectName(),
				Cmds: tree.AlterTableCmds{&tree.AlterTableAddConstraint{
					ConstraintDef: &tree.ForeignKeyConstraintTableDef{
						Table:    ref.Table,
						FromCols: toNames(fkCols),
						ToCols:   toNames(usingCols),
						Actions:  actions,
						Match:    match,
					},
				}},
			})
			changed = true
			break
		}
	}

	return stmts, changed
}

func randAction(rng *rand.Rand, table *tree.CreateTable) tree.ReferenceAction {
	const highestAction = tree.Cascade
	// Find a valid action. Depending on the random action chosen, we have
	// to verify some validity conditions.
Loop:
	for {
		action := tree.ReferenceAction(rng.Intn(int(highestAction + 1)))
		for _, def := range table.Defs {
			col, ok := def.(*tree.ColumnTableDef)
			if !ok {
				continue
			}
			switch action {
			case tree.SetNull:
				if col.Nullable.Nullability == tree.NotNull {
					continue Loop
				}
			case tree.SetDefault:
				if col.DefaultExpr.Expr == nil && col.Nullable.Nullability == tree.NotNull {
					continue Loop
				}
			}
		}
		return action
	}
}
