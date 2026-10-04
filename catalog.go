package gbase

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/rey4L/gbase/internal/storage"
)

type (
	reference struct{ Table, Column string }
	column    struct {
		Name, Type               string
		Primary, Unique, NotNull bool
		Default                  []byte
		Ref                      *reference
	}
)

type table struct {
	Name      string
	Columns   []column
	Root      uint32
	NextID    int64
	Exhausted bool
}

// index.Column is the leading column. Columns lists every column of a
// composite index and is empty for single-column indexes, which keeps their
// catalog encoding unchanged.
type index struct {
	Name, Table, Column string
	Columns             []string `json:",omitempty"`
	Root                uint32
	Unique, Automatic   bool
}

func (i *index) cols() []string {
	if len(i.Columns) > 0 {
		return i.Columns
	}
	return []string{i.Column}
}

// single reports whether i indexes exactly the named column.
func (i *index) single(name string) bool {
	return len(i.Columns) == 0 && canon(i.Column) == canon(name)
}

// covers reports whether the named column is any column of i.
func (i *index) covers(name string) bool {
	for _, c := range i.cols() {
		if canon(c) == canon(name) {
			return true
		}
	}
	return false
}

// indexKey concatenates each column's order-preserving prefix, so a composite
// key sorts by its columns in order and a leading-column prefix is a key
// prefix. hasNull reports a NULL in any column, which exempts the row from
// uniqueness as in SQL.
func indexKey(t *table, i *index, values []Value) (key []byte, hasNull bool, err error) {
	for _, c := range i.cols() {
		v := values[t.col(c)]
		hasNull = hasNull || v == nil
		p, e := indexPrefix(v)
		if e != nil {
			return nil, false, e
		}
		key = append(key, p...)
	}
	if len(key)+8 > 1024 {
		return nil, false, fail("limit", "indexed value exceeds 1024-byte key limit")
	}
	return key, hasNull, nil
}

type catalog struct {
	Tables  map[string]*table
	Indexes map[string]*index
}

func emptyCatalog() *catalog { return &catalog{map[string]*table{}, map[string]*index{}} }
func canon(s string) string  { return strings.ToLower(s) }
func (t *table) col(name string) int {
	for i, c := range t.Columns {
		if canon(c.Name) == canon(name) {
			return i
		}
	}
	return -1
}

func (tx *Tx) loadCatalog() error {
	if tx.pages.Root() == 0 {
		tx.cat = emptyCatalog()
		return nil
	}
	tr := storage.Tree{Tx: tx.pages, Root: tx.pages.Root()}
	b, ok, e := tr.Get([]byte("catalog"))
	if e != nil {
		return e
	}
	if !ok {
		return fail("corrupt", "missing catalog")
	}
	tx.cat = emptyCatalog()
	if e = json.Unmarshal(b, tx.cat); e != nil {
		return fail("corrupt", "invalid catalog: %v", e)
	}
	if tx.cat.Tables == nil || tx.cat.Indexes == nil {
		return fail("corrupt", "missing schema maps")
	}
	return tx.validateCatalog()
}

func (tx *Tx) saveCatalog() error {
	root := tx.pages.Root()
	if root == 0 {
		var e error
		root, e = storage.CreateTree(tx.pages)
		if e != nil {
			return e
		}
	}
	b, e := json.Marshal(tx.cat)
	if e != nil {
		return e
	}
	tr := storage.Tree{Tx: tx.pages, Root: root}
	existing, found, err := tr.Get([]byte("catalog"))
	if err != nil {
		return err
	}
	if found && bytes.Equal(existing, b) {
		return nil
	}
	if e = tr.Put([]byte("catalog"), b); e != nil {
		return e
	}
	tx.pages.SetRoot(tr.Root)
	return nil
}

func cloneCatalog(c *catalog) *catalog {
	b, _ := json.Marshal(c)
	out := emptyCatalog()
	json.Unmarshal(b, out)
	return out
}

func (tx *Tx) getTable(name string) (*table, error) {
	t := tx.cat.Tables[canon(name)]
	if t == nil {
		return nil, fail("schema", "unknown table %s", name)
	}
	return t, nil
}

func (tx *Tx) validateCatalog() error {
	for name, t := range tx.cat.Tables {
		if t == nil || name != canon(t.Name) || t.Name == "" || t.Root == 0 || t.Root >= tx.pages.PageCount() || len(t.Columns) == 0 || t.NextID < 1 {
			return fail("corrupt", "invalid table metadata")
		}
		seen := map[string]bool{}
		primary := 0
		for _, c := range t.Columns {
			if c.Name == "" || seen[canon(c.Name)] {
				return fail("corrupt", "duplicate or empty column")
			}
			seen[canon(c.Name)] = true
			switch c.Type {
			case "INTEGER", "REAL", "TEXT", "BLOB":
			default:
				return fail("corrupt", "unknown column type")
			}
			if c.Primary {
				primary++
				if !c.Unique || !c.NotNull {
					return fail("corrupt", "invalid primary key flags")
				}
			}
			if c.Default != nil {
				v, e := decodeRecord(c.Default)
				if e != nil || len(v) != 1 {
					return fail("corrupt", "invalid column default")
				}
				if c.Primary && c.Type == "INTEGER" && v[0] == nil {
					continue
				}
				if _, e = coerce(c, v[0]); e != nil {
					return fail("corrupt", "invalid default type")
				}
			}
		}
		if primary > 1 {
			return fail("corrupt", "multiple primary keys")
		}
	}
	for name, i := range tx.cat.Indexes {
		if i == nil || name != canon(i.Name) || i.Root == 0 || i.Root >= tx.pages.PageCount() {
			return fail("corrupt", "invalid index metadata")
		}
		t := tx.cat.Tables[canon(i.Table)]
		if t == nil || len(i.Columns) == 1 || len(i.Columns) > 0 && i.Column != i.Columns[0] {
			return fail("corrupt", "invalid index metadata")
		}
		seen := map[string]bool{}
		for _, c := range i.cols() {
			if t.col(c) < 0 || seen[canon(c)] {
				return fail("corrupt", "index target missing")
			}
			seen[canon(c)] = true
		}
		if len(i.Columns) > 0 && i.Automatic && !i.Unique {
			return fail("corrupt", "invalid constraint index")
		}
	}
	for _, t := range tx.cat.Tables {
		for _, c := range t.Columns {
			if c.Unique && !(c.Primary && c.Type == "INTEGER") {
				found := false
				for _, i := range tx.indexes(t) {
					if i.Automatic && i.Unique && i.single(c.Name) {
						found = true
						break
					}
				}
				if !found {
					return fail("corrupt", "missing constraint index")
				}
			}
			if c.Ref != nil {
				parent := tx.cat.Tables[canon(c.Ref.Table)]
				if parent == nil {
					return fail("corrupt", "foreign table missing")
				}
				p := parent.col(c.Ref.Column)
				if p < 0 || !tx.uniqueColumn(parent, c.Ref.Column) || parent.Columns[p].Type != c.Type {
					return fail("corrupt", "invalid foreign key target")
				}
			}
		}
	}
	return nil
}
