package gbase

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/rey4L/gbase/internal/sql"
)

type evalBinding struct {
	name   string
	table  *table
	values []Value
}
type evalEnv struct {
	bindings []evalBinding
	group    []evalEnv
}

func rowEnv(t *table, values []Value) evalEnv {
	return evalEnv{bindings: []evalBinding{{name: t.Name, table: t, values: values}}}
}

func matches(x sql.Expr, t *table, values []Value, args []Value) (bool, error) {
	env := rowEnv(t, values)
	if e := bindExpr(x, env, false); e != nil {
		return false, e
	}
	if x == nil {
		return true, nil
	}
	v, e := eval(x, env, args)
	if e != nil {
		return false, e
	}
	return truth(v)
}

func resolveColumn(c *sql.Column, env evalEnv) (int, int, error) {
	bi, ci := -1, -1
	for i, b := range env.bindings {
		if c.Table != "" && canon(c.Table) != canon(b.name) {
			continue
		}
		j := b.table.col(c.Name)
		if j < 0 {
			continue
		}
		if bi >= 0 {
			return 0, 0, fail("schema", "ambiguous column %s", c.Name)
		}
		bi, ci = i, j
	}
	if bi < 0 {
		return 0, 0, fail("schema", "unknown column %s.%s", c.Table, c.Name)
	}
	return bi, ci, nil
}

func aggregate(name string) bool {
	switch strings.ToUpper(name) {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
		return true
	}
	return false
}

func bindExpr(x sql.Expr, env evalEnv, allowAggregate bool) error {
	if x == nil {
		return nil
	}
	switch e := x.(type) {
	case *sql.Column:
		_, _, err := resolveColumn(e, env)
		return err
	case *sql.Unary:
		return bindExpr(e.X, env, allowAggregate)
	case *sql.Binary:
		if err := bindExpr(e.Left, env, allowAggregate); err != nil {
			return err
		}
		return bindExpr(e.Right, env, allowAggregate)
	case *sql.Call:
		n := strings.ToUpper(e.Name)
		a := aggregate(n)
		if a && !allowAggregate {
			return fail("query", "aggregate %s not allowed here", n)
		}
		if a {
			if e.Star && n != "COUNT" || !e.Star && len(e.Args) != 1 {
				return fail("query", "invalid aggregate arguments")
			}
		} else {
			switch n {
			case "COALESCE":
				if len(e.Args) == 0 {
					return fail("query", "COALESCE needs arguments")
				}
			case "LOWER", "UPPER", "LENGTH", "ABS":
				if len(e.Args) != 1 {
					return fail("query", "%s needs one argument", n)
				}
			default:
				return fail("query", "unknown function %s", n)
			}
		}
		for _, arg := range e.Args {
			if err := bindExpr(arg, env, allowAggregate && !a); err != nil {
				return err
			}
		}
	case *sql.Star:
		return fail("query", "unexpected star")
	case *sql.Literal, *sql.Parameter:
	default:
		return fail("query", "unsupported expression %T", x)
	}
	return nil
}

func truth(v Value) (bool, error) {
	switch x := v.(type) {
	case nil:
		return false, nil
	case int64:
		return x != 0, nil
	case float64:
		return x != 0, nil
	default:
		return false, fail("type", "boolean requires number")
	}
}

func boolValue(b bool) Value {
	if b {
		return int64(1)
	}
	return int64(0)
}

// compareValues preserves all integer bits, including mixed integer/real comparisons.
func compareValues(a, b Value) (int, error) {
	if a == nil {
		if b == nil {
			return 0, nil
		}
		return -1, nil
	}
	if b == nil {
		return 1, nil
	}
	switch x := a.(type) {
	case int64:
		switch y := b.(type) {
		case int64:
			if x < y {
				return -1, nil
			}
			if x > y {
				return 1, nil
			}
			return 0, nil
		case float64:
			return compareNumbers(x, y), nil
		}
	case float64:
		switch y := b.(type) {
		case int64:
			return -compareNumbers(y, x), nil
		case float64:
			if x < y {
				return -1, nil
			}
			if x > y {
				return 1, nil
			}
			return 0, nil
		}
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y), nil
		}
	case []byte:
		if y, ok := b.([]byte); ok {
			return bytes.Compare(x, y), nil
		}
	}
	return 0, fail("type", "cannot compare %T and %T", a, b)
}

