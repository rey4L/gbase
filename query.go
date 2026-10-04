package gbase

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/rey4L/gbase/internal/sql"
	"github.com/rey4L/gbase/internal/storage"
)

type queryResult struct {
	columns []string
	rows    [][]Value
	next    func() ([]Value, bool, error)
}
type queryOp struct {
	code, detail string
	join         int
}
type queryProgram struct {
	stmt          *sql.Select
	env           evalEnv
	items         []sql.SelectItem
	ops           []queryOp
	idx           *index
	primary       bool
	start, end    []byte
	limit, offset int64
	grouped       bool
	seekOp        string // comparison operator of the chosen index term, for EXPLAIN QUERY PLAN
}
type queryRow struct {
	env           evalEnv
	values, order []Value
}

func (tx *Tx) queryContext() error {
	if tx.ctx == nil {
		return nil
	}
	return tx.ctx.Err()
}

func containsAggregate(x sql.Expr) bool {
	switch e := x.(type) {
	case *sql.Call:
		if aggregate(e.Name) {
			return true
		}
		if slices.ContainsFunc(e.Args, containsAggregate) {
			return true
		}
	case *sql.Binary:
		return containsAggregate(e.Left) || containsAggregate(e.Right)
	case *sql.Unary:
		return containsAggregate(e.X)
	}
	return false
}

func groupedExpr(x sql.Expr, groups []sql.Expr) bool {
	for _, g := range groups {
		if reflect.DeepEqual(x, g) {
			return true
		}
	}
	switch e := x.(type) {
	case *sql.Column:
		return false
	case *sql.Call:
		if aggregate(e.Name) {
			return true
		}
		for _, a := range e.Args {
			if !groupedExpr(a, groups) {
				return false
			}
		}
	case *sql.Binary:
		return groupedExpr(e.Left, groups) && groupedExpr(e.Right, groups)
	case *sql.Unary:
		return groupedExpr(e.X, groups)
	}
	return true
}

func queryBound(x sql.Expr, args []Value, def int64) (int64, error) {
	if x == nil {
		return def, nil
	}
	switch x.(type) {
	case *sql.Literal, *sql.Parameter:
	default:
		return 0, fail("query", "LIMIT/OFFSET requires integer literal or parameter")
	}
	v, e := eval(x, evalEnv{}, args)
	if e != nil {
		return 0, e
	}
	n, ok := v.(int64)
	if !ok || n < 0 {
		return 0, fail("query", "LIMIT/OFFSET requires nonnegative integer")
	}
	return n, nil
}

