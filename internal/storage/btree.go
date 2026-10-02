package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
)

const (
	MaxKeySize  = 1024
	treeHeader  = 24
	treeEnd     = PageSize - 4
	inlineLimit = 512
	noPage      = ^uint32(0)
)

const (
	leafPage     byte = 1
	branchPage   byte = 2
	overflowPage byte = 3
)

var (
	ErrTreeCorrupt   = errors.New("corrupt B+ tree")
	ErrValueTooLarge = errors.New("B+ tree value exceeds uint32 length")
)

// KeyTooLargeError reports a key that cannot fit within the tree's key limit.
type KeyTooLargeError struct{ Size int }

func (e *KeyTooLargeError) Error() string {
	return fmt.Sprintf("B+ tree key size %d exceeds %d", e.Size, MaxKeySize)
}

// Tree is a transaction-bound B+ tree. Persist Root after successful mutations.
// Operations require exclusive access to the transaction. Cursors are invalidated by mutations.
type Tree struct {
	Tx   *Tx
	Root uint32
}
type treeRecord struct {
	key, value      []byte
	child, overflow uint32
	size            uint32
}
type treeNode struct {
	kind    byte
	next    uint32
	records []treeRecord
}

func corrupt(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrTreeCorrupt, fmt.Sprintf(format, a...))
}

func checkKey(k []byte) error {
	if len(k) > MaxKeySize {
		return &KeyTooLargeError{len(k)}
	}
	return nil
}

func nodeSize(n *treeNode) int {
	s := treeHeader
	for _, r := range n.records {
		s += 4 + 6 + len(r.key)
		if n.kind == leafPage {
			s += 4
			if r.overflow == noPage {
				s += len(r.value)
			}
		}
	}
	return s
}

func treeChecksum(p []byte) uint32 {
	h := crc32.NewIEEE()
	h.Write(p[:16])
	h.Write([]byte{0, 0, 0, 0})
	h.Write(p[20:treeEnd])
	return h.Sum32()
}

func pageBytes(kind byte) []byte {
	p := make([]byte, PageSize)
	copy(p, "GBT1")
	p[4] = kind
	p[5] = 1
	return p
}
func sealTreePage(p []byte) { binary.LittleEndian.PutUint32(p[16:20], treeChecksum(p)) }
func (t *Tree) readPage(id uint32, kind byte) ([]byte, error) {
	if t.Tx == nil || id == noPage {
		return nil, corrupt("invalid page %d", id)
	}
	p, e := t.Tx.Read(id)
	if e != nil {
		return nil, fmt.Errorf("%w: read page %d: %w", ErrTreeCorrupt, id, e)
	}
	if len(p) != PageSize || string(p[:4]) != "GBT1" || p[5] != 1 || (kind != 0 && p[4] != kind) || binary.LittleEndian.Uint32(p[16:20]) != treeChecksum(p) {
		return nil, corrupt("invalid header/checksum on page %d", id)
	}
	return p, nil
}

func (t *Tree) readNode(id uint32) (*treeNode, error) {
	p, e := t.readPage(id, 0)
	if e != nil {
		return nil, e
	}
	kind := p[4]
	if kind != leafPage && kind != branchPage {
		return nil, corrupt("page %d is not a node", id)
	}
	n := &treeNode{kind: kind, next: binary.LittleEndian.Uint32(p[8:12])}
	count := int(binary.LittleEndian.Uint16(p[6:8]))
	if count > (treeEnd-treeHeader)/4 {
		return nil, corrupt("slot count")
	}
	end := treeEnd
	for i := 0; i < count; i++ {
		slot := p[treeHeader+4*i : treeHeader+4*i+4]
		off := int(binary.LittleEndian.Uint16(slot))
		size := int(binary.LittleEndian.Uint16(slot[2:]))
		if off < treeHeader+4*count || size < 6 || off+size != end {
			return nil, corrupt("invalid slot on page %d", id)
		}
		end = off
		b := p[off : off+size]
		kl := int(binary.LittleEndian.Uint16(b))
		fixed := 6
		if kind == leafPage {
			fixed = 10
		}
		if kl > MaxKeySize || len(b) < fixed+kl {
			return nil, corrupt("invalid record")
		}
		r := treeRecord{key: bytes.Clone(b[fixed : fixed+kl]), overflow: noPage}
		if kind == branchPage {
			if len(b) != fixed+kl {
				return nil, corrupt("branch record length")
			}
			r.child = binary.LittleEndian.Uint32(b[2:6])
			if r.child == noPage {
				return nil, corrupt("invalid child")
			}
		} else {
			r.size = binary.LittleEndian.Uint32(b[2:6])
			r.overflow = binary.LittleEndian.Uint32(b[6:10])
			if r.overflow == noPage {
				if uint64(len(b)-fixed-kl) != uint64(r.size) || r.size > inlineLimit {
					return nil, corrupt("inline value length")
				}
				r.value = bytes.Clone(b[fixed+kl:])
			} else if len(b) != fixed+kl || r.size <= inlineLimit {
				return nil, corrupt("overflow record length")
			}
		}
		if i > 0 && bytes.Compare(n.records[i-1].key, r.key) >= 0 {
			return nil, corrupt("unordered keys")
		}
		n.records = append(n.records, r)
	}
	if kind == branchPage && (count == 0 || n.next != noPage) {
		return nil, corrupt("invalid branch")
	}
	return n, nil
}

