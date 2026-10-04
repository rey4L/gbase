package sql

import (
	"reflect"
	"testing"
)

func TestLex(t *testing.T) {
	tokens, err := Lex(`select "select", 'a''b', X'00ff', 12, .5, 2e-3, ?, ? -- ?
 /* ? */ FROM café WHERE a<=1 AND b!=2;`)
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].Kind != TokenKeyword || tokens[0].Text != "SELECT" || tokens[1].Kind != TokenIdentifier || tokens[1].Text != "select" {
		t.Fatal(tokens[:2])
	}
	parameters := 0
	for i, tok := range tokens {
		if tok.Kind == TokenParameter {
			if tok.Value != parameters {
				t.Fatal(tok)
			}
			parameters++
		}
		if i > 0 && tok.Pos <= tokens[i-1].Pos {
			t.Fatal("non-increasing offsets")
		}
	}
	if parameters != 2 || tokens[len(tokens)-1].Kind != TokenEOF {
		t.Fatal(tokens)
	}
	for _, test := range []struct {
		text  string
		value any
	}{{"0", int64(0)}, {"1.0", float64(1)}, {"'a''b'", "a'b"}, {"X'00ff'", []byte{0, 255}}} {
		ts, err := Lex(test.text)
		if err != nil || !reflect.DeepEqual(ts[0].Value, test.value) {
			t.Fatalf("%q: %#v %v", test.text, ts, err)
		}
	}
}

func TestFunctionNameTokens(t *testing.T) {
	tokens, err := Lex("count sum avg min max AvG lower COALESCE")
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"count", "sum", "avg", "min", "max", "AvG", "lower", "COALESCE"} {
		if tokens[i].Kind != TokenIdentifier || tokens[i].Text != name {
			t.Fatalf("function name must remain an identifier: %#v", tokens[i])
		}
	}
}

func FuzzLex(f *testing.F) {
	for _, s := range []string{"", "SELECT ?,'?',X'ff' -- ?", "/* ? */ SELECT \"a\"\"b\"", "1e-2 .5 9223372036854775808", "\xff", "\x00", "'unterminated"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, input string) {
		tokens, err := Lex(input)
		if err != nil {
			if tokens != nil {
				t.Fatal("tokens returned with error")
			}
			return
		}
		if len(tokens) == 0 || tokens[len(tokens)-1].Kind != TokenEOF || tokens[len(tokens)-1].Pos != len(input) {
			t.Fatal("missing EOF")
		}
		parameter := 0
		for i, tok := range tokens {
			if tok.Pos < 0 || tok.Pos > len(input) || i > 0 && tok.Pos <= tokens[i-1].Pos {
				t.Fatal("invalid token offsets")
			}
			if tok.Kind == TokenEOF && i != len(tokens)-1 {
				t.Fatal("early EOF")
			}
			if tok.Kind == TokenParameter {
				if tok.Value != parameter {
					t.Fatal("parameter indexes not lexical")
				}
				parameter++
			}
		}
		again, err := Lex(input)
		if err != nil || !reflect.DeepEqual(tokens, again) {
			t.Fatal("non-deterministic lexer")
		}
	})
}

func TestNamedParameters(t *testing.T) {
	tokens, err := Lex("SELECT :a, @b, $a, :c1 FROM t WHERE x = ':no' -- :no\n")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	var indexes []int
	for _, tok := range tokens {
		if tok.Kind == TokenParameter {
			got = append(got, tok.Text)
			indexes = append(indexes, tok.Value.(int))
		}
	}
	if !reflect.DeepEqual(got, []string{":a", "@b", "$a", ":c1"}) || !reflect.DeepEqual(indexes, []int{0, 1, 0, 2}) {
		t.Fatalf("%q %v", got, indexes)
	}
	for _, bad := range []string{"SELECT ?, :a", "SELECT :a, ?", "SELECT :", "SELECT :1", "SELECT @ x"} {
		if _, err := Lex(bad); err == nil {
			t.Errorf("%q lexed", bad)
		}
	}
	s, err := Parse("SELECT * FROM t WHERE a = :x OR b = :x LIMIT :n")
	if err != nil || ParameterCount(s) != 2 {
		t.Fatalf("%v %v", err, s)
	}
}
