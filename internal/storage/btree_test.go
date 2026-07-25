package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
)

func newTestTree(t *testing.T) (*Pager, *Tree) {
	t.Helper()
	p, e := Open(filepath.Join(t.TempDir(), "tree.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	tx, e := p.Begin()
	if e != nil {
		t.Fatal(e)
	}
	root, e := CreateTree(tx)
	if e != nil {
		t.Fatal(e)
	}
	return p, &Tree{Tx: tx, Root: root}
}
func assertTree(t *testing.T, tr *Tree, want map[string][]byte) {
	t.Helper()
	pages, e := tr.Pages()
	if e != nil {
		t.Fatal(e)
	}
	owned := make(map[uint32]bool, len(pages))
	for _, id := range pages {
		owned[id] = true
	}
	for id := uint32(1); id < tr.Tx.PageCount(); id++ {
		_, readErr := tr.Tx.Read(id)
		if (readErr == nil) != owned[id] {
			t.Fatalf("page %d leaked or missing from ownership", id)
		}
	}
	c, e := tr.Scan(nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	i := 0
	for c.Next() {
		if i >= len(keys) {
			t.Fatal("extra scan record")
		}
		k := keys[i]
		if !bytes.Equal(c.Key(), []byte(k)) || !bytes.Equal(c.Value(), want[k]) {
			t.Fatalf("scan mismatch at %d: key %x want %x", i, c.Key(), k)
		}
		v, ok, e := tr.Get([]byte(k))
		if e != nil || !ok || !bytes.Equal(v, want[k]) {
			t.Fatalf("get mismatch %x: %v %v", k, ok, e)
		}
		i++
	}
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
	if i != len(keys) {
		t.Fatalf("scan length %d want %d", i, len(keys))
	}
}
func TestBTreeRandomized(t *testing.T) {
	_, tr := newTestTree(t)
	rng := rand.New(rand.NewSource(781))
	want := map[string][]byte{}
	keys := make([][]byte, 480)
	for i := range keys {
		k := make([]byte, 2+rng.Intn(45))
		rng.Read(k)
		binary.BigEndian.PutUint16(k, uint16(i))
		if i%19 == 0 {
			k = append(k, bytes.Repeat([]byte{byte(i)}, MaxKeySize-len(k))...)
		}
		keys[i] = k
	}
	keys[0] = []byte{}
	for step := 0; step < 2400; step++ {
		k := keys[rng.Intn(len(keys))]
		if rng.Intn(4) == 0 {
			if e := tr.Delete(k); e != nil {
				t.Fatalf("delete step %d: %v", step, e)
			}
			delete(want, string(k))
		} else {
			length := rng.Intn(620)
			if step%13 == 0 {
				length = 8000 + rng.Intn(23000)
			}
			v := make([]byte, length)
			rng.Read(v)
			if e := tr.Put(k, v); e != nil {
				t.Fatalf("put step %d: %v", step, e)
			}
			want[string(k)] = bytes.Clone(v)
			if len(v) > 0 {
				v[0] ^= 255
			}
		}
		if step%47 == 0 {
			assertTree(t, tr, want)
		}
	}
	assertTree(t, tr, want)
	order := rng.Perm(len(keys))
	for i, j := range order {
		if e := tr.Delete(keys[j]); e != nil {
			t.Fatal(e)
		}
		delete(want, string(keys[j]))
		if i%31 == 0 {
			assertTree(t, tr, want)
		}
	}
	assertTree(t, tr, want)
	n, e := tr.readNode(tr.Root)
	if e != nil || n.kind != leafPage || len(n.records) != 0 {
		t.Fatalf("root did not collapse: %v", e)
	}
	pages, e := tr.Pages()
	if e != nil || len(pages) != 1 {
		t.Fatalf("empty ownership: %v %v", pages, e)
	}
}
func TestBTreeDeepSplitsMerges(t *testing.T) {
	_, tr := newTestTree(t)
	rng := rand.New(rand.NewSource(92))
	want := map[string][]byte{}
	keys := make([][]byte, 350)
	initial := tr.Root
	for i := range keys {
		keys[i] = bytes.Repeat([]byte{byte(i)}, MaxKeySize)
		binary.BigEndian.PutUint32(keys[i], uint32(i))
	}
	for _, i := range rng.Perm(len(keys)) {
		v := bytes.Repeat([]byte{byte(i)}, inlineLimit)
		if e := tr.Put(keys[i], v); e != nil {
			t.Fatal(e)
		}
		want[string(keys[i])] = v
	}
	if tr.Root == initial {
		t.Fatal("root did not split")
	}
	depth := 0
	id := tr.Root
	for {
		n, e := tr.readNode(id)
		if e != nil {
			t.Fatal(e)
		}
		if n.kind == leafPage {
			break
		}
		depth++
		id = n.records[0].child
	}
	if depth < 3 {
		t.Fatalf("depth %d", depth)
	}
	assertTree(t, tr, want)
	for j, i := range rng.Perm(len(keys)) {
		if e := tr.Delete(keys[i]); e != nil {
			t.Fatalf("delete %d: %v", j, e)
		}
		delete(want, string(keys[i]))
		if j%7 == 0 {
			assertTree(t, tr, want)
		}
	}
	assertTree(t, tr, want)
}
func TestBTreeReopenOverflowOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	p, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { p.Close() }()
	tx, e := p.Begin()
	if e != nil {
		t.Fatal(e)
	}
	root, e := CreateTree(tx)
	if e != nil {
		t.Fatal(e)
	}
	tr := &Tree{Tx: tx, Root: root}
	want := map[string][]byte{}
	for i := 0; i < 160; i++ {
		k := fmt.Sprintf("key-%04d", i)
		v := bytes.Repeat([]byte{byte(i)}, i*173)
		if e = tr.Put([]byte(k), v); e != nil {
			t.Fatal(e)
		}
		want[k] = v
	}
	tx.SetRoot(tr.Root)
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	p, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	tx, e = p.Begin()
	if e != nil {
		t.Fatal(e)
	}
	tr = &Tree{Tx: tx, Root: tx.Root()}
	assertTree(t, tr, want)
	for i := 0; i < 160; i += 2 {
		k := fmt.Sprintf("key-%04d", i)
		if e = tr.Delete([]byte(k)); e != nil {
			t.Fatal(e)
		}
		delete(want, k)
	}
	assertTree(t, tr, want)
	pages, e := tr.Pages()
	if e != nil {
		t.Fatal(e)
	}
	before := tx.PageCount()
	if e = tr.Destroy(); e != nil {
		t.Fatal(e)
	}
	for _, id := range pages {
		if _, e = tx.Read(id); e == nil {
			t.Fatalf("page %d not freed", id)
		}
	}
	tx.SetRoot(0)
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	p, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	tx, e = p.Begin()
	if e != nil {
		t.Fatal(e)
	}
	newRoot, e := CreateTree(tx)
	if e != nil {
		t.Fatal(e)
	}
	if tx.PageCount() > before {
		t.Fatal("freed pages not reused")
	}
	fresh := &Tree{Tx: tx, Root: newRoot}
	assertTree(t, fresh, map[string][]byte{})
}
func TestBTreeBinaryScanBounds(t *testing.T) {
	_, tr := newTestTree(t)
	keys := [][]byte{{}, {0}, {0, 0}, {0, 255}, {1}, {1, 0}, {255}, {255, 255}}
	for _, k := range keys {
		if e := tr.Put(k, k); e != nil {
			t.Fatal(e)
		}
	}
	bounds := [][]byte{nil, {}, {0}, {0, 1}, {1}, {1, 0}, {128}, {255}, {255, 255}, {255, 255, 0}}
	for _, start := range bounds {
		for _, end := range bounds {
			c, e := tr.Scan(start, end)
			if e != nil {
				t.Fatal(e)
			}
			var want [][]byte
			for _, k := range keys {
				if (start == nil || bytes.Compare(k, start) >= 0) && (end == nil || bytes.Compare(k, end) < 0) {
					want = append(want, k)
				}
			}
			i := 0
			for c.Next() {
				if i >= len(want) || !bytes.Equal(c.Key(), want[i]) {
					t.Fatalf("bounds %x/%x at %d", start, end, i)
				}
				i++
			}
			if c.Err() != nil || i != len(want) {
				t.Fatalf("bounds %x/%x count %d want %d err %v", start, end, i, len(want), c.Err())
			}
		}
	}
}
func TestBTreeKeyLimit(t *testing.T) {
	_, tr := newTestTree(t)
	key := make([]byte, MaxKeySize+1)
	calls := []func() error{func() error { return tr.Put(key, nil) }, func() error { _, _, e := tr.Get(key); return e }, func() error { return tr.Delete(key) }, func() error { _, e := tr.Scan(key, nil); return e }, func() error { _, e := tr.Scan(nil, key); return e }}
	for _, call := range calls {
		e := call()
		var typed *KeyTooLargeError
		if !errors.As(e, &typed) || typed.Size != len(key) {
			t.Fatalf("wrong size error %v", e)
		}
	}
	if e := tr.Put(key[:MaxKeySize], nil); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := tr.Get(key[:MaxKeySize]); e != nil || !ok {
		t.Fatalf("max key: %v %v", ok, e)
	}
}
func TestBTreeCorruption(t *testing.T) {
	for _, mode := range []string{"checksum", "slot", "leaf-cycle", "overflow-cycle", "overflow-size", "shared-child", "separator", "child-cycle"} {
		t.Run(mode, func(t *testing.T) {
			_, tr := newTestTree(t)
			for i := 0; i < 260; i++ {
				if e := tr.Put([]byte(fmt.Sprintf("%04d", i)), bytes.Repeat([]byte{byte(i)}, 6000)); e != nil {
					t.Fatal(e)
				}
			}
			id, n, e := tr.find([]byte("0000"))
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "checksum", "slot":
				p, e := tr.Tx.Read(id)
				if e != nil {
					t.Fatal(e)
				}
				if mode == "checksum" {
					p[30] ^= 127
				} else {
					binary.LittleEndian.PutUint16(p[treeHeader:], 1)
					sealTreePage(p)
				}
				if e = tr.Tx.Write(id, p); e != nil {
					t.Fatal(e)
				}
			case "leaf-cycle":
				n.next = id
				if e = tr.writeNode(id, n); e != nil {
					t.Fatal(e)
				}
			case "overflow-cycle":
				oid := n.records[0].overflow
				p, e := tr.Tx.Read(oid)
				if e != nil {
					t.Fatal(e)
				}
				binary.LittleEndian.PutUint32(p[8:12], oid)
				sealTreePage(p)
				if e = tr.Tx.Write(oid, p); e != nil {
					t.Fatal(e)
				}
			case "overflow-size":
				n.records[0].size++
				if e = tr.writeNode(id, n); e != nil {
					t.Fatal(e)
				}
			default:
				root, e := tr.readNode(tr.Root)
				if e != nil {
					t.Fatal(e)
				}
				if root.kind != branchPage {
					t.Fatal("expected branch")
				}
				switch mode {
				case "shared-child":
					root.records[1].child = root.records[0].child
				case "separator":
					root.records[0].key = []byte("!")
				case "child-cycle":
					root.records[0].child = tr.Root
				}
				if e = tr.writeNode(tr.Root, root); e != nil {
					t.Fatal(e)
				}
			}
			if e = tr.Check(); !errors.Is(e, ErrTreeCorrupt) {
				t.Fatalf("Check did not detect %s: %v", mode, e)
			}
			if e = tr.Destroy(); !errors.Is(e, ErrTreeCorrupt) {
				t.Fatalf("Destroy did not reject corruption: %v", e)
			}
			if mode == "leaf-cycle" || mode == "overflow-cycle" || mode == "overflow-size" || mode == "checksum" || mode == "slot" || mode == "child-cycle" {
				c, e := tr.Scan(nil, nil)
				if e == nil {
					for c.Next() {
					}
					e = c.Err()
				}
				if !errors.Is(e, ErrTreeCorrupt) {
					t.Fatalf("Scan failed to detect %s: %v", mode, e)
				}
			}
		})
	}
}
func TestBTreeOverflowBoundaries(t *testing.T) {
	_, tr := newTestTree(t)
	sizes := []int{0, 1, inlineLimit, inlineLimit + 1, treeEnd - treeHeader, treeEnd - treeHeader + 1, 2 * (treeEnd - treeHeader), 2*(treeEnd-treeHeader) + 1, 1 << 20}
	for _, size := range sizes {
		value := bytes.Repeat([]byte{0, 255, 42}, (size+2)/3)[:size]
		if e := tr.Put([]byte("same"), value); e != nil {
			t.Fatal(e)
		}
		got, ok, e := tr.Get([]byte("same"))
		if e != nil || !ok || !bytes.Equal(got, value) {
			t.Fatalf("size %d: %v", size, e)
		}
		if e = tr.Check(); e != nil {
			t.Fatal(e)
		}
	}
	if e := tr.Delete([]byte("same")); e != nil {
		t.Fatal(e)
	}
	pages, e := tr.Pages()
	if e != nil || len(pages) != 1 {
		t.Fatalf("overflow leak: %v %v", pages, e)
	}
}

// Removing a short minimum can enlarge a separator enough to split its parent.
func TestBTreeDeleteSeparatorGrowth(t *testing.T) {
	_, tr := newTestTree(t)
	root := &treeNode{kind: branchPage, next: noPage}
	const children = 180
	ids := make([]uint32, children)
	for i := range ids {
		id, e := tr.Tx.Alloc()
		if e != nil {
			t.Fatal(e)
		}
		ids[i] = id
	}
	want := make(map[string][]byte)
	for i, id := range ids {
		short := make([]byte, 4)
		binary.BigEndian.PutUint32(short, uint32(i))
		long := append(bytes.Clone(short), bytes.Repeat([]byte{255}, MaxKeySize-4)...)
		next := noPage
		if i+1 < len(ids) {
			next = ids[i+1]
		}
		leaf := &treeNode{kind: leafPage, next: next, records: []treeRecord{
			{key: short, overflow: noPage}, {key: long, overflow: noPage},
		}}
		if e := tr.writeNode(id, leaf); e != nil {
			t.Fatal(e)
		}
		root.records = append(root.records, treeRecord{key: short, child: id})
		want[string(short)] = nil
		want[string(long)] = nil
	}
	if e := tr.writeNode(tr.Root, root); e != nil {
		t.Fatal(e)
	}
	assertTree(t, tr, want)
	// Underfull leaves merge first. The growing branch separators must still fit.
	for i := 0; i < children; i++ {
		short := make([]byte, 4)
		binary.BigEndian.PutUint32(short, uint32(i))
		if e := tr.Delete(short); e != nil {
			t.Fatalf("delete %d: %v", i, e)
		}
		delete(want, string(short))
		if i%9 == 0 {
			assertTree(t, tr, want)
		}
	}
	assertTree(t, tr, want)
}
