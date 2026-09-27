package gbase

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/rey4L/gbase/internal/storage"
)

type reference struct{ Table, Column string }
type column struct {
	Name, Type               string
	Primary, Unique, NotNull bool
	Default                  []byte
	Ref                      *reference
}
type table struct {
	Name      string
	Columns   []column
	Root      uint32
	NextID    int64
	Exhausted bool
}
type index struct {
	Name, Table, Column string
	Root                uint32
	Unique, Automatic   bool
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
		if t == nil || t.col(i.Column) < 0 {
			return fail("corrupt", "index target missing")
		}
	}
	for _, t := range tx.cat.Tables {
		for _, c := range t.Columns {
			if c.Unique && !(c.Primary && c.Type == "INTEGER") {
				found := false
				for _, i := range tx.indexes(t) {
					if i.Automatic && i.Unique && canon(i.Column) == canon(c.Name) {
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