func (tx *Tx) compileSelect(s *sql.Select, args []Value) (*queryProgram, error) {
	if e := tx.queryContext(); e != nil {
		return nil, e
	}
	p := &queryProgram{stmt: s, limit: -1}
	add := func(r sql.TableRef) error {
		t, e := tx.getTable(r.Name)
		if e != nil {
			return e
		}
		name := r.Alias
		if name == "" {
			name = t.Name
		}
		for _, b := range p.env.bindings {
			if canon(b.name) == canon(name) {
				return fail("schema", "duplicate table binding %s", name)
			}
		}
		p.env.bindings = append(p.env.bindings, evalBinding{name: name, table: t})
		return nil
	}
	if s.From.Name != "" {
		if e := add(s.From); e != nil {
			return nil, e
		}
	} else if len(s.Joins) > 0 {
		return nil, fail("query", "JOIN requires FROM")
	}
	for _, j := range s.Joins {
		if e := add(j.Table); e != nil {
			return nil, e
		}
		if j.Type != "INNER" && j.Type != "LEFT" {
			return nil, fail("query", "unsupported join %s", j.Type)
		}
		if e := bindExpr(j.On, p.env, false); e != nil {
			return nil, e
		}
	}
	for _, item := range s.Columns {
		if star, ok := item.Expr.(*sql.Star); ok {
			found := false
			for _, b := range p.env.bindings {
				if star.Table != "" && canon(star.Table) != canon(b.name) {
					continue
				}
				found = true
				for _, c := range b.table.Columns {
					p.items = append(p.items, sql.SelectItem{Expr: &sql.Column{Table: b.name, Name: c.Name}})
				}
			}
			if !found {
				return nil, fail("schema", "unknown star source %s", star.Table)
			}
		} else {
			p.items = append(p.items, item)
		}
	}
	if len(p.items) == 0 {
		return nil, fail("query", "empty projection")
	}
	if e := bindExpr(s.Where, p.env, false); e != nil {
		return nil, e
	}
	for _, g := range s.GroupBy {
		if e := bindExpr(g, p.env, false); e != nil {
			return nil, e
		}
	}
	p.grouped = len(s.GroupBy) > 0 || containsAggregate(s.Having)
	for _, item := range p.items {
		if containsAggregate(item.Expr) {
			p.grouped = true
		}
		if e := bindExpr(item.Expr, p.env, true); e != nil {
			return nil, e
		}
	}
	for _, o := range s.OrderBy {
		expr, e := p.orderExpr(o.Expr)
		if e != nil {
			return nil, e
		}
		if containsAggregate(expr) {
			p.grouped = true
		}
		if e = bindExpr(expr, p.env, true); e != nil {
			return nil, e
		}
	}
	if e := bindExpr(s.Having, p.env, true); e != nil {
		return nil, e
	}
	if s.Having != nil && !p.grouped {
		return nil, fail("query", "HAVING requires grouping")
	}
	if p.grouped {
		for _, item := range p.items {
			if !groupedExpr(item.Expr, s.GroupBy) {
				return nil, fail("query", "ungrouped column in projection")
			}
		}
		if !groupedExpr(s.Having, s.GroupBy) {
			return nil, fail("query", "ungrouped column in HAVING")
		}
		for _, o := range s.OrderBy {
			expr, _ := p.orderExpr(o.Expr)
			if !groupedExpr(expr, s.GroupBy) {
				return nil, fail("query", "ungrouped column in ORDER BY")
			}
		}
	}
	var e error
	p.limit, e = queryBound(s.Limit, args, -1)
	if e != nil {
		return nil, e
	}
	p.offset, e = queryBound(s.Offset, args, 0)
	if e != nil {
		return nil, e
	}
	p.chooseIndex(tx, args)
	scan := queryOp{code: "Scan", detail: s.From.Name}
	if p.idx != nil {
		scan.code = "IndexSeek"
		scan.detail = p.idx.Name
	}
	if s.From.Name == "" {
		scan.detail = "constant"
	}
	p.ops = append(p.ops, scan)
	for i, j := range s.Joins {
		p.ops = append(p.ops, queryOp{code: "Join", detail: j.Type + " " + j.Table.Name, join: i})
	}
	if s.Where != nil {
		p.ops = append(p.ops, queryOp{code: "Filter", detail: exprName(s.Where)})
	}
	if p.grouped {
		p.ops = append(p.ops, queryOp{code: "Group"})
	}
	if s.Having != nil {
		p.ops = append(p.ops, queryOp{code: "Having", detail: exprName(s.Having)})
	}
	p.ops = append(p.ops, queryOp{code: "Project"})
	if s.Distinct {
		p.ops = append(p.ops, queryOp{code: "Distinct"})
	}
	if len(s.OrderBy) > 0 {
		p.ops = append(p.ops, queryOp{code: "Sort"})
	}
	if s.Limit != nil || s.Offset != nil {
		p.ops = append(p.ops, queryOp{code: "Limit"})
	}
	return p, nil
}

func (p *queryProgram) orderExpr(x sql.Expr) (sql.Expr, error) {
	if c, ok := x.(*sql.Column); ok && c.Table == "" {
		var found sql.Expr
		for _, it := range p.items {
			if it.Alias != "" && canon(it.Alias) == canon(c.Name) {
				if found != nil {
					return nil, fail("schema", "ambiguous ORDER BY alias %s", c.Name)
				}
				found = it.Expr
			}
		}
		if found != nil {
			return found, nil
		}
	}
	if l, ok := x.(*sql.Literal); ok {
		if n, ok := l.Value.(int64); ok {
			if n < 1 || n > int64(len(p.items)) {
				return nil, fail("query", "ORDER BY position out of range")
			}
			return p.items[n-1].Expr, nil
		}
	}
	return x, nil
}

