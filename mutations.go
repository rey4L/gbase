package gbase

import (
	"bytes"
	"math"
	"reflect"
	"sort"

	"github.com/rey4L/gbase/internal/sql"
	"github.com/rey4L/gbase/internal/storage"
)

type storedRow struct {
	id     int64
	values []Value
}

func checkParameters(stmt sql.Statement, n int) error {
	required := sql.ParameterCount(stmt)
	if n != required {
		return fail("parameter", "expected %d parameters, got %d", required, n)
	}
	return nil
}

func (tx *Tx) execute(stmt sql.Statement, args []Value) (Result, error) {
	switch s := stmt.(type) {
	case *sql.CreateTable:
		return Result{}, tx.createTable(s)
	case *sql.DropTable:
		return Result{}, tx.dropTable(s)
	case *sql.CreateIndex:
		return Result{}, tx.createIndex(s)
	case *sql.DropIndex:
		return Result{}, tx.dropIndex(s)
	case *sql.AlterTable:
		return Result{}, tx.alterTable(s)
	case *sql.Insert:
		return tx.insert(s, args)
	case *sql.Update:
		return tx.update(s, args)
	case *sql.Delete:
		return tx.deleteRows(s, args)
	default:
		return Result{}, fail("statement", "Exec requires schema or mutation statement")
	}
}

func (tx *Tx) createTable(s *sql.CreateTable) error {
	name := canon(s.Name)
	if tx.cat.Tables[name] != nil {
		if s.IfNotExists {
			return nil
		}
		return fail("schema", "table %s exists", s.Name)
	}
	if tx.cat.Indexes[name] != nil {
		return fail("schema", "name %s already used", s.Name)
	}
	if len(s.Columns) == 0 {
		return fail("schema", "table needs columns")
	}
	t := &table{Name: s.Name, NextID: 1}
	seen := map[string]bool{}
	for _, d := range s.Columns {
		n := canon(d.Name)
		if seen[n] {
			return fail("schema", "duplicate column %s", d.Name)
		}
		seen[n] = true
		c, e := columnFromDef(d)
		if e != nil {
			return e
		}
		t.Columns = append(t.Columns, c)
	}
	for _, con := range s.Constraints {
		i := t.col(con.Column)
		if i < 0 {
			return fail("schema", "constraint column %s missing", con.Column)
		}
		switch con.Kind {
		case "PRIMARY KEY":
			t.Columns[i].Primary = true
		case "UNIQUE":
			if len(con.Columns) == 0 {
				t.Columns[i].Unique = true
			}
		case "FOREIGN KEY":
			if t.Columns[i].Ref != nil {
				return fail("schema", "duplicate foreign key on %s", con.Column)
			}
			if con.References == nil {
				return fail("schema", "missing foreign key target")
			}
			t.Columns[i].Ref = &reference{con.References.Table, con.References.Column}
		}
	}
	pk := 0
	for i := range t.Columns {
		if t.Columns[i].Primary {
			pk++
			t.Columns[i].Unique = true
			t.Columns[i].NotNull = true
		}
	}
	if pk > 1 {
		return fail("schema", "only one primary key supported")
	}
	root, e := storage.CreateTree(tx.pages)
	if e != nil {
		return e
	}
	t.Root = root
	tx.cat.Tables[name] = t
	for _, c := range t.Columns {
		if e = tx.checkReference(c); e != nil {
			return e
		}
	}
	var constraints [][]string
	for _, c := range t.Columns {
		if c.Unique && !(c.Primary && c.Type == "INTEGER") {
			constraints = append(constraints, []string{c.Name})
		}
	}
	for _, con := range s.Constraints {
		if len(con.Columns) > 0 {
			constraints = append(constraints, con.Columns)
		}
	}
	for _, cols := range constraints {
		n := automaticIndexName(t, cols)
		if tx.cat.Indexes[n] != nil || tx.cat.Tables[n] != nil {
			return fail("schema", "reserved index name collision")
		}
		if e = tx.addIndex(n, t, cols, true, true); e != nil {
			return e
		}
	}
	return nil
}

