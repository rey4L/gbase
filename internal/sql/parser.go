package sql

import (
	"fmt"
	"math"
	"strings"
)

type parser struct {
	tokens     []Token
	pos, depth int
	err        error
}

// Parse accepts exactly one supported statement, optionally followed by a semicolon.
// Identifiers preserve their spelling. Operators, types, and function names are uppercase.
func Parse(input string) (Statement, error) {
	tokens, err := Lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	s := p.statement()
	p.take(";")
	if p.err == nil && p.peek().Kind != TokenEOF {
		p.fail("unexpected token %q; only one statement is supported", p.peek().Text)
	}
	if p.err != nil {
		return nil, p.err
	}
	return s, nil
}
func (p *parser) peek() Token { return p.tokens[p.pos] }
func (p *parser) next() Token {
	t := p.peek()
	if t.Kind != TokenEOF {
		p.pos++
	}
	return t
}

func (p *parser) is(s string) bool {
	t := p.peek()
	return (t.Kind == TokenKeyword || t.Kind == TokenSymbol) && t.Text == s
}

func (p *parser) take(s string) bool {
	if p.err == nil && p.is(s) {
		p.next()
		return true
	}
	return false
}

// takeWord consumes an unreserved word such as QUERY or PLAN, matched case-insensitively.
func (p *parser) takeWord(s string) bool {
	if t := p.peek(); p.err == nil && t.Kind == TokenIdentifier && strings.EqualFold(t.Text, s) {
		p.next()
		return true
	}
	return false
}

func (p *parser) fail(format string, args ...any) {
	if p.err == nil {
		p.err = &Error{Pos: p.peek().Pos, Message: fmt.Sprintf(format, args...)}
	}
}

func (p *parser) expect(s string) {
	if !p.take(s) {
		p.fail("expected %s, found %q", s, p.peek().Text)
	}
}

func (p *parser) ident() string {
	if p.err != nil {
		return ""
	}
	if p.peek().Kind != TokenIdentifier {
		p.fail("expected identifier, found %q", p.peek().Text)
		return ""
	}
	return p.next().Text
}

func (p *parser) statement() Statement {
	switch {
	case p.take("CREATE"):
		return p.create()
	case p.take("DROP"):
		return p.drop()
	case p.take("INSERT"):
		return p.insert()
	case p.take("SELECT"):
		return p.selectStatement()
	case p.take("UPDATE"):
		return p.update()
	case p.take("DELETE"):
		return p.deleteStatement()
	case p.take("ALTER"):
		return p.alter()
	case p.take("EXPLAIN"):
		plan := p.takeWord("QUERY")
		if plan && !p.takeWord("PLAN") {
			p.fail("expected PLAN, found %q", p.peek().Text)
			return nil
		}
		if p.is("EXPLAIN") {
			p.fail("nested EXPLAIN is unsupported")
			return nil
		}
		return &Explain{Statement: p.statement(), QueryPlan: plan}
	default:
		p.fail("expected supported SQL statement, found %q", p.peek().Text)
		return nil
	}
}

func (p *parser) ifNotExists() bool {
	if !p.take("IF") {
		return false
	}
	p.expect("NOT")
	p.expect("EXISTS")
	return true
}

func (p *parser) ifExists() bool {
	if !p.take("IF") {
		return false
	}
	p.expect("EXISTS")
	return true
}

func (p *parser) names(single bool) []string {
	p.expect("(")
	names := []string{p.ident()}
	for p.take(",") {
		if single {
			p.fail("only single-column table constraints are supported")
			break
		}
		names = append(names, p.ident())
	}
	p.expect(")")
	return names
}

func (p *parser) create() Statement {
	if p.take("TABLE") {
		return p.createTable()
	}
	unique := p.take("UNIQUE")
	p.expect("INDEX")
	s := &CreateIndex{Unique: unique, IfNotExists: p.ifNotExists()}
	s.Name = p.ident()
	p.expect("ON")
	s.Table = p.ident()
	s.Columns = p.names(false)
	return s
}