func (p *queryProgram) chooseIndex(tx *Tx, args []Value) {
	if len(p.env.bindings) == 0 {
		return
	}
	var terms []sql.Expr
	var flatten func(sql.Expr)
	flatten = func(x sql.Expr) {
		if b, ok := x.(*sql.Binary); ok && strings.EqualFold(b.Op, "AND") {
			flatten(b.Left)
			flatten(b.Right)
		} else {
			terms = append(terms, x)
		}
	}
	flatten(p.stmt.Where)
	binding := p.env.bindings[0]
	candidates := tx.indexes(binding.table)
	var primary *index
	for _, c := range binding.table.Columns {
		if c.Primary && c.Type == "INTEGER" {
			primary = &index{Name: binding.table.Name + " PRIMARY KEY", Table: binding.table.Name, Column: c.Name, Root: binding.table.Root, Unique: true}
			candidates = append([]*index{primary}, candidates...)
			break
		}
	}
	for _, idx := range candidates {
		for _, term := range terms {
			b, ok := term.(*sql.Binary)
			if !ok {
				continue
			}
			op := b.Op
			left, right := b.Left, b.Right
			c, ok := left.(*sql.Column)
			if !ok {
				c, ok = right.(*sql.Column)
				right = left
				switch op {
				case "<":
					op = ">"
				case "<=":
					op = ">="
				case ">":
					op = "<"
				case ">=":
					op = "<="
				}
			}
			if !ok || canon(c.Name) != canon(idx.Column) || c.Table != "" && canon(c.Table) != canon(binding.name) {
				continue
			}
			switch right.(type) {
			case *sql.Literal, *sql.Parameter:
			default:
				continue
			}
			v, e := eval(right, evalEnv{}, args)
			if e != nil || v == nil {
				continue
			}
			typ := strings.ToUpper(binding.table.Columns[binding.table.col(idx.Column)].Type)
			safe := false
			switch v.(type) {
			case int64:
				safe = typ == "INTEGER"
			case float64:
				safe = typ == "REAL"
			case string:
				safe = typ == "TEXT"
			case []byte:
				safe = typ == "BLOB"
			}
			if !safe {
				continue
			}
			prefix, e := indexPrefix(v)
			if e != nil {
				continue
			}
			typeStart := prefix[:1]
			typeEnd := prefixEnd(typeStart)
			if idx == primary {
				prefix = rowKey(v.(int64))
				typeStart = nil
				typeEnd = nil
			}
			switch op {
			case "=":
				p.start, p.end = prefix, prefixEnd(prefix)
			case ">":
				p.start, p.end = prefixEnd(prefix), typeEnd
				if idx == primary && p.start == nil {
					p.start = append(append([]byte(nil), prefix...), 0)
				}
			case ">=":
				p.start, p.end = prefix, typeEnd
			case "<":
				p.start, p.end = typeStart, prefix
			case "<=":
				p.start, p.end = typeStart, prefixEnd(prefix)
			default:
				continue
			}
			p.idx = idx
			p.primary = idx == primary
			p.seekOp = op
			return
		}
	}
}

func (p *queryProgram) columns() []string {
	out := make([]string, len(p.items))
	for i, it := range p.items {
		out[i] = it.Alias
		if out[i] == "" {
			out[i] = exprName(it.Expr)
		}
	}
	return out
}