// automaticIndexName names the index backing a UNIQUE constraint.
func automaticIndexName(t *table, cols []string) string {
	n := "__gbase_" + canon(t.Name)
	for _, c := range cols {
		n += "_" + canon(c)
	}
	return n
}

func columnFromDef(d sql.ColumnDef) (column, error) {
	c := column{Name: d.Name, Type: d.Type, Primary: d.PrimaryKey, Unique: d.Unique, NotNull: d.NotNull}
	if d.Default != nil {
		v, e := eval(d.Default, evalEnv{}, nil)
		if e != nil {
			return c, e
		}
		v, e = coerce(c, v)
		if e != nil {
			return c, e
		}
		c.Default, e = encodeRecord([]Value{v})
		if e != nil {
			return c, e
		}
	}
	if d.References != nil {
		c.Ref = &reference{d.References.Table, d.References.Column}
	}
	return c, nil
}

func (tx *Tx) checkReference(c column) error {
	if c.Ref == nil {
		return nil
	}
	parent, e := tx.getTable(c.Ref.Table)
	if e != nil {
		return e
	}
	i := parent.col(c.Ref.Column)
	if i < 0 || !tx.uniqueColumn(parent, c.Ref.Column) {
		return fail("constraint", "foreign key must reference primary or unique column")
	}
	if parent.Columns[i].Type != c.Type {
		return fail("constraint", "foreign key types differ")
	}
	return nil
}

func (tx *Tx) dropTable(s *sql.DropTable) error {
	t, e := tx.getTable(s.Name)
	if e != nil {
		if s.IfExists {
			return nil
		}
		return e
	}
	for _, child := range tx.cat.Tables {
		if child == t {
			continue
		}
		for _, c := range child.Columns {
			if c.Ref != nil && canon(c.Ref.Table) == canon(t.Name) {
				return fail("constraint", "table %s referenced by %s", t.Name, child.Name)
			}
		}
	}
	for name, i := range tx.cat.Indexes {
		if canon(i.Table) == canon(t.Name) {
			tr := storage.Tree{Tx: tx.pages, Root: i.Root}
			if e = tr.Destroy(); e != nil {
				return e
			}
			delete(tx.cat.Indexes, name)
		}
	}
	tr := storage.Tree{Tx: tx.pages, Root: t.Root}
	if e = tr.Destroy(); e != nil {
		return e
	}
	delete(tx.cat.Tables, canon(t.Name))
	return nil
}

func (tx *Tx) createIndex(s *sql.CreateIndex) error {
	n := canon(s.Name)
	if tx.cat.Indexes[n] != nil {
		if s.IfNotExists {
			return nil
		}
		return fail("schema", "index exists")
	}
	if tx.cat.Tables[n] != nil {
		return fail("schema", "name already used")
	}
	if len(n) >= 8 && n[:8] == "__gbase_" {
		return fail("schema", "reserved index name")
	}
	t, e := tx.getTable(s.Table)
	if e != nil {
		return e
	}
	return tx.addIndex(s.Name, t, s.Columns, s.Unique, false)
}

func (tx *Tx) addIndex(name string, t *table, cols []string, unique, automatic bool) error {
	idx := &index{Name: name, Table: t.Name, Unique: unique, Automatic: automatic}
	seen := map[string]bool{}
	for _, col := range cols {
		p := t.col(col)
		if p < 0 {
			return fail("schema", "unknown index column %s", col)
		}
		if seen[canon(col)] {
			return fail("schema", "duplicate index column %s", col)
		}
		seen[canon(col)] = true
		idx.Columns = append(idx.Columns, t.Columns[p].Name)
	}
	idx.Column = idx.Columns[0]
	if len(idx.Columns) == 1 {
		idx.Columns = nil
	}
	root, e := storage.CreateTree(tx.pages)
	if e != nil {
		return e
	}
	idx.Root = root
	tx.cat.Indexes[canon(name)] = idx
	rows, e := tx.scanTable(t)
	if e != nil {
		return e
	}
	for _, r := range rows {
		if e = tx.indexInsert(t, idx, r.id, r.values); e != nil {
			return e
		}
	}
	return nil
}