func (t *Tree) writeNode(id uint32, n *treeNode) error {
	if nodeSize(n) > treeEnd {
		return errors.New("B+ tree node does not fit")
	}
	p := pageBytes(n.kind)
	binary.LittleEndian.PutUint16(p[6:8], uint16(len(n.records)))
	binary.LittleEndian.PutUint32(p[8:12], n.next)
	end := treeEnd
	for i, r := range n.records {
		fixed := 6
		size := 6 + len(r.key)
		if n.kind == leafPage {
			fixed = 10
			size += 4
			if r.overflow == noPage {
				size += len(r.value)
			}
		}
		end -= size
		b := p[end : end+size]
		binary.LittleEndian.PutUint16(b, uint16(len(r.key)))
		if n.kind == branchPage {
			binary.LittleEndian.PutUint32(b[2:6], r.child)
		} else {
			binary.LittleEndian.PutUint32(b[2:6], r.size)
			binary.LittleEndian.PutUint32(b[6:10], r.overflow)
			copy(b[fixed+len(r.key):], r.value)
		}
		copy(b[fixed:], r.key)
		slot := p[treeHeader+4*i:]
		binary.LittleEndian.PutUint16(slot, uint16(end))
		binary.LittleEndian.PutUint16(slot[2:], uint16(size))
	}
	sealTreePage(p)
	return t.Tx.Write(id, p)
}

func CreateTree(tx *Tx) (uint32, error) {
	if tx == nil {
		return 0, errors.New("nil transaction")
	}
	id, e := tx.Alloc()
	if e != nil {
		return 0, e
	}
	t := Tree{tx, id}
	if e = t.writeNode(id, &treeNode{kind: leafPage, next: noPage}); e != nil {
		return 0, e
	}
	return id, nil
}

func childIndex(n *treeNode, k []byte) int {
	i := sort.Search(len(n.records), func(i int) bool { return bytes.Compare(n.records[i].key, k) > 0 }) - 1
	if i < 0 {
		i = 0
	}
	return i
}

func recordIndex(n *treeNode, k []byte) int {
	return sort.Search(len(n.records), func(i int) bool { return bytes.Compare(n.records[i].key, k) >= 0 })
}

func (t *Tree) find(k []byte) (uint32, *treeNode, error) {
	id := t.Root
	seen := map[uint32]bool{}
	for {
		if seen[id] {
			return 0, nil, corrupt("node cycle")
		}
		seen[id] = true
		n, e := t.readNode(id)
		if e != nil {
			return 0, nil, e
		}
		if n.kind == leafPage {
			return id, n, nil
		}
		id = n.records[childIndex(n, k)].child
	}
}

func (t *Tree) overflow(r treeRecord, visit func(uint32) error) ([]byte, error) {
	if r.overflow == noPage {
		return bytes.Clone(r.value), nil
	}
	var out []byte
	remaining := uint64(r.size)
	id := r.overflow
	seen := map[uint32]bool{}
	for id != noPage {
		if seen[id] {
			return nil, corrupt("overflow cycle")
		}
		seen[id] = true
		if visit != nil {
			if e := visit(id); e != nil {
				return nil, e
			}
		}
		p, e := t.readPage(id, overflowPage)
		if e != nil {
			return nil, e
		}
		length := uint64(binary.LittleEndian.Uint32(p[12:16]))
		next := binary.LittleEndian.Uint32(p[8:12])
		if length == 0 || length > treeEnd-treeHeader || length > remaining || (next != noPage && length != treeEnd-treeHeader) {
			return nil, corrupt("overflow length")
		}
		remaining -= length
		if visit == nil {
			out = append(out, p[treeHeader:treeHeader+int(length)]...)
		}
		id = next
		if remaining == 0 && id != noPage {
			return nil, corrupt("excess overflow pages")
		}
	}
	if remaining != 0 {
		return nil, corrupt("truncated overflow")
	}
	return out, nil
}

