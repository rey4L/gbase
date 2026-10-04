package sql

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenKind describes a lexical token. Keywords and symbols use canonical Text.
type TokenKind uint8

const (
	TokenEOF TokenKind = iota
	TokenIdentifier
	TokenKeyword
	TokenLiteral
	TokenParameter
	TokenSymbol
)

// Token.Pos is a zero-based byte offset. Literal Value has the same representation
// as Literal.Value; parameter Value is its zero-based lexical index.
type Token struct {
	Kind  TokenKind
	Text  string
	Pos   int
	Value any
}

// Error reports a lexical or syntactic error at a zero-based byte offset.
type Error struct {
	Pos     int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("sql: byte %d: %s", e.Pos, e.Message) }

var keywords = func() map[string]bool {
	m := make(map[string]bool)
	// Function names are identifiers: parentheses distinguish calls from columns
	// and leave names such as avg available for aliases and schema objects.
	for _, k := range strings.Fields("CREATE DROP TABLE INDEX UNIQUE IF NOT EXISTS INSERT INTO VALUES SELECT AS FROM WHERE INNER LEFT OUTER JOIN ON DISTINCT ORDER BY ASC DESC LIMIT OFFSET GROUP HAVING UPDATE SET DELETE EXPLAIN PRIMARY KEY NULL DEFAULT REFERENCES RESTRICT INTEGER REAL TEXT BLOB AND OR IS FOREIGN CONSTRAINT CHECK AUTOINCREMENT CASCADE NO ACTION RIGHT FULL CROSS NATURAL USING UNION ALL INTERSECT EXCEPT LIKE IN BETWEEN COLLATE RETURNING REPLACE ALTER TRUE FALSE") {
		m[k] = true
	}
	return m
}()

// Lex tokenizes one SQL input, including an EOF token. It does not validate grammar.
func Lex(input string) ([]Token, error) {
	tokens := make([]Token, 0)
	param := 0
	fail := func(pos int, message string) ([]Token, error) { return nil, &Error{Pos: pos, Message: message} }
	for i := 0; i < len(input); {
		start := i
		r, size := utf8.DecodeRuneInString(input[i:])
		if r == utf8.RuneError && size == 1 {
			return fail(i, "invalid UTF-8")
		}
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		if strings.HasPrefix(input[i:], "--") {
			i += 2
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(input[i:], "/*") {
			end := strings.Index(input[i+2:], "*/")
			if end < 0 {
				return fail(i, "unterminated comment")
			}
			i += end + 4
			continue
		}
		// Strings, quoted identifiers, and hexadecimal blob literals.
		blob := (r == 'x' || r == 'X') && i+1 < len(input) && input[i+1] == '\''
		if r == '\'' || r == '"' || blob {
			quote := byte(r)
			if blob {
				i++
				quote = '\''
			}
			i++
			var b strings.Builder
			closed := false
			for i < len(input) {
				if input[i] == quote {
					i++
					if i < len(input) && input[i] == quote {
						b.WriteByte(quote)
						i++
						continue
					}
					closed = true
					break
				}
				rr, n := utf8.DecodeRuneInString(input[i:])
				if rr == utf8.RuneError && n == 1 {
					return fail(i, "invalid UTF-8")
				}
				b.WriteString(input[i : i+n])
				i += n
			}
			if !closed {
				return fail(start, "unterminated quoted value")
			}
			t := Token{Kind: TokenLiteral, Text: input[start:i], Pos: start, Value: b.String()}
			if blob {
				v, err := hex.DecodeString(b.String())
				if err != nil {
					return fail(start, "invalid hexadecimal blob")
				}
				t.Value = v
			} else if quote == '"' {
				if b.Len() == 0 {
					return fail(start, "empty quoted identifier")
				}
				t.Kind = TokenIdentifier
				t.Text = b.String()
				t.Value = nil
			}
			tokens = append(tokens, t)
			continue
		}
		if unicode.IsLetter(r) || r == '_' {
			i += size
			for i < len(input) {
				rr, n := utf8.DecodeRuneInString(input[i:])
				if !unicode.IsLetter(rr) && !unicode.IsDigit(rr) && rr != '_' {
					break
				}
				i += n
			}
			s := input[start:i]
			t := Token{Kind: TokenIdentifier, Text: s, Pos: start}
			upper := strings.ToUpper(s)
			if keywords[upper] {
				t.Kind = TokenKeyword
				t.Text = upper
			}
			tokens = append(tokens, t)
			continue
		}
		if r >= '0' && r <= '9' || r == '.' && i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9' {
			real := false
			for i < len(input) && input[i] >= '0' && input[i] <= '9' {
				i++
			}
			if i < len(input) && input[i] == '.' {
				real = true
				i++
				for i < len(input) && input[i] >= '0' && input[i] <= '9' {
					i++
				}
			}
			if i < len(input) && (input[i] == 'e' || input[i] == 'E') {
				real = true
				i++
				if i < len(input) && (input[i] == '+' || input[i] == '-') {
					i++
				}
				digits := i
				for i < len(input) && input[i] >= '0' && input[i] <= '9' {
					i++
				}
				if i == digits {
					return fail(start, "invalid numeric exponent")
				}
			}
			s := input[start:i]
			var value any
			if real {
				v, err := strconv.ParseFloat(s, 64)
				if err != nil {
					return fail(start, "real literal out of range")
				}
				value = v
			} else {
				v, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					// This magnitude is valid only after unary minus; parser handles that case.
					if s != "9223372036854775808" {
						return fail(start, "integer literal out of range")
					}
					value = uint64(1) << 63
				} else {
					value = v
				}
			}
			tokens = append(tokens, Token{Kind: TokenLiteral, Text: s, Pos: start, Value: value})
			continue
		}
		if r == '?' {
			tokens = append(tokens, Token{Kind: TokenParameter, Text: "?", Pos: i, Value: param})
			param++
			i++
			continue
		}
		if i+1 < len(input) {
			op := input[i : i+2]
			if op == "<=" || op == ">=" || op == "<>" || op == "!=" || op == "||" {
				tokens = append(tokens, Token{Kind: TokenSymbol, Text: op, Pos: i})
				i += 2
				continue
			}
		}
		if strings.ContainsRune("(),.;+-*/=<>%", r) {
			tokens = append(tokens, Token{Kind: TokenSymbol, Text: string(r), Pos: i})
			i += size
			continue
		}
		return fail(i, fmt.Sprintf("unexpected character %q", r))
	}
	tokens = append(tokens, Token{Kind: TokenEOF, Pos: len(input)})
	return tokens, nil
}
