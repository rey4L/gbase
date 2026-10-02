// Package sql parses the SQL subset supported by gbase without external dependencies.
package sql

// Statement is a parsed SQL statement. All implementations are pointers.
type Statement interface{ statement() }

// Expr is a SQL expression. All implementations are pointers.
type Expr interface{ expr() }

type (
	Literal   struct{ Value any }
	Column    struct{ Table, Name string }
	Parameter struct{ Index int }
	Unary     struct {
		Op string
		X  Expr
	}
)

type Binary struct {
	Op          string
	Left, Right Expr
}
type Call struct {
	Name string
	Args []Expr
	Star bool
}
type Star struct{ Table string }

func (*Literal) expr()   {}
func (*Column) expr()    {}
func (*Parameter) expr() {}
func (*Unary) expr()     {}
func (*Binary) expr()    {}
func (*Call) expr()      {}
func (*Star) expr()      {}

// ForeignKey names a single referenced column. Actions are empty or "RESTRICT".
type ForeignKey struct{ Table, Column, OnDelete, OnUpdate string }

// ColumnDef.Type is INTEGER, REAL, TEXT, or BLOB. A nil Default means absent;
// an explicit DEFAULT NULL is represented by &Literal{Value: nil}.
type ColumnDef struct {
	Name, Type                  string
	PrimaryKey, Unique, NotNull bool
	Default                     Expr
	References                  *ForeignKey
}

// TableConstraint.Kind is PRIMARY KEY, UNIQUE, or FOREIGN KEY.
// Constraints retain source form; executors should process both Columns and Constraints.
type TableConstraint struct {
	Kind, Column string
	References   *ForeignKey
}
type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	Constraints []TableConstraint
}
type DropTable struct {
	Name     string
	IfExists bool
}
type CreateIndex struct {
	Name, Table         string
	Columns             []string
	Unique, IfNotExists bool
}
type DropIndex struct {
	Name             string
	IfExists, Unique bool
}
type Insert struct {
	Table   string
	Columns []string
	Rows    [][]Expr
}
type SelectItem struct {
	Expr  Expr
	Alias string
}
type TableRef struct{ Name, Alias string }

// Join.Type is INNER or LEFT.
type Join struct {
	Type  string
	Table TableRef
	On    Expr
}
type OrderTerm struct {
	Expr Expr
	Desc bool
}

// Limit and Offset are expressions (integer literals or parameters), nil when absent.
// From.Name is empty for SELECT without FROM.
type Select struct {
	Distinct      bool
	Columns       []SelectItem
	From          TableRef
	Joins         []Join
	Where         Expr
	GroupBy       []Expr
	Having        Expr
	OrderBy       []OrderTerm
	Limit, Offset Expr
}
type Assignment struct {
	Column string
	Value  Expr
}
type Update struct {
	Table       string
	Assignments []Assignment
	Where       Expr
}
type Delete struct {
	Table string
	Where Expr
}
type Explain struct{ Statement Statement }

func (*CreateTable) statement() {}
func (*DropTable) statement()   {}
func (*CreateIndex) statement() {}
func (*DropIndex) statement()   {}
func (*Insert) statement()      {}
func (*Select) statement()      {}
func (*Update) statement()      {}
func (*Delete) statement()      {}
func (*Explain) statement()     {}