func (tx *Tx) dropIndex(s *sql.DropIndex) error {
	i := tx.cat.Indexes[canon(s.Name)]
	if i == nil {
		if s.IfExists {
			return nil
		}
		return fail("schema", "unknown index")
	}
	if i.Automatic {
		return fail("constraint", "cannot drop constraint index")
	}
	if i.Unique && len(i.Columns) == 0 {
		parent := tx.cat.Tables[canon(i.Table)]
		col := parent.col(i.Column)
		redundant := parent.Columns[col].Primary || parent.Columns[col].Unique
		for _, other := range tx.indexes(parent) {
			if other != i && other.Unique && other.single(i.Column) {
				redundant = true
			}
		}
		if !redundant {
			for _, child := range tx.cat.Tables {
				for _, c := range child.Columns {
					if c.Ref != nil && canon(c.Ref.Table) == canon(i.Table) && canon(c.Ref.Column) == canon(i.Column) {
						return fail("constraint", "index %s backs foreign key", i.Name)
					}
				}
			}
		}
	}
	tr := storage.Tree{Tx: tx.pages, Root: i.Root}
	if e := tr.Destroy(); e != nil {
		return e
	}
	delete(tx.cat.Indexes, canon(s.Name))
	return nil
}

func coerce(c column, v Value) (Value, error) {
	if v == nil {
		if c.NotNull {
			return nil, fail("constraint", "%s cannot be NULL", c.Name)
		}
		return nil, nil
	}
	switch c.Type {
	case "INTEGER":
		if _, ok := v.(int64); ok {
			return v, nil
		}
	case "REAL":
		switch x := v.(type) {
		case int64:
			return float64(x), nil
		case float64:
			return x, nil
		}
	case "TEXT":
		if _, ok := v.(string); ok {
			return v, nil
		}
	case "BLOB":
		if _, ok := v.([]byte); ok {
			return v, nil
		}
	}
	return nil, fail("type", "%s requires %s", c.Name, c.Type)
}

func (tx *Tx) scanTable(t *table) ([]storedRow, error) {
	tr := storage.Tree{Tx: tx.pages, Root: t.Root}
	c, e := tr.Scan(nil, nil)
	if e != nil {
		return nil, e
	}
	var out []storedRow
	for c.Next() {
		if e = tx.ctx.Err(); e != nil {
			return nil, e
		}
		id, e := keyRow(c.Key())
		if e != nil {
			return nil, e
		}
		v, e := decodeRecord(c.Value())
		if e != nil {
			return nil, e
		}
		if len(v) != len(t.Columns) {
			return nil, fail("corrupt", "row width differs from schema")
		}
		out = append(out, storedRow{id, v})
	}
	return out, c.Err()
}

// mutationProgram builds the access path for UPDATE and DELETE, which reuse SELECT's index choice.
// where must already be bound to t.
func (tx *Tx) mutationProgram(t *table, where sql.Expr, args []Value) *queryProgram {
	p := &queryProgram{stmt: &sql.Select{From: sql.TableRef{Name: t.Name}, Where: where}, env: rowEnv(t, nil)}
	p.chooseIndex(tx, args)
	return p
}