func (t *Tree) Get(key []byte) ([]byte, bool, error) {
	if e := checkKey(key); e != nil {
		return nil, false, e
	}
	_, n, e := t.find(key)
	if e != nil {
		return nil, false, e
	}
	i := recordIndex(n, key)
	if i == len(n.records) || !bytes.Equal(n.records[i].key, key) {
		return nil, false, nil
	}
	v, e := t.overflow(n.records[i], nil)
	return v, e == nil, e
}

func (t *Tree) newRecord(k, v []byte) (treeRecord, error) {
	r := treeRecord{key: bytes.Clone(k), size: uint32(len(v)), overflow: noPage}
	if len(v) <= inlineLimit {
		r.value = bytes.Clone(v)
		return r, nil
	}
	var ids []uint32
	for offset := 0; offset < len(v); offset += treeEnd - treeHeader {
		id, e := t.Tx.Alloc()
		if e != nil {
			return r, e
		}
		ids = append(ids, id)
	}
	r.overflow = ids[0]
	for i, id := range ids {
		p := pageBytes(overflowPage)
		next := noPage
		if i+1 < len(ids) {
			next = ids[i+1]
		}
		binary.LittleEndian.PutUint32(p[8:12], next)
		offset := i * (treeEnd - treeHeader)
		length := len(v) - offset
		if length > treeEnd-treeHeader {
			length = treeEnd - treeHeader
		}
		binary.LittleEndian.PutUint32(p[12:16], uint32(length))
		copy(p[treeHeader:], v[offset:offset+length])
		sealTreePage(p)
		if e := t.Tx.Write(id, p); e != nil {
			return r, e
		}
	}
	return r, nil
}

func (t *Tree) freeValue(r treeRecord) error {
	var ids []uint32
	_, e := t.overflow(r, func(id uint32) error { ids = append(ids, id); return nil })
	if e != nil {
		return e
	}
	for _, id := range ids {
		if e = t.Tx.Free(id); e != nil {
			return e
		}
	}
	return nil
}

func minimum(n *treeNode) []byte {
	if len(n.records) == 0 {
		return nil
	}
	return bytes.Clone(n.records[0].key)
}

func splitIndex(n *treeNode) int {
	best := 0
	gap := int(^uint(0) >> 1)
	for i := 1; i < len(n.records); i++ {
		a := nodeSize(&treeNode{kind: n.kind, records: n.records[:i]})
		b := nodeSize(&treeNode{kind: n.kind, records: n.records[i:]})
		if a <= treeEnd && b <= treeEnd {
			d := a - b
			if d < 0 {
				d = -d
			}
			if d < gap {
				gap = d
				best = i
			}
		}
	}
	return best
}

func (t *Tree) split(id uint32, n *treeNode) (*treeRecord, error) {
	i := splitIndex(n)
	if i == 0 {
		return nil, errors.New("B+ tree cannot split node")
	}
	rightID, e := t.Tx.Alloc()
	if e != nil {
		return nil, e
	}
	right := &treeNode{kind: n.kind, next: n.next, records: n.records[i:]}
	n.records = n.records[:i]
	if n.kind == leafPage {
		n.next = rightID
	}
	if e = t.writeNode(rightID, right); e != nil {
		return nil, e
	}
	if e = t.writeNode(id, n); e != nil {
		return nil, e
	}
	return &treeRecord{key: minimum(right), child: rightID}, nil
}

func insertRecord(rs []treeRecord, i int, r treeRecord) []treeRecord {
	rs = append(rs, treeRecord{})
	copy(rs[i+1:], rs[i:])
	rs[i] = r
	return rs
}

func (t *Tree) put(id uint32, k, v []byte, seen map[uint32]bool) ([]byte, *treeRecord, error) {
	if seen[id] {
		return nil, nil, corrupt("node cycle")
	}
	seen[id] = true
	n, e := t.readNode(id)
	if e != nil {
		return nil, nil, e
	}
	if n.kind == leafPage {
		i := recordIndex(n, k)
		r, e := t.newRecord(k, v)
		if e != nil {
			return nil, nil, e
		}
		if i < len(n.records) && bytes.Equal(n.records[i].key, k) {
			if e = t.freeValue(n.records[i]); e != nil {
				return nil, nil, e
			}
			n.records[i] = r
		} else {
			n.records = insertRecord(n.records, i, r)
		}
	} else {
		i := childIndex(n, k)
		min, right, e := t.put(n.records[i].child, k, v, seen)
		if e != nil {
			return nil, nil, e
		}
		n.records[i].key = min
		if right != nil {
			n.records = insertRecord(n.records, i+1, *right)
		}
	}
	if nodeSize(n) > treeEnd {
		r, e := t.split(id, n)
		return minimum(n), r, e
	}
	return minimum(n), nil, t.writeNode(id, n)
}

