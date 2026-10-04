package gbase

import "context"

// TableInfo describes a table for tools such as exporters.
type TableInfo struct {
	Name    string
	Columns []ColumnInfo
	// Indexes lists CREATE INDEX indexes and multi-column UNIQUE constraints
	// (Constraint set). Single-column UNIQUE appears as ColumnInfo.Unique.
	Indexes []IndexInfo
	// NextID is the next automatic INTEGER PRIMARY KEY value. IDs are never
	// reused, so an exporter should start the target's identity here.
	NextID int64
}

// ColumnInfo.Type is the declared type: INTEGER, REAL, TEXT, BLOB, BOOLEAN,
// DATE, DATETIME, or TIMESTAMP. StorageType is one of the first four.
type ColumnInfo struct {
	Name, Type, StorageType     string
	PrimaryKey, Unique, NotNull bool
	HasDefault                  bool
	Default                     Value
	References                  *ForeignKeyInfo
}

// ForeignKeyInfo names the referenced column. gbase foreign keys are RESTRICT.
type ForeignKeyInfo struct{ Table, Column string }

type IndexInfo struct {
	Name               string
	Columns            []string
	Unique, Constraint bool
}

// Describe lists tables with every foreign key's target before the tables
// referencing it.
func (db *DB) Describe(ctx context.Context) ([]TableInfo, error) {
	tx, e := db.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	return tx.Describe()
}

// Describe is DB.Describe within the transaction, so it matches the data the
// transaction reads.
func (tx *Tx) Describe() ([]TableInfo, error) {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return nil, ErrClosed
	}
	tables, e := tx.orderedTables()
	if e != nil {
		return nil, e
	}
	out := make([]TableInfo, 0, len(tables))
	for _, t := range tables {
		info := TableInfo{Name: t.Name, NextID: t.NextID}
		for _, c := range t.Columns {
			ci := ColumnInfo{Name: c.Name, Type: c.typeName(), StorageType: c.Type, PrimaryKey: c.Primary, Unique: c.Unique, NotNull: c.NotNull}
			if c.Default != nil {
				v, e := decodeRecord(c.Default)
				if e != nil {
					return nil, e
				}
				ci.HasDefault, ci.Default = true, v[0]
			}
			if c.Ref != nil {
				ci.References = &ForeignKeyInfo{Table: c.Ref.Table, Column: c.Ref.Column}
			}
			info.Columns = append(info.Columns, ci)
		}
		for _, i := range tx.indexes(t) {
			if i.Automatic && len(i.Columns) == 0 {
				continue
			}
			info.Indexes = append(info.Indexes, IndexInfo{Name: i.Name, Columns: append([]string(nil), i.cols()...), Unique: i.Unique, Constraint: i.Automatic})
		}
		out = append(out, info)
	}
	return out, nil
}