// scanWhere returns a superset of the rows matching where, seeking through an index when one applies.
// Callers still evaluate the full predicate on every returned row.
func (tx *Tx) scanWhere(t *table, where sql.Expr, args []Value) ([]storedRow, error) {
	p := tx.mutationProgram(t, where, args)
	if p.idx == nil {
		return tx.scanTable(t)
	}
	table := storage.Tree{Tx: tx.pages, Root: t.Root}
	source := table
	if !p.primary {
		source = storage.Tree{Tx: tx.pages, Root: p.idx.Root}
	}
	c, e := source.Scan(p.start, p.end)
	if e != nil {
		return nil, e
	}
	var out []storedRow
	for c.Next() {
		if e = tx.ctx.Err(); e != nil {
			return nil, e
		}
		key, data := c.Key(), c.Value()
		if !p.primary {
			key = data
			var ok bool
			if data, ok, e = table.Get(key); e != nil {
				return nil, e
			} else if !ok {
				return nil, fail("corrupt", "dangling index entry")
			}
		}
		id, e := keyRow(key)
		if e != nil {
			return nil, e
		}
		v, e := decodeRecord(data)
		if e != nil {
			return nil, e
		}
		if len(v) != len(t.Columns) {
			return nil, fail("corrupt", "row width differs from schema")
		}
		out = append(out, storedRow{id, v})
	}
	return out, c.Err()
}

