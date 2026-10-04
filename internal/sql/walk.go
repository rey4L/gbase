package sql

// WalkExpr visits an expression and its children in preorder. Returning false
// from visit skips that expression's children.
func WalkExpr(expression Expr, visit func(Expr) bool) {
	if expression == nil || !visit(expression) {
		return
	}
	for _, child := range Children(expression) {
		WalkExpr(child, visit)
	}
}

// Children returns an expression's direct subexpressions, omitting absent ones.
func Children(expression Expr) []Expr {
	switch e := expression.(type) {
	case *Unary:
		return []Expr{e.X}
	case *Binary:
		return []Expr{e.Left, e.Right}
	case *Call:
		return e.Args
	case *Case:
		var out []Expr
		if e.Operand != nil {
			out = append(out, e.Operand)
		}
		for _, w := range e.Whens {
			out = append(out, w.Cond, w.Result)
		}
		if e.Else != nil {
			out = append(out, e.Else)
		}
		return out
	}
	return nil
}

// WalkStatement visits every expression in a statement, including descendants
// and statements wrapped by EXPLAIN. Returning false skips expression children.
func WalkStatement(statement Statement, visit func(Expr) bool) {
	walk := func(e Expr) { WalkExpr(e, visit) }
	switch s := statement.(type) {
	case *CreateTable:
		for _, c := range s.Columns {
			walk(c.Default)
		}
	case *AlterTable:
		walk(s.Column.Default)
	case *Insert:
		for _, row := range s.Rows {
			for _, e := range row {
				walk(e)
			}
		}
	case *Select:
		for _, c := range s.Columns {
			walk(c.Expr)
		}
		for _, j := range s.Joins {
			walk(j.On)
		}
		walk(s.Where)
		for _, e := range s.GroupBy {
			walk(e)
		}
		walk(s.Having)
		for _, o := range s.OrderBy {
			walk(o.Expr)
		}
		walk(s.Limit)
		walk(s.Offset)
	case *Update:
		for _, a := range s.Assignments {
			walk(a.Value)
		}
		walk(s.Where)
	case *Delete:
		walk(s.Where)
	case *Explain:
		WalkStatement(s.Statement, visit)
	}
}

// ParameterCount returns the number of positional arguments required by a
// parsed statement. Parameter indexes are assigned in lexical order starting at 0.
func ParameterCount(statement Statement) int {
	count := 0
	WalkStatement(statement, func(e Expr) bool {
		if p, ok := e.(*Parameter); ok && p.Index >= count {
			count = p.Index + 1
		}
		return true
	})
	return count
}