func (tx *Tx) sourceRows(p *queryProgram) (func() (evalEnv, bool, error), error) {
	ctx := tx.ctx
	if len(p.env.bindings) == 0 {
		done := false
		return func() (evalEnv, bool, error) {
			if e := ctx.Err(); e != nil {
				return evalEnv{}, false, e
			}
			if done {
				return evalEnv{}, false, nil
			}
			done = true
			return evalEnv{}, true, nil
		}, nil
	}
	binding := p.env.bindings[0]
	tr := storage.Tree{Tx: tx.pages, Root: binding.table.Root}
	tree := tr
	if p.idx != nil && !p.primary {
		tree = storage.Tree{Tx: tx.pages, Root: p.idx.Root}
	}
	cursor, e := tree.Scan(p.start, p.end)
	if e != nil {
		return nil, e
	}
	return func() (evalEnv, bool, error) {
		if e := ctx.Err(); e != nil {
			return evalEnv{}, false, e
		}
		if !cursor.Next() {
			return evalEnv{}, false, cursor.Err()
		}
		data := cursor.Value()
		if p.idx != nil && !p.primary {
			var ok bool
			var e error
			data, ok, e = tr.Get(data)
			if e != nil {
				return evalEnv{}, false, e
			}
			if !ok {
				return evalEnv{}, false, fail("corrupt", "dangling index entry")
			}
		}
		values, e := decodeRecord(data)
		if e != nil {
			return evalEnv{}, false, e
		}
		if len(values) != len(binding.table.Columns) {
			return evalEnv{}, false, fail("corrupt", "record column count mismatch")
		}
		b := binding
		b.values = values
		return evalEnv{bindings: []evalBinding{b}}, true, nil
	}, nil
}

func evalFilter(x sql.Expr, env evalEnv, args []Value) (bool, error) {
	if x == nil {
		return true, nil
	}
	v, e := eval(x, env, args)
	if e != nil {
		return false, e
	}
	return truth(v)
}

func (p *queryProgram) project(env evalEnv, args []Value) (queryRow, error) {
	r := queryRow{env: env}
	for _, item := range p.items {
		v, e := eval(item.Expr, env, args)
		if e != nil {
			return r, e
		}
		r.values = append(r.values, v)
	}
	for _, term := range p.stmt.OrderBy {
		x, _ := p.orderExpr(term.Expr)
		v, e := eval(x, env, args)
		if e != nil {
			return r, e
		}
		r.order = append(r.order, v)
	}
	return r, nil
}