func (p *parser) drop() Statement {
	if p.take("TABLE") {
		s := &DropTable{IfExists: p.ifExists()}
		s.Name = p.ident()
		return s
	}
	unique := p.take("UNIQUE")
	p.expect("INDEX")
	s := &DropIndex{Unique: unique, IfExists: p.ifExists()}
	s.Name = p.ident()
	return s
}

// alter parses ALTER TABLE t ADD [COLUMN] def, DROP [COLUMN] c,
// RENAME TO name, or RENAME [COLUMN] c TO name.
func (p *parser) alter() Statement {
	p.expect("TABLE")
	s := &AlterTable{Table: p.ident()}
	switch {
	case p.takeWord("ADD"):
		p.takeWord("COLUMN")
		s.Action = "ADD COLUMN"
		s.Column = p.columnDef()
	case p.take("DROP"):
		p.takeWord("COLUMN")
		s.Action = "DROP COLUMN"
		s.From = p.ident()
	case p.takeWord("RENAME"):
		if p.takeWord("TO") {
			s.Action = "RENAME TO"
			s.To = p.ident()
			break
		}
		p.takeWord("COLUMN")
		s.Action = "RENAME COLUMN"
		s.From = p.ident()
		if !p.takeWord("TO") {
			p.fail("expected TO, found %q", p.peek().Text)
		}
		s.To = p.ident()
	default:
		p.fail("expected ADD, DROP, or RENAME, found %q", p.peek().Text)
	}
	return s
}

func (p *parser) createTable() Statement {
	s := &CreateTable{IfNotExists: p.ifNotExists()}
	s.Name = p.ident()
	p.expect("(")
	for p.err == nil {
		if p.is("PRIMARY") || p.is("UNIQUE") || p.is("FOREIGN") {
			c := TableConstraint{}
			switch {
			case p.take("PRIMARY"):
				p.expect("KEY")
				c.Kind = "PRIMARY KEY"
			case p.take("UNIQUE"):
				c.Kind = "UNIQUE"
			case p.take("FOREIGN"):
				p.expect("KEY")
				c.Kind = "FOREIGN KEY"
			}
			names := p.names(c.Kind != "UNIQUE")
			c.Column = names[0]
			if len(names) > 1 {
				c.Columns = names
			}
			if c.Kind == "FOREIGN KEY" {
				p.expect("REFERENCES")
				c.References = p.reference()
			}
			s.Constraints = append(s.Constraints, c)
		} else {
			c := p.columnDef()
			s.Columns = append(s.Columns, c)
		}
		if !p.take(",") {
			break
		}
	}
	p.expect(")")
	if p.err != nil {
		return s
	}
	if len(s.Columns) == 0 {
		p.fail("table requires at least one column")
		return s
	}
	columns := map[string]bool{}
	primary := 0
	for _, c := range s.Columns {
		name := strings.ToUpper(c.Name)
		if columns[name] {
			p.fail("duplicate column %q", c.Name)
		}
		columns[name] = true
		if c.PrimaryKey {
			primary++
		}
	}
	constraints := map[string]bool{}
	for _, c := range s.Constraints {
		names := c.Columns
		if len(names) == 0 {
			names = []string{c.Column}
		}
		seen := map[string]bool{}
		for _, n := range names {
			n = strings.ToUpper(n)
			if !columns[n] {
				p.fail("constraint references unknown column %q", n)
			}
			if seen[n] {
				p.fail("duplicate constraint column %q", n)
			}
			seen[n] = true
		}
		name := strings.ToUpper(strings.Join(names, ","))
		key := c.Kind + ":" + name
		if constraints[key] {
			p.fail("duplicate table constraint")
		}
		constraints[key] = true
		if c.Kind == "PRIMARY KEY" {
			primary++
		}
	}
	if primary > 1 {
		p.fail("only one single-column primary key is supported")
	}
	return s
}