func (t *Tree) Put(key, value []byte) error {
	if e := checkKey(key); e != nil {
		return e
	}
	if uint64(len(value)) > uint64(^uint32(0)) {
		return ErrValueTooLarge
	}
	min, right, e := t.put(t.Root, key, value, map[uint32]bool{})
	if e != nil {
		return e
	}
	if right != nil {
		id, e := t.Tx.Alloc()
		if e != nil {
			return e
		}
		n := &treeNode{kind: branchPage, next: noPage, records: []treeRecord{{key: min, child: t.Root}, *right}}
		if e = t.writeNode(id, n); e != nil {
			return e
		}
		t.Root = id
	}
	return nil
}

func (t *Tree) rebalance(parent *treeNode, i int) error {
	if len(parent.records) < 2 {
		return nil
	}
	left := i
	if left == len(parent.records)-1 {
		left--
	}
	aID, bID := parent.records[left].child, parent.records[left+1].child
	if aID == bID {
		return corrupt("shared sibling")
	}
	a, e := t.readNode(aID)
	if e != nil {
		return e
	}
	b, e := t.readNode(bID)
	if e != nil {
		return e
	}
	if a.kind != b.kind {
		return corrupt("sibling type mismatch")
	}
	if a.kind == leafPage && a.next != bID {
		return corrupt("broken leaf link")
	}
	combined := &treeNode{kind: a.kind, next: b.next, records: append(append([]treeRecord{}, a.records...), b.records...)}
	if nodeSize(combined) <= treeEnd {
		if e = t.writeNode(aID, combined); e != nil {
			return e
		}
		if e = t.Tx.Free(bID); e != nil {
			return e
		}
		parent.records[left].key = minimum(combined)
		parent.records = append(parent.records[:left+1], parent.records[left+2:]...)
		return nil
	}
	cut := splitIndex(combined)
	if cut == 0 {
		return corrupt("cannot redistribute")
	}
	a.records = combined.records[:cut]
	b.records = combined.records[cut:]
	parent.records[left].key = minimum(a)
	parent.records[left+1].key = minimum(b)
	if e = t.writeNode(aID, a); e != nil {
		return e
	}
	return t.writeNode(bID, b)
}

func (t *Tree) delete(id uint32, k []byte, seen map[uint32]bool) (*treeNode, *treeRecord, error) {
	if seen[id] {
		return nil, nil, corrupt("node cycle")
	}
	seen[id] = true
	n, e := t.readNode(id)
	if e != nil {
		return nil, nil, e
	}
	if n.kind == leafPage {
		i := recordIndex(n, k)
		if i == len(n.records) || !bytes.Equal(n.records[i].key, k) {
			return n, nil, nil
		}
		if e = t.freeValue(n.records[i]); e != nil {
			return nil, nil, e
		}
		n.records = append(n.records[:i], n.records[i+1:]...)
	} else {
		i := childIndex(n, k)
		child, right, e := t.delete(n.records[i].child, k, seen)
		if e != nil {
			return nil, nil, e
		}
		n.records[i].key = minimum(child)
		if right != nil {
			n.records = insertRecord(n.records, i+1, *right)
		}
		if nodeSize(child) < treeEnd/2 || (child.kind == branchPage && len(child.records) < 2) {
			if e = t.rebalance(n, i); e != nil {
				return nil, nil, e
			}
		}
	}
	if nodeSize(n) > treeEnd {
		right, e := t.split(id, n)
		return n, right, e
	}
	return n, nil, t.writeNode(id, n)
}

func (t *Tree) Delete(key []byte) error {
	if e := checkKey(key); e != nil {
		return e
	}
	n, right, e := t.delete(t.Root, key, map[uint32]bool{})
	if e != nil {
		return e
	}
	if right != nil {
		id, e := t.Tx.Alloc()
		if e != nil {
			return e
		}
		root := &treeNode{kind: branchPage, next: noPage, records: []treeRecord{{key: minimum(n), child: t.Root}, *right}}
		if e = t.writeNode(id, root); e != nil {
			return e
		}
		t.Root = id
		return nil
	}
	for n.kind == branchPage && len(n.records) == 1 {
		old := t.Root
		next := n.records[0].child
		n, e = t.readNode(next)
		if e != nil {
			return e
		}
		if e = t.Tx.Free(old); e != nil {
			return e
		}
		t.Root = next
	}
	return nil
}