func compareNumbers(i int64, f float64) int {
	a := new(big.Rat).SetInt64(i)
	b := new(big.Rat).SetFloat64(f)
	if b == nil {
		if f > 0 {
			return -1
		}
		return 1
	}
	return a.Cmp(b)
}

func numeric(v Value) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func arithmetic(op string, a, b Value) (Value, error) {
	if a == nil || b == nil {
		return nil, nil
	}
	if x, ok := a.(int64); ok {
		if y, ok := b.(int64); ok {
			switch op {
			case "+", "-", "*":
				z := new(big.Int).SetInt64(x)
				switch op {
				case "+":
					z.Add(z, big.NewInt(y))
				case "-":
					z.Sub(z, big.NewInt(y))
				case "*":
					z.Mul(z, big.NewInt(y))
				}
				if !z.IsInt64() {
					return nil, fail("range", "integer overflow")
				}
				return z.Int64(), nil
			case "/":
				if y == 0 {
					return nil, fail("arithmetic", "division by zero")
				}
				if x == math.MinInt64 && y == -1 {
					return nil, fail("range", "integer overflow")
				}
				return x / y, nil
			case "%":
				if y == 0 {
					return nil, fail("arithmetic", "division by zero")
				}
				return x % y, nil
			}
		}
	}
	x, ok := numeric(a)
	y, ok2 := numeric(b)
	if !ok || !ok2 {
		return nil, fail("type", "arithmetic requires numbers")
	}
	var z float64
	switch op {
	case "+":
		z = x + y
	case "-":
		z = x - y
	case "*":
		z = x * y
	case "/":
		if y == 0 {
			return nil, fail("arithmetic", "division by zero")
		}
		z = x / y
	case "%":
		return nil, fail("type", "remainder requires integers")
	default:
		return nil, fail("query", "unknown operator %s", op)
	}
	if math.IsInf(z, 0) || math.IsNaN(z) {
		return nil, fail("range", "real overflow")
	}
	return z, nil
}