func (tx *Tx) executeSelect(s *sql.Select, args []Value) (*queryResult, error) {
	p, e := tx.compileSelect(s, args)
	if e != nil {
		return nil, e
	}
	next, e := tx.sourceRows(p)
	if e != nil {
		return nil, e
	}
	q := &queryResult{columns: p.columns()}
	ctx := tx.ctx
	if len(s.Joins) == 0 && !p.grouped && !s.Distinct && len(s.OrderBy) == 0 {
		var skipped, emitted int64
		q.next = func() ([]Value, bool, error) {
			for {
				if e := ctx.Err(); e != nil {
					return nil, false, e
				}
				if p.limit >= 0 && emitted >= p.limit {
					return nil, false, nil
				}
				env, ok, e := next()
				if e != nil || !ok {
					return nil, false, e
				}
				yes, e := evalFilter(s.Where, env, args)
				if e != nil {
					return nil, false, e
				}
				if !yes {
					continue
				}
				if skipped < p.offset {
					skipped++
					continue
				}
				r, e := p.project(env, args)
				if e != nil {
					return nil, false, e
				}
				emitted++
				return r.values, true, nil
			}
		}
		return q, nil
	}
	var rows []queryRow
	for {
		env, ok, e := next()
		if e != nil {
			return nil, e
		}
		if !ok {
			break
		}
		rows = append(rows, queryRow{env: env})
	}
	for _, op := range p.ops {
		if e := tx.queryContext(); e != nil {
			return nil, e
		}
		switch op.code {
		case "Scan", "IndexSeek":
		case "Join":
			j := s.Joins[op.join]
			b := p.env.bindings[op.join+1]
			right, e := tx.scanTable(b.table)
			if e != nil {
				return nil, e
			}
			var joined []queryRow
			for _, r := range rows {
				hit := false
				for _, sr := range right {
					if e := tx.queryContext(); e != nil {
						return nil, e
					}
					binding := b
					binding.values = sr.values
					env := evalEnv{bindings: append(append([]evalBinding(nil), r.env.bindings...), binding)}
					ok, e := evalFilter(j.On, env, args)
					if e != nil {
						return nil, e
					}
					if ok {
						hit = true
						joined = append(joined, queryRow{env: env})
					}
				}
				if !hit && j.Type == "LEFT" {
					binding := b
					binding.values = make([]Value, len(b.table.Columns))
					joined = append(joined, queryRow{env: evalEnv{bindings: append(append([]evalBinding(nil), r.env.bindings...), binding)}})
				}
			}
			rows = joined
		case "Filter", "Having":
			x := s.Where
			if op.code == "Having" {
				x = s.Having
			}
			out := rows[:0]
			for _, r := range rows {
				if e := tx.queryContext(); e != nil {
					return nil, e
				}
				ok, e := evalFilter(x, r.env, args)
				if e != nil {
					return nil, e
				}
				if ok {
					out = append(out, r)
				}
			}
			rows = out
		case "Group":
			var groups [][]evalEnv
			var keys [][]Value
			if len(s.GroupBy) == 0 {
				groups = append(groups, []evalEnv{})
				keys = append(keys, nil)
			}
			for _, r := range rows {
				if e := tx.queryContext(); e != nil {
					return nil, e
				}
				var key []Value
				for _, x := range s.GroupBy {
					v, e := eval(x, r.env, args)
					if e != nil {
						return nil, e
					}
					key = append(key, v)
				}
				at := -1
				for i, k := range keys {
					same, e := equalValues(k, key)
					if e != nil {
						return nil, e
					}
					if same {
						at = i
						break
					}
				}
				if at < 0 {
					at = len(groups)
					groups = append(groups, nil)
					keys = append(keys, key)
				}
				groups[at] = append(groups[at], r.env)
			}
			rows = nil
			for _, g := range groups {
				env := p.env
				if len(g) > 0 {
					env = g[0]
				}
				env.group = g
				rows = append(rows, queryRow{env: env})
			}
		case "Project":
			for i, r := range rows {
				if e := tx.queryContext(); e != nil {
					return nil, e
				}
				projected, e := p.project(r.env, args)
				if e != nil {
					return nil, e
				}
				rows[i] = projected
			}
		case "Distinct":
			var out []queryRow
			for _, r := range rows {
				if e := tx.queryContext(); e != nil {
					return nil, e
				}
				seen := false
				for _, other := range out {
					same, e := equalValues(r.values, other.values)
					if e != nil {
						return nil, e
					}
					if same {
						seen = true
						break
					}
				}
				if !seen {
					out = append(out, r)
				}
			}
			rows = out
		case "Sort":
			var sortErr error
			sort.SliceStable(rows, func(i, j int) bool {
				if sortErr != nil {
					return false
				}
				if e := tx.queryContext(); e != nil {
					sortErr = e
					return false
				}
				for k, term := range s.OrderBy {
					c, e := compareValues(rows[i].order[k], rows[j].order[k])
					if e != nil {
						sortErr = e
						return false
					}
					if c != 0 {
						if term.Desc {
							return c > 0
						}
						return c < 0
					}
				}
				return false
			})
			if sortErr != nil {
				return nil, sortErr
			}
		case "Limit":
			start := p.offset
			if start > int64(len(rows)) {
				start = int64(len(rows))
			}
			rows = rows[start:]
			if p.limit >= 0 && p.limit < int64(len(rows)) {
				rows = rows[:p.limit]
			}
		}
	}
	for _, r := range rows {
		q.rows = append(q.rows, r.values)
	}
	return q, nil
}