// Cursor returns borrowed key/value slices, valid until the next call to Next.
type Cursor struct {
	tree       *Tree
	node       *treeNode
	index      int
	end        []byte
	bounded    bool
	seen       map[uint32]bool
	key, value []byte
	err        error
	done       bool
}

func (t *Tree) Scan(start, end []byte) (*Cursor, error) {
	if e := checkKey(start); e != nil {
		return nil, e
	}
	if e := checkKey(end); e != nil {
		return nil, e
	}
	id, n, e := t.find(start)
	if e != nil {
		return nil, e
	}
	return &Cursor{tree: t, node: n, index: recordIndex(n, start), end: bytes.Clone(end), bounded: end != nil, seen: map[uint32]bool{id: true}, done: end != nil && bytes.Compare(start, end) >= 0}, nil
}

func (c *Cursor) Next() bool {
	c.key = nil
	c.value = nil
	if c.done || c.err != nil {
		return false
	}
	for c.index == len(c.node.records) {
		id := c.node.next
		if id == noPage {
			c.done = true
			return false
		}
		if c.seen[id] {
			c.err = corrupt("leaf cycle")
			return false
		}
		c.seen[id] = true
		n, e := c.tree.readNode(id)
		if e != nil {
			c.err = e
			return false
		}
		if n.kind != leafPage || len(n.records) == 0 || (len(c.node.records) > 0 && bytes.Compare(c.node.records[len(c.node.records)-1].key, n.records[0].key) >= 0) {
			c.err = corrupt("invalid leaf successor")
			return false
		}
		c.node = n
		c.index = 0
	}
	r := c.node.records[c.index]
	if c.bounded && bytes.Compare(r.key, c.end) >= 0 {
		c.done = true
		return false
	}
	v, e := c.tree.overflow(r, nil)
	if e != nil {
		c.err = e
		return false
	}
	c.index++
	c.key = r.key
	c.value = v
	return true
}
func (c *Cursor) Key() []byte   { return c.key }
func (c *Cursor) Value() []byte { return c.value }
func (c *Cursor) Err() error    { return c.err }

// Pages validates the tree and returns every owned node and overflow page once.
// Unrelated pages in the transaction are not considered tree corruption.
func (t *Tree) Pages() ([]uint32, error) {
	var ids, leaves, links []uint32
	owned := map[uint32]bool{}
	leafDepth := -1
	own := func(id uint32) error {
		if owned[id] {
			return corrupt("page %d has multiple owners", id)
		}
		owned[id] = true
		ids = append(ids, id)
		return nil
	}
	var walk func(uint32, int) ([]byte, []byte, error)
	walk = func(id uint32, depth int) ([]byte, []byte, error) {
		if e := own(id); e != nil {
			return nil, nil, e
		}
		n, e := t.readNode(id)
		if e != nil {
			return nil, nil, e
		}
		if n.kind == leafPage {
			if leafDepth < 0 {
				leafDepth = depth
			} else if leafDepth != depth {
				return nil, nil, corrupt("unequal leaf depths")
			}
			if id != t.Root && len(n.records) == 0 {
				return nil, nil, corrupt("empty non-root leaf")
			}
			leaves = append(leaves, id)
			links = append(links, n.next)
			for _, r := range n.records {
				if _, e = t.overflow(r, own); e != nil {
					return nil, nil, e
				}
			}
			if len(n.records) == 0 {
				return nil, nil, nil
			}
			return minimum(n), n.records[len(n.records)-1].key, nil
		}
		if len(n.records) < 2 {
			return nil, nil, corrupt("uncollapsed branch")
		}
		var first, last []byte
		for i, r := range n.records {
			min, max, e := walk(r.child, depth+1)
			if e != nil {
				return nil, nil, e
			}
			if !bytes.Equal(min, r.key) || (i > 0 && bytes.Compare(last, min) >= 0) {
				return nil, nil, corrupt("invalid child boundary")
			}
			if i == 0 {
				first = min
			}
			last = max
		}
		return first, last, nil
	}
	if _, _, e := walk(t.Root, 0); e != nil {
		return nil, e
	}
	for i, id := range links {
		want := noPage
		if i+1 < len(leaves) {
			want = leaves[i+1]
		}
		if id != want {
			return nil, corrupt("invalid leaf chain")
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}
func (t *Tree) Check() error { _, e := t.Pages(); return e }

// Destroy frees all owned pages after validation. Root becomes invalid on success.
func (t *Tree) Destroy() error {
	ids, e := t.Pages()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if e = t.Tx.Free(id); e != nil {
			return e
		}
	}
	t.Root = noPage
	return nil
}