func (p *parser) columnDef() ColumnDef {
	c := ColumnDef{Name: p.ident()}
	switch {
	case p.take("INTEGER"):
		c.Type = "INTEGER"
	case p.take("REAL"):
		c.Type = "REAL"
	case p.take("TEXT"):
		c.Type = "TEXT"
	case p.take("BLOB"):
		c.Type = "BLOB"
	default:
		p.fail("expected INTEGER, REAL, TEXT, or BLOB")
	}
	seen := map[string]bool{}
	for p.err == nil {
		key := p.peek().Text
		if key != "PRIMARY" && key != "UNIQUE" && key != "NOT" && key != "DEFAULT" && key != "REFERENCES" || p.peek().Kind != TokenKeyword {
			break
		}
		if seen[key] {
			p.fail("duplicate column constraint %s", key)
			break
		}
		seen[key] = true
		p.next()
		switch key {
		case "PRIMARY":
			p.expect("KEY")
			c.PrimaryKey = true
		case "UNIQUE":
			c.Unique = true
		case "NOT":
			p.expect("NULL")
			c.NotNull = true
		case "DEFAULT":
			c.Default = p.defaultLiteral()
		case "REFERENCES":
			c.References = p.reference()
		}
	}
	return c
}

func (p *parser) reference() *ForeignKey {
	f := &ForeignKey{Table: p.ident()}
	f.Column = p.names(true)[0]
	for p.take("ON") {
		action := ""
		switch {
		case p.take("DELETE"):
			action = "DELETE"
		case p.take("UPDATE"):
			action = "UPDATE"
		default:
			p.fail("expected DELETE or UPDATE after ON")
		}
		p.expect("RESTRICT")
		if action == "DELETE" {
			if f.OnDelete != "" {
				p.fail("duplicate ON DELETE action")
			}
			f.OnDelete = "RESTRICT"
		} else {
			if f.OnUpdate != "" {
				p.fail("duplicate ON UPDATE action")
			}
			f.OnUpdate = "RESTRICT"
		}
	}
	return f
}

func (p *parser) defaultLiteral() Expr {
	sign := ""
	if p.take("+") {
		sign = "+"
	} else if p.take("-") {
		sign = "-"
	}
	if p.take("NULL") {
		if sign != "" {
			p.fail("DEFAULT sign requires a number")
		}
		return &Literal{Value: nil}
	}
	if p.peek().Kind != TokenLiteral {
		p.fail("DEFAULT requires a literal")
		return nil
	}
	t := p.next()
	v := t.Value
	switch n := v.(type) {
	case int64:
		if sign == "-" {
			v = -n
		}
	case float64:
		if sign == "-" {
			v = -n
		}
	case uint64:
		if sign == "-" {
			v = int64(math.MinInt64)
		} else {
			p.fail("integer literal out of range")
		}
	default:
		if sign != "" {
			p.fail("DEFAULT sign requires a number")
		}
	}
	return &Literal{Value: v}
}

func (p *parser) insert() Statement {
	p.expect("INTO")
	s := &Insert{Table: p.ident()}
	if p.is("(") {
		s.Columns = p.names(false)
	}
	p.expect("VALUES")
	width := -1
	for p.err == nil {
		p.expect("(")
		row := []Expr{p.expression(1)}
		for p.take(",") {
			row = append(row, p.expression(1))
		}
		p.expect(")")
		if width < 0 {
			width = len(row)
		} else if len(row) != width {
			p.fail("VALUES rows must have equal length")
		}
		if len(s.Columns) > 0 && len(row) != len(s.Columns) {
			p.fail("VALUES row does not match column count")
		}
		s.Rows = append(s.Rows, row)
		if !p.take(",") {
			break
		}
	}
	return s
}

func (p *parser) tableRef() TableRef {
	t := TableRef{Name: p.ident()}
	if p.take("AS") {
		t.Alias = p.ident()
	} else if p.peek().Kind == TokenIdentifier {
		t.Alias = p.ident()
	}
	return t
}