func eval(x sql.Expr, env evalEnv, args []Value) (Value, error) {
	if x == nil {
		return nil, nil
	}
	switch e := x.(type) {
	case *sql.Literal:
		return e.Value, nil
	case *sql.Parameter:
		if e.Index < 0 || e.Index >= len(args) {
			return nil, fail("parameter", "missing parameter %d", e.Index+1)
		}
		return args[e.Index], nil
	case *sql.Column:
		i, j, err := resolveColumn(e, env)
		if err != nil {
			return nil, err
		}
		if j >= len(env.bindings[i].values) {
			return nil, fail("query", "column has no row")
		}
		return env.bindings[i].values[j], nil
	case *sql.Unary:
		v, err := eval(e.X, env, args)
		if err != nil {
			return nil, err
		}
		switch strings.ToUpper(e.Op) {
		case "IS NULL":
			return boolValue(v == nil), nil
		case "IS NOT NULL":
			return boolValue(v != nil), nil
		case "NOT":
			if v == nil {
				return nil, nil
			}
			b, err := truth(v)
			return boolValue(!b), err
		case "+":
			if v == nil {
				return nil, nil
			}
			if _, ok := numeric(v); !ok {
				return nil, fail("type", "unary plus requires number")
			}
			return v, nil
		case "-":
			return arithmetic("-", int64(0), v)
		}
	case *sql.Binary:
		a, err := eval(e.Left, env, args)
		if err != nil {
			return nil, err
		}
		op := strings.ToUpper(e.Op)
		if op == "AND" || op == "OR" {
			av, err := truth(a)
			if err != nil {
				return nil, err
			}
			if a != nil && (op == "AND" && !av || op == "OR" && av) {
				return boolValue(av), nil
			}
			b, err := eval(e.Right, env, args)
			if err != nil {
				return nil, err
			}
			bv, err := truth(b)
			if err != nil {
				return nil, err
			}
			if op == "AND" {
				if b != nil && !bv {
					return int64(0), nil
				}
				if a == nil || b == nil {
					return nil, nil
				}
				return boolValue(av && bv), nil
			}
			if b != nil && bv {
				return int64(1), nil
			}
			if a == nil || b == nil {
				return nil, nil
			}
			return boolValue(av || bv), nil
		}
		b, err := eval(e.Right, env, args)
		if err != nil {
			return nil, err
		}
		switch op {
		case "+", "-", "*", "/", "%":
			return arithmetic(op, a, b)
		case "||":
			if a == nil || b == nil {
				return nil, nil
			}
			as, ok := a.(string)
			bs, ok2 := b.(string)
			if !ok || !ok2 {
				return nil, fail("type", "concatenation requires text")
			}
			return as + bs, nil
		case "=", "!=", "<>", "<", "<=", ">", ">=":
			if a == nil || b == nil {
				return nil, nil
			}
			c, err := compareValues(a, b)
			if err != nil {
				return nil, err
			}
			switch op {
			case "=":
				return boolValue(c == 0), nil
			case "!=", "<>":
				return boolValue(c != 0), nil
			case "<":
				return boolValue(c < 0), nil
			case "<=":
				return boolValue(c <= 0), nil
			case ">":
				return boolValue(c > 0), nil
			default:
				return boolValue(c >= 0), nil
			}
		}
	case *sql.Call:
		if aggregate(e.Name) {
			return evalAggregate(e, env, args)
		}
		var values []Value
		for _, arg := range e.Args {
			v, err := eval(arg, env, args)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
			if strings.EqualFold(e.Name, "COALESCE") && v != nil {
				return v, nil
			}
		}
		if strings.EqualFold(e.Name, "COALESCE") {
			return nil, nil
		}
		if len(values) != 1 {
			return nil, fail("query", "invalid function arguments")
		}
		v := values[0]
		if v == nil {
			return nil, nil
		}
		switch strings.ToUpper(e.Name) {
		case "LOWER", "UPPER":
			s, ok := v.(string)
			if !ok {
				return nil, fail("type", "text required")
			}
			if strings.EqualFold(e.Name, "LOWER") {
				return strings.ToLower(s), nil
			}
			return strings.ToUpper(s), nil
		case "LENGTH":
			switch s := v.(type) {
			case string:
				return int64(len([]rune(s))), nil
			case []byte:
				return int64(len(s)), nil
			}
		case "ABS":
			switch a := v.(type) {
			case int64:
				if a == math.MinInt64 {
					return nil, fail("range", "integer overflow")
				}
				if a < 0 {
					a = -a
				}
				return a, nil
			case float64:
				return math.Abs(a), nil
			}
		}
	}
	return nil, fail("query", "unsupported expression %T", x)
}

func evalAggregate(c *sql.Call, env evalEnv, args []Value) (Value, error) {
	if env.group == nil {
		return nil, fail("query", "aggregate outside group")
	}
	name := strings.ToUpper(c.Name)
	var n int64
	var result Value
	for _, row := range env.group {
		var v Value
		var err error
		if c.Star {
			v = int64(1)
		} else {
			if len(c.Args) != 1 {
				return nil, fail("query", "invalid aggregate")
			}
			v, err = eval(c.Args[0], row, args)
			if err != nil {
				return nil, err
			}
		}
		if v == nil {
			continue
		}
		n++
		switch name {
		case "COUNT":
		case "SUM", "AVG":
			if _, ok := numeric(v); !ok {
				return nil, fail("type", "aggregate requires numbers")
			}
			if result == nil {
				result = v
			} else {
				result, err = arithmetic("+", result, v)
				if err != nil {
					return nil, err
				}
			}
		case "MIN", "MAX":
			if result == nil {
				result = v
			} else {
				cmp, err := compareValues(v, result)
				if err != nil {
					return nil, err
				}
				if name == "MIN" && cmp < 0 || name == "MAX" && cmp > 0 {
					result = v
				}
			}
		}
	}
	if name == "COUNT" {
		return n, nil
	}
	if name == "AVG" && n > 0 {
		f, _ := numeric(result)
		return f / float64(n), nil
	}
	return result, nil
}

func exprName(x sql.Expr) string {
	switch e := x.(type) {
	case *sql.Column:
		return e.Name
	case *sql.Call:
		return strings.ToUpper(e.Name)
	case *sql.Literal:
		return fmt.Sprint(e.Value)
	case *sql.Parameter:
		return "?"
	case *sql.Unary:
		return e.Op + " " + exprName(e.X)
	case *sql.Binary:
		return exprName(e.Left) + " " + e.Op + " " + exprName(e.Right)
	}
	return "expression"
}