func equalValues(a, b []Value) (bool, error) {
	if len(a) != len(b) {
		return false, nil
	}
	for i, v := range a {
		if v == nil || b[i] == nil {
			if v != nil || b[i] != nil {
				return false, nil
			}
			continue
		}
		c, e := compareValues(v, b[i])
		if e != nil {
			return false, nil
		}
		if c != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (tx *Tx) explainSelect(s *sql.Select, args []Value) (*queryResult, error) {
	p, e := tx.compileSelect(s, args)
	if e != nil {
		return nil, e
	}
	q := &queryResult{columns: []string{"addr", "opcode", "detail"}}
	for i, op := range p.ops {
		q.rows = append(q.rows, []Value{int64(i), op.code, op.detail})
	}
	return q, nil
}

type planNode struct {
	id, parent int64
	detail     string
}

func tableLabel(t sql.TableRef) string {
	if t.Alias != "" {
		return t.Alias
	}
	return t.Name
}

// queryPlan describes the access path the way SQLite's EXPLAIN QUERY PLAN does:
// one node per table access in join order, then the temp b-trees the executor builds.
func (p *queryProgram) queryPlan() []planNode {
	s := p.stmt
	var nodes []planNode
	add := func(detail string) { nodes = append(nodes, planNode{id: int64(len(nodes) + 1), detail: detail}) }
	switch {
	case s.From.Name == "":
		add("SCAN CONSTANT ROW")
	case p.idx == nil:
		add("SCAN " + tableLabel(s.From))
	default:
		col, using := p.idx.Column, "INDEX "+p.idx.Name
		if p.primary {
			col, using = "rowid", "INTEGER PRIMARY KEY"
		}
		cmp := p.seekOp
		switch cmp {
		case ">=":
			cmp = ">"
		case "<=":
			cmp = "<"
		}
		add(fmt.Sprintf("SEARCH %s USING %s (%s%s?)", tableLabel(s.From), using, col, cmp))
	}
	for _, j := range s.Joins {
		add("SCAN " + tableLabel(j.Table))
	}
	if len(s.GroupBy) > 0 {
		add("USE TEMP B-TREE FOR GROUP BY")
	}
	if s.Distinct {
		add("USE TEMP B-TREE FOR DISTINCT")
	}
	if len(s.OrderBy) > 0 {
		add("USE TEMP B-TREE FOR ORDER BY")
	}
	return nodes
}

func (tx *Tx) explainQueryPlan(stmt sql.Statement, args []Value) (*queryResult, error) {
	switch s := stmt.(type) {
	case *sql.Select:
		p, e := tx.compileSelect(s, args)
		if e != nil {
			return nil, e
		}
		return queryPlanResult(p.queryPlan()), nil
	case *sql.Update, *sql.Delete:
		// Mutations always scan the whole table; validate them without writing.
		var table string
		var where sql.Expr
		if u, ok := s.(*sql.Update); ok {
			table, where = u.Table, u.Where
		} else {
			d := s.(*sql.Delete)
			table, where = d.Table, d.Where
		}
		t, e := tx.getTable(table)
		if e != nil {
			return nil, e
		}
		if u, ok := s.(*sql.Update); ok {
			for _, a := range u.Assignments {
				if t.col(a.Column) < 0 {
					return nil, fail("schema", "unknown update column %s", a.Column)
				}
				if e := bindExpr(a.Value, rowEnv(t, nil), false); e != nil {
					return nil, e
				}
			}
		}
		if e := bindExpr(where, rowEnv(t, nil), false); e != nil {
			return nil, e
		}
		return queryPlanResult([]planNode{{id: 1, detail: "SCAN " + t.Name}}), nil
	}
	return nil, fmt.Errorf("EXPLAIN QUERY PLAN supports SELECT, UPDATE and DELETE")
}

func queryPlanResult(nodes []planNode) *queryResult {
	q := &queryResult{columns: []string{"id", "parent", "notused", "detail"}}
	for _, n := range nodes {
		q.rows = append(q.rows, []Value{n.id, n.parent, int64(0), n.detail})
	}
	return q
}

var _ context.Context