func (tx *Tx) indexes(t *table) []*index {
	var out []*index
	for _, i := range tx.cat.Indexes {
		if canon(i.Table) == canon(t.Name) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (tx *Tx) indexInsert(t *table, i *index, id int64, values []Value) error {
	p, hasNull, e := indexKey(t, i, values)
	if e != nil {
		return e
	}
	tr := storage.Tree{Tx: tx.pages, Root: i.Root}
	if i.Unique && !hasNull {
		c, e := tr.Scan(p, prefixEnd(p))
		if e != nil {
			return e
		}
		if c.Next() {
			return fail("constraint", "unique index %s violated", i.Name)
		}
		if e = c.Err(); e != nil {
			return e
		}
	}
	if e = tr.Put(append(p, rowKey(id)...), rowKey(id)); e != nil {
		return e
	}
	i.Root = tr.Root
	return nil
}

func (tx *Tx) indexDelete(t *table, i *index, id int64, values []Value) error {
	p, _, e := indexKey(t, i, values)
	if e != nil {
		return e
	}
	tr := storage.Tree{Tx: tx.pages, Root: i.Root}
	if e = tr.Delete(append(p, rowKey(id)...)); e != nil {
		return e
	}
	i.Root = tr.Root
	return nil
}

func (tx *Tx) putRow(t *table, id int64, v []Value) error {
	data, e := encodeRecord(v)
	if e != nil {
		return e
	}
	tr := storage.Tree{Tx: tx.pages, Root: t.Root}
	if _, ok, e := tr.Get(rowKey(id)); e != nil {
		return e
	} else if ok {
		return fail("constraint", "duplicate primary key")
	}
	if e = tr.Put(rowKey(id), data); e != nil {
		return e
	}
	t.Root = tr.Root
	for _, i := range tx.indexes(t) {
		if e = tx.indexInsert(t, i, id, v); e != nil {
			return e
		}
	}
	return nil
}

func (tx *Tx) removeRow(t *table, r storedRow) error {
	for _, i := range tx.indexes(t) {
		if e := tx.indexDelete(t, i, r.id, r.values); e != nil {
			return e
		}
	}
	tr := storage.Tree{Tx: tx.pages, Root: t.Root}
	if e := tr.Delete(rowKey(r.id)); e != nil {
		return e
	}
	t.Root = tr.Root
	return nil
}

func (tx *Tx) insert(s *sql.Insert, args []Value) (Result, error) {
	t, e := tx.getTable(s.Table)
	if e != nil {
		return Result{}, e
	}
	mapping := make([]int, len(t.Columns))
	for i := range mapping {
		mapping[i] = i
	}
	if len(s.Columns) > 0 {
		mapping = make([]int, len(s.Columns))
		seen := map[int]bool{}
		for i, n := range s.Columns {
			p := t.col(n)
			if p < 0 {
				return Result{}, fail("schema", "unknown column %s", n)
			}
			if seen[p] {
				return Result{}, fail("schema", "duplicate insert column")
			}
			seen[p] = true
			mapping[i] = p
		}
	}
	var result Result
	var inserted [][]Value
	for _, exprs := range s.Rows {
		if e = tx.ctx.Err(); e != nil {
			return Result{}, e
		}
		if len(exprs) != len(mapping) {
			return Result{}, fail("schema", "insert value count differs")
		}
		values := make([]Value, len(t.Columns))
		for i, c := range t.Columns {
			if c.Default != nil {
				v, e := decodeRecord(c.Default)
				if e != nil {
					return Result{}, e
				}
				values[i] = v[0]
			}
		}
		for i, x := range exprs {
			v, e := eval(x, evalEnv{}, args)
			if e != nil {
				return Result{}, e
			}
			values[mapping[i]] = v
		}
		id := t.NextID
		explicitID := false
		for i, c := range t.Columns {
			if c.Primary && c.Type == "INTEGER" {
				if values[i] != nil {
					explicitID = true
					var ok bool
					id, ok = values[i].(int64)
					if !ok {
						return Result{}, fail("type", "integer primary key required")
					}
				} else {
					values[i] = id
				}
			}
			values[i], e = coerce(c, values[i])
			if e != nil {
				return Result{}, e
			}
		}
		if t.Exhausted && !explicitID {
			return Result{}, fail("limit", "row identifier exhausted")
		}
		if id >= t.NextID {
			if id == math.MaxInt64 {
				t.NextID = id
				t.Exhausted = true
			} else {
				t.NextID = id + 1
			}
		}

		if e = tx.putRow(t, id, values); e != nil {
			return Result{}, e
		}
		inserted = append(inserted, values)
		result.RowsAffected++
		result.LastInsertID = id
	}
	for _, values := range inserted {
		if e = tx.foreignKeys(t, values); e != nil {
			return Result{}, e
		}
	}
	return result, nil
}

func (tx *Tx) uniqueColumn(t *table, name string) bool {
	p := t.col(name)
	if p < 0 {
		return false
	}
	if t.Columns[p].Primary || t.Columns[p].Unique {
		return true
	}
	for _, idx := range tx.indexes(t) {
		if idx.Unique && idx.single(name) {
			return true
		}
	}
	return false
}

func (tx *Tx) referencedValue(t *table, name string, value Value) (bool, error) {
	p := t.col(name)
	if p < 0 {
		return false, fail("corrupt", "missing foreign column")
	}
	if t.Columns[p].Primary && t.Columns[p].Type == "INTEGER" {
		id, ok := value.(int64)
		if !ok {
			return false, fail("type", "foreign key requires integer")
		}
		tr := storage.Tree{Tx: tx.pages, Root: t.Root}
		_, found, err := tr.Get(rowKey(id))
		return found, err
	}
	for _, idx := range tx.indexes(t) {
		if !idx.Unique || !idx.single(name) {
			continue
		}
		prefix, err := indexPrefix(value)
		if err != nil {
			return false, err
		}
		tr := storage.Tree{Tx: tx.pages, Root: idx.Root}
		cursor, err := tr.Scan(prefix, prefixEnd(prefix))
		if err != nil {
			return false, err
		}
		found := cursor.Next()
		return found, cursor.Err()
	}
	return false, fail("corrupt", "foreign key target lacks unique index")
}

func (tx *Tx) foreignKeys(t *table, values []Value) error {
	for p, c := range t.Columns {
		if c.Ref == nil || values[p] == nil {
			continue
		}
		parent, err := tx.getTable(c.Ref.Table)
		if err != nil {
			return err
		}
		found, err := tx.referencedValue(parent, c.Ref.Column, values[p])
		if err != nil {
			return err
		}
		if !found {
			return fail("constraint", "foreign key %s.%s violated", t.Name, c.Name)
		}
	}
	return nil
}

func sameValue(a, b Value) bool {
	if x, ok := a.([]byte); ok {
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return reflect.DeepEqual(a, b)
}

func (tx *Tx) restrict(t *table, old storedRow, newValues []Value) error {
	for _, child := range tx.cat.Tables {
		for p, c := range child.Columns {
			if c.Ref == nil || canon(c.Ref.Table) != canon(t.Name) {
				continue
			}
			i := t.col(c.Ref.Column)
			if i < 0 {
				return fail("corrupt", "missing referenced column")
			}
			if newValues != nil && sameValue(old.values[i], newValues[i]) {
				continue
			}
			if old.values[i] == nil {
				continue
			}
			rows, e := tx.scanTable(child)
			if e != nil {
				return e
			}
			for _, r := range rows {
				if child == t && r.id == old.id {
					continue
				}
				if r.values[p] != nil && sameValue(r.values[p], old.values[i]) {
					return fail("constraint", "%s referenced by %s", t.Name, child.Name)
				}
			}
		}
	}
	return nil
}

func (tx *Tx) deleteRows(s *sql.Delete, args []Value) (Result, error) {
	t, e := tx.getTable(s.Table)
	if e != nil {
		return Result{}, e
	}
	if e := bindExpr(s.Where, rowEnv(t, nil), false); e != nil {
		return Result{}, e
	}
	rows, e := tx.scanWhere(t, s.Where, args)
	if e != nil {
		return Result{}, e
	}
	var result Result
	for _, r := range rows {
		ok, e := matches(s.Where, t, r.values, args)
		if e != nil {
			return Result{}, e
		}
		if !ok {
			continue
		}
		if e = tx.restrict(t, r, nil); e != nil {
			return Result{}, e
		}
		if e = tx.removeRow(t, r); e != nil {
			return Result{}, e
		}
		result.RowsAffected++
	}
	return result, nil
}

func (tx *Tx) update(s *sql.Update, args []Value) (Result, error) {
	t, e := tx.getTable(s.Table)
	if e != nil {
		return Result{}, e
	}
	seen := map[int]bool{}
	for _, a := range s.Assignments {
		i := t.col(a.Column)
		if i < 0 {
			return Result{}, fail("schema", "unknown update column %s", a.Column)
		}
		if seen[i] {
			return Result{}, fail("schema", "duplicate update column")
		}
		seen[i] = true
	}
	if e := bindExpr(s.Where, rowEnv(t, nil), false); e != nil {
		return Result{}, e
	}
	for _, a := range s.Assignments {
		if e := bindExpr(a.Value, rowEnv(t, nil), false); e != nil {
			return Result{}, e
		}
	}
	rows, e := tx.scanWhere(t, s.Where, args)
	if e != nil {
		return Result{}, e
	}
	var result Result
	for _, r := range rows {
		if e = tx.ctx.Err(); e != nil {
			return Result{}, e
		}
		ok, e := matches(s.Where, t, r.values, args)
		if e != nil {
			return Result{}, e
		}
		if !ok {
			continue
		}
		v := append([]Value(nil), r.values...)
		for _, a := range s.Assignments {
			x, e := eval(a.Value, rowEnv(t, r.values), args)
			if e != nil {
				return Result{}, e
			}
			i := t.col(a.Column)
			v[i], e = coerce(t.Columns[i], x)
			if e != nil {
				return Result{}, e
			}
		}
		if e = tx.restrict(t, r, v); e != nil {
			return Result{}, e
		}
		id := r.id
		for i, c := range t.Columns {
			if c.Primary && c.Type == "INTEGER" {
				id = v[i].(int64)
			}
		}
		if id >= t.NextID {
			if id == math.MaxInt64 {
				t.NextID = id
				t.Exhausted = true
			} else {
				t.NextID = id + 1
			}
		}
		if e = tx.removeRow(t, r); e != nil {
			return Result{}, e
		}
		if e = tx.putRow(t, id, v); e != nil {
			return Result{}, e
		}
		if e = tx.foreignKeys(t, v); e != nil {
			return Result{}, e
		}
		result.RowsAffected++
	}
	return result, nil
}
