package gbase

import (
	"strings"
	"time"
)

// declaredStorage maps each declared column type to the storage type that
// holds it. Declared types only validate and canonicalize values, so their
// columns sort, compare, and index exactly like their storage type.
var declaredStorage = map[string]string{"BOOLEAN": "INTEGER", "DATE": "TEXT", "DATETIME": "TEXT", "TIMESTAMP": "TEXT"}

// TimestampLayout is the canonical stored form of DATETIME and TIMESTAMP
// values: UTC with a fixed six-digit fraction, so text order is time order.
const TimestampLayout = "2006-01-02T15:04:05.000000Z"

// DateLayout is the canonical stored form of DATE values.
const DateLayout = "2006-01-02"

// FormatTimestamp returns t in the canonical DATETIME and TIMESTAMP form.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format(TimestampLayout)
}

func coerceDeclared(c column, v Value) (Value, error) {
	switch c.Declared {
	case "BOOLEAN":
		if n, ok := v.(int64); ok && (n == 0 || n == 1) {
			return n, nil
		}
		return nil, fail("type", "%s requires BOOLEAN 0 or 1", c.Name)
	case "DATE":
		if s, ok := v.(string); ok {
			if d, e := time.Parse(DateLayout, s); e == nil && d.Format(DateLayout) == s {
				return s, nil
			}
			// A timestamp at exactly UTC midnight names a date without losing anything.
			if t, ok := parseTimestamp(s); ok && t.Equal(t.Truncate(24*time.Hour)) {
				return t.Format(DateLayout), nil
			}
		}
		return nil, fail("type", "%s requires DATE as YYYY-MM-DD", c.Name)
	case "DATETIME", "TIMESTAMP":
		if s, ok := v.(string); ok {
			if t, ok := parseTimestamp(s); ok {
				return FormatTimestamp(t), nil
			}
		}
		return nil, fail("type", "%s requires %s as RFC 3339 or YYYY-MM-DD[ HH:MM:SS[.fff]]", c.Name, c.Declared)
	}
	return nil, fail("type", "unknown declared type %s", c.Declared)
}

// parseTimestamp accepts RFC 3339 with any offset, and the SQLite-style
// "YYYY-MM-DD HH:MM:SS[.fff]" and "YYYY-MM-DD" forms, which are taken as UTC.
func parseTimestamp(s string) (time.Time, bool) {
	if t, e := time.Parse(time.RFC3339Nano, s); e == nil {
		return t, true
	}
	if t, e := time.Parse("2006-01-02T15:04:05.999999999", s); e == nil {
		return t, true
	}
	if t, e := time.Parse("2006-01-02 15:04:05.999999999", s); e == nil {
		return t, true
	}
	if t, e := time.Parse(DateLayout, s); e == nil && !strings.ContainsAny(s, " T") {
		return t, true
	}
	return time.Time{}, false
}

// typeName is the column type as written in CREATE TABLE.
func (c column) typeName() string {
	if c.Declared != "" {
		return c.Declared
	}
	return c.Type
}