func (p *parser) selectStatement() Statement {
	s := &Select{Distinct: p.take("DISTINCT")}
	for p.err == nil {
		item := SelectItem{}
		if p.take("*") {
			item.Expr = &Star{}
		} else if p.peek().Kind == TokenIdentifier && p.pos+2 < len(p.tokens) && p.tokens[p.pos+1].Kind == TokenSymbol && p.tokens[p.pos+1].Text == "." && p.tokens[p.pos+2].Kind == TokenSymbol && p.tokens[p.pos+2].Text == "*" {
			item.Expr = &Star{Table: p.ident()}
			p.expect(".")
			p.expect("*")
		} else {
			item.Expr = p.expression(1)
		}
		if p.take("AS") {
			item.Alias = p.ident()
		} else if p.peek().Kind == TokenIdentifier {
			item.Alias = p.ident()
		}
		if _, star := item.Expr.(*Star); star && item.Alias != "" {
			p.fail("star cannot have an alias")
		}
		s.Columns = append(s.Columns, item)
		if !p.take(",") {
			break
		}
	}
	if p.take("FROM") {
		s.From = p.tableRef()
		for p.err == nil && (p.is("INNER") || p.is("LEFT") || p.is("JOIN")) {
			j := Join{Type: "INNER"}
			if p.take("LEFT") {
				j.Type = "LEFT"
				p.take("OUTER")
			} else {
				p.take("INNER")
			}
			p.expect("JOIN")
			j.Table = p.tableRef()
			p.expect("ON")
			j.On = p.expression(1)
			s.Joins = append(s.Joins, j)
		}
	}
	if p.take("WHERE") {
		s.Where = p.expression(1)
	}
	if p.take("GROUP") {
		p.expect("BY")
		s.GroupBy = p.exprList()
	}
	if p.take("HAVING") {
		s.Having = p.expression(1)
	}
	if p.take("ORDER") {
		p.expect("BY")
		for p.err == nil {
			term := OrderTerm{Expr: p.expression(1)}
			if p.take("DESC") {
				term.Desc = true
			} else {
				p.take("ASC")
			}
			s.OrderBy = append(s.OrderBy, term)
			if !p.take(",") {
				break
			}
		}
	}
	if p.take("LIMIT") {
		s.Limit = p.bound()
	}
	if p.take("OFFSET") {
		s.Offset = p.bound()
	}
	return s
}

func (p *parser) exprList() []Expr {
	result := []Expr{p.expression(1)}
	for p.take(",") {
		result = append(result, p.expression(1))
	}
	return result
}

func (p *parser) bound() Expr {
	if p.peek().Kind == TokenParameter {
		return &Parameter{Index: p.next().Value.(int)}
	}
	if p.peek().Kind == TokenLiteral {
		if n, ok := p.peek().Value.(int64); ok && n >= 0 {
			p.next()
			return &Literal{Value: n}
		}
	}
	p.fail("LIMIT/OFFSET requires a nonnegative integer or parameter")
	return nil
}

func (p *parser) update() Statement {
	s := &Update{Table: p.ident()}
	p.expect("SET")
	for p.err == nil {
		a := Assignment{Column: p.ident()}
		p.expect("=")
		a.Value = p.expression(1)
		s.Assignments = append(s.Assignments, a)
		if !p.take(",") {
			break
		}
	}
	if p.take("WHERE") {
		s.Where = p.expression(1)
	}
	return s
}

func (p *parser) deleteStatement() Statement {
	p.expect("FROM")
	s := &Delete{Table: p.ident()}
	if p.take("WHERE") {
		s.Where = p.expression(1)
	}
	return s
}

func precedence(op string) int {
	switch op {
	case "OR":
		return 1
	case "AND":
		return 2
	case "=", "!=", "<>", "<", ">", "<=", ">=", "IS":
		return 4
	case "+", "-":
		return 5
	case "*", "/", "%":
		return 6
	case "||":
		return 7
	}
	return 0
}

func (p *parser) expression(min int) Expr {
	if p.err != nil {
		return nil
	}
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 256 {
		p.fail("expression nesting exceeds 256")
		return nil
	}
	var left Expr
	switch {
	case p.take("NOT"):
		left = &Unary{Op: "NOT", X: p.expression(3)}
	case p.take("+"):
		left = &Unary{Op: "+", X: p.expression(7)}
	case p.take("-"):
		if n, ok := p.peek().Value.(uint64); p.peek().Kind == TokenLiteral && ok && n == uint64(1)<<63 {
			p.next()
			left = &Literal{Value: int64(math.MinInt64)}
		} else {
			left = &Unary{Op: "-", X: p.expression(7)}
		}
	case p.take("("):
		left = p.expression(1)
		p.expect(")")
	case p.take("NULL"):
		left = &Literal{Value: nil}
	default:
		t := p.peek()
		switch t.Kind {
		case TokenLiteral:
			p.next()
			if _, ok := t.Value.(uint64); ok {
				p.fail("integer literal out of range")
			}
			left = &Literal{Value: t.Value}
		case TokenParameter:
			p.next()
			left = &Parameter{Index: t.Value.(int)}
		case TokenIdentifier:
			name := p.ident()
			if p.is("(") {
				left = p.call(name)
				break
			}
			c := &Column{Name: name}
			if p.take(".") {
				c.Table = name
				c.Name = p.ident()
			}
			left = c
		default:
			p.fail("expected expression, found %q", t.Text)
		}
	}
	for p.err == nil {
		t := p.peek()
		if t.Kind != TokenKeyword && t.Kind != TokenSymbol {
			break
		}
		if min <= 4 && predicateStart(t.Text, p.tokens[p.pos+1]) {
			left = p.predicate(left)
			continue
		}
		prec := precedence(t.Text)
		if prec == 0 || prec < min {
			break
		}
		p.next()
		if t.Text == "IS" {
			op := "IS NULL"
			if p.take("NOT") {
				op = "IS NOT NULL"
			}
			p.expect("NULL")
			left = &Unary{Op: op, X: left}
			continue
		}
		left = &Binary{Op: t.Text, Left: left, Right: p.expression(prec + 1)}
	}
	return left
}

func (p *parser) call(name string) Expr {
	// Function availability and arity are checked by the binder.
	c := &Call{Name: strings.ToUpper(name)}
	p.expect("(")
	if p.take("*") {
		c.Star = true
	} else if !p.is(")") {
		c.Args = p.exprList()
	}
	p.expect(")")
	return c
}

func predicateStart(text string, next Token) bool {
	switch text {
	case "LIKE", "IN", "BETWEEN":
		return true
	case "NOT":
		return next.Kind == TokenKeyword && (next.Text == "LIKE" || next.Text == "IN" || next.Text == "BETWEEN")
	}
	return false
}

// predicate parses [NOT] LIKE, IN, and BETWEEN at comparison precedence.
// IN and BETWEEN desugar to comparisons so they keep three-valued NULL
// semantics and BETWEEN can use range seeks. LIKE becomes a Call that user SQL
// cannot spell, since LIKE is a keyword rather than a function identifier.
func (p *parser) predicate(left Expr) Expr {
	not := p.take("NOT")
	var x Expr
	switch {
	case p.take("LIKE"):
		args := []Expr{left, p.expression(5)}
		if p.takeWord("ESCAPE") {
			args = append(args, p.expression(5))
		}
		x = &Call{Name: "LIKE", Args: args}
	case p.take("IN"):
		p.expect("(")
		x = &Binary{Op: "=", Left: left, Right: p.expression(1)}
		for p.take(",") {
			x = &Binary{Op: "OR", Left: x, Right: &Binary{Op: "=", Left: left, Right: p.expression(1)}}
		}
		p.expect(")")
	case p.take("BETWEEN"):
		low := p.expression(5)
		p.expect("AND")
		high := p.expression(5)
		x = &Binary{Op: "AND", Left: &Binary{Op: ">=", Left: left, Right: low}, Right: &Binary{Op: "<=", Left: left, Right: high}}
	}
	if not {
		x = &Unary{Op: "NOT", X: x}
	}
	return x
}
