// Package storage implements fixed-size pages with rollback-journal transactions.
package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

const PageSize = 4096

// PageDataSize excludes the final four bytes, which the pager owns as a CRC.
const PageDataSize = PageSize - 4
const CachePages = 64
const formatVersion = 1

var (
	ErrCorrupt   = errors.New("storage: corrupt database or journal")
	ErrClosed    = errors.New("storage: closed")
	ErrPoisoned  = errors.New("storage: failed commit; close and reopen required")
	ErrBusy      = errors.New("storage: transaction already active")
	ErrPage      = errors.New("storage: invalid or free page")
	ErrLocked    = errors.New("storage: database locked")
	dbMagic      = []byte("GBASEDB1")
	journalMagic = []byte("GBASEJR1")
	freeMagic    = []byte("GBASEFR1")
)

// Pager owns an exclusive advisory lock for its entire lifetime. Callers must
// serialize all operations. Fault may return errors or terminate the process to
// simulate crashes. Commit calls it at journal-created, journal-body-written,
// journal-body-synced, journal-ready-written, journal-ready-synced,
// journal-dir-synced, db-page-written (once per page), db-synced,
// journal-removed, and removal-dir-synced. Any returned error poisons the pager.
type Pager struct {
	file             *os.File
	path             string
	count, root      uint32
	free             map[uint32]bool
	cache            map[uint32][]byte
	order            []uint32
	active           *Tx
	closed, poisoned bool
	Fault            func(point string) error
}

// Tx buffers all changes until Commit. Page zero is reserved for pager metadata.
type Tx struct {
	p           *Pager
	count, root uint32
	free        map[uint32]bool
	dirty       map[uint32][]byte
	done        bool
}

// Snapshot is an opaque, reusable savepoint owned by one transaction.
type Snapshot struct {
	owner       *Tx
	count, root uint32
	free        map[uint32]bool
	dirty       map[uint32][]byte
}

func checksum(b []byte) uint32 { return crc32.ChecksumIEEE(b) }
func seal(b []byte)            { binary.LittleEndian.PutUint32(b[PageSize-4:], checksum(b[:PageSize-4])) }
func valid(b []byte) bool {
	return len(b) == PageSize && binary.LittleEndian.Uint32(b[PageSize-4:]) == checksum(b[:PageSize-4])
}
func put(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func get(b []byte, off int) uint32    { return binary.LittleEndian.Uint32(b[off:]) }
func metadata(count, root, head, nfree uint32) []byte {
	b := make([]byte, PageSize)
	copy(b, dbMagic)
	put(b, 8, formatVersion)
	put(b, 12, PageSize)
	put(b, 16, count)
	put(b, 20, root)
	put(b, 24, head)
	put(b, 28, nfree)
	seal(b)
	return b
}
func syncDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func writeAt(f *os.File, b []byte, off int64) error {
	n, err := f.WriteAt(b, off)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}
func readPage(f *os.File, id uint32) ([]byte, error) {
	b := make([]byte, PageSize)
	_, err := f.ReadAt(b, int64(id)*PageSize)
	return b, err
}

// Open locks the database, rolls back a ready journal, and validates metadata
// and the complete free list before exposing any pages.
func Open(path string) (p *Pager, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			f.Close()
		}
	}()
	if e := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return nil, fmt.Errorf("%w: %v", ErrLocked, e)
	}
	// Symlink aliases must use the same journal as the target database.
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	p = &Pager{file: f, path: path, free: make(map[uint32]bool), cache: make(map[uint32][]byte)}
	if err = p.recover(); err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() == 0 {
		if err = writeAt(f, metadata(1, 0, 0, 0), 0); err != nil {
			return nil, err
		}
		if err = f.Sync(); err != nil {
			return nil, err
		}
		if err = syncDir(path); err != nil {
			return nil, err
		}
	}
	if err = p.load(); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *Pager) load() error {
	b, err := readPage(p.file, 0)
	if err != nil {
		return fmt.Errorf("%w: header: %v", ErrCorrupt, err)
	}
	if !valid(b) || !bytes.Equal(b[:8], dbMagic) || get(b, 8) != formatVersion || get(b, 12) != PageSize {
		return ErrCorrupt
	}
	p.count, p.root = get(b, 16), get(b, 20)
	st, err := p.file.Stat()
	if err != nil {
		return err
	}
	if p.count == 0 || st.Size() != int64(p.count)*PageSize || p.root >= p.count {
		return ErrCorrupt
	}
	head, n := get(b, 24), get(b, 28)
	if n >= p.count {
		return ErrCorrupt
	}
	for head != 0 {
		if head >= p.count || p.free[head] || uint32(len(p.free)) >= n {
			return ErrCorrupt
		}
		p.free[head] = true
		b, err = readPage(p.file, head)
		if err != nil {
			return err
		}
		if !valid(b) || !bytes.Equal(b[:8], freeMagic) {
			return ErrCorrupt
		}
		head = get(b, 8)
	}
	if uint32(len(p.free)) != n || p.free[p.root] {
		return ErrCorrupt
	}
	return nil
}
func (p *Pager) check() error {
	if p.closed {
		return ErrClosed
	}
	if p.poisoned {
		return ErrPoisoned
	}
	return nil
}
func (p *Pager) Begin() (*Tx, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if p.active != nil {
		return nil, ErrBusy
	}
	t := &Tx{p: p, count: p.count, root: p.root, free: cloneFree(p.free), dirty: make(map[uint32][]byte)}
	p.active = t
	return t, nil
}
func (p *Pager) Close() error {
	if p.closed {
		return nil
	}
	if p.active != nil {
		p.active.done = true
		p.active = nil
	}
	p.closed = true
	// Closing the descriptor releases flock, including on a poisoned pager.
	return p.file.Close()
}
func cloneFree(m map[uint32]bool) map[uint32]bool {
	n := make(map[uint32]bool, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}
func cloneDirty(m map[uint32][]byte) map[uint32][]byte {
	n := make(map[uint32][]byte, len(m))
	for k, v := range m {
		n[k] = bytes.Clone(v)
	}
	return n
}
func (t *Tx) check() error {
	if err := t.p.check(); err != nil {
		return err
	}
	if t.done || t.p.active != t {
		return ErrClosed
	}
	return nil
}
func (t *Tx) page(id uint32) error {
	if id == 0 || id >= t.count || t.free[id] {
		return ErrPage
	}
	return nil
}
func (t *Tx) Read(id uint32) ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if err := t.page(id); err != nil {
		return nil, err
	}
	if b, ok := t.dirty[id]; ok {
		return bytes.Clone(b), nil
	}
	if b, ok := t.p.cache[id]; ok {
		return bytes.Clone(b), nil
	}
	b, err := readPage(t.p.file, id)
	if err != nil {
		return nil, err
	}
	if !valid(b) {
		return nil, fmt.Errorf("%w: page %d checksum", ErrCorrupt, id)
	}
	if len(t.p.order) == CachePages {
		delete(t.p.cache, t.p.order[0])
		t.p.order = t.p.order[1:]
	}
	t.p.cache[id] = b
	t.p.order = append(t.p.order, id)
	return bytes.Clone(b), nil
}

// Write copies a full page and replaces its final four bytes with the page CRC.
// It rejects free pages. Only the first PageDataSize bytes are caller-owned.
func (t *Tx) Write(id uint32, data []byte) error {
	if err := t.check(); err != nil {
		return err
	}
	if err := t.page(id); err != nil {
		return err
	}
	if len(data) != PageSize {
		return fmt.Errorf("storage: page must contain exactly %d bytes", PageSize)
	}
	b := bytes.Clone(data)
	seal(b)
	t.dirty[id] = b
	return nil
}

// Alloc returns a live page with a zero payload and a valid page CRC.
func (t *Tx) Alloc() (uint32, error) {
	if err := t.check(); err != nil {
		return 0, err
	}
	var id uint32
	for k := range t.free {
		if id == 0 || k < id {
			id = k
		}
	}
	if id != 0 {
		delete(t.free, id)
	} else {
		if t.count == math.MaxUint32 {
			return 0, errors.New("storage: page limit reached")
		}
		id = t.count
		t.count++
	}
	t.dirty[id] = make([]byte, PageSize)
	seal(t.dirty[id])
	return id, nil
}
func (t *Tx) Free(id uint32) error {
	if err := t.check(); err != nil {
		return err
	}
	if err := t.page(id); err != nil {
		return err
	}
	t.free[id] = true
	delete(t.dirty, id)
	return nil
}
func (t *Tx) Root() uint32      { return t.root }
func (t *Tx) SetRoot(id uint32) { t.root = id }

// PageCount includes the reserved metadata page and free pages.
func (t *Tx) PageCount() uint32 { return t.count }

// FreePages returns a sorted copy of the transaction's free page IDs.
func (t *Tx) FreePages() []uint32 {
	pages := make([]uint32, 0, len(t.free))
	for id := range t.free {
		pages = append(pages, id)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	return pages
}

// CheckOwnership checks a complete traversal of all live pages. The caller
// supplies each reachable page ID once, excluding the reserved metadata page.
// Duplicate references, unreachable live pages, and references to free pages
// are corruption. This does not inspect the caller's page format.
func (t *Tx) CheckOwnership(owned []uint32) error {
	if err := t.check(); err != nil {
		return err
	}
	seen := make(map[uint32]bool, len(owned))
	for _, id := range owned {
		if t.page(id) != nil || seen[id] {
			return fmt.Errorf("%w: invalid or duplicate ownership of page %d", ErrCorrupt, id)
		}
		seen[id] = true
	}
	if uint64(len(seen))+uint64(len(t.free))+1 != uint64(t.count) {
		return fmt.Errorf("%w: unreachable live pages", ErrCorrupt)
	}
	if t.root != 0 && !seen[t.root] {
		return fmt.Errorf("%w: unowned root", ErrCorrupt)
	}
	return nil
}
func (t *Tx) Savepoint() Snapshot {
	return Snapshot{t, t.count, t.root, cloneFree(t.free), cloneDirty(t.dirty)}
}
func (t *Tx) Restore(s Snapshot) error {
	if err := t.check(); err != nil {
		return err
	}
	if s.owner != t {
		return errors.New("storage: foreign savepoint")
	}
	t.count, t.root = s.count, s.root
	t.free = cloneFree(s.free)
	t.dirty = cloneDirty(s.dirty)
	return nil
}
func (t *Tx) Rollback() error {
	if err := t.check(); err != nil {
		return err
	}
	t.done = true
	t.p.active = nil
	return nil
}
func (p *Pager) fault(point string) error {
	if p.Fault != nil {
		return p.Fault(point)
	}
	return nil
}
func ids(m map[uint32][]byte) []uint32 {
	r := make([]uint32, 0, len(m))
	for id := range m {
		r = append(r, id)
	}
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	return r
}

func (t *Tx) Commit() (err error) {
	if err = t.check(); err != nil {
		return err
	}
	if t.root != 0 && (t.root >= t.count || t.free[t.root]) {
		return ErrPage
	}
	p := t.p
	// From this point any failure requires recovery; buffered state is never reused.
	defer func() {
		t.done = true
		p.active = nil
		if err != nil {
			p.poisoned = true
		}
	}()
	dirty := cloneDirty(t.dirty)
	frees := make([]uint32, 0, len(t.free))
	for id := range t.free {
		frees = append(frees, id)
	}
	sort.Slice(frees, func(i, j int) bool { return frees[i] < frees[j] })
	var head uint32
	for i := len(frees) - 1; i >= 0; i-- {
		b := make([]byte, PageSize)
		copy(b, freeMagic)
		put(b, 8, head)
		seal(b)
		head = frees[i]
		dirty[head] = b
	}
	dirty[0] = metadata(t.count, t.root, head, uint32(len(frees)))
	pageIDs := ids(dirty)
	originals := make(map[uint32][]byte)
	for _, id := range pageIDs {
		if id < p.count {
			b, e := readPage(p.file, id)
			if e != nil {
				return e
			}
			if !valid(b) {
				return fmt.Errorf("%w: original page %d checksum", ErrCorrupt, id)
			}
			originals[id] = b
		}
	}
	if err = p.journal(originals); err != nil {
		return err
	}
	for _, id := range pageIDs {
		if err = writeAt(p.file, dirty[id], int64(id)*PageSize); err != nil {
			return err
		}
		if err = p.fault("db-page-written"); err != nil {
			return err
		}
	}
	if err = p.file.Sync(); err != nil {
		return err
	}
	if err = p.fault("db-synced"); err != nil {
		return err
	}
	if err = os.Remove(p.path + "-journal"); err != nil {
		return err
	}
	if err = p.fault("journal-removed"); err != nil {
		return err
	}
	if err = syncDir(p.path); err != nil {
		return err
	}
	if err = p.fault("removal-dir-synced"); err != nil {
		return err
	}
	p.count, p.root = t.count, t.root
	p.free = cloneFree(t.free)
	p.cache = make(map[uint32][]byte)
	p.order = nil
	return nil
}

// The journal contains a checksummed header and fixed records (page ID, page
// bytes, record CRC). The header is marked ready only after the body is synced.
func journalHeader(length uint64, n uint32, ready uint32, bodyCRC uint32) []byte {
	b := make([]byte, PageSize)
	copy(b, journalMagic)
	put(b, 8, formatVersion)
	put(b, 12, PageSize)
	binary.LittleEndian.PutUint64(b[16:], length)
	put(b, 24, n)
	put(b, 28, ready)
	put(b, 32, bodyCRC)
	// A second marker makes a damaged ready bit fail closed rather than
	// incorrectly classifying a ready journal as an incomplete journal.
	if ready == 1 {
		put(b, 36, 0x52454144)
	}
	seal(b)
	return b
}
func (p *Pager) journal(originals map[uint32][]byte) (err error) {
	f, err := os.OpenFile(p.path+"-journal", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	// An incomplete initial header is safe to discard only when its unready
	// markers can be identified; ambiguous damage is rejected conservatively.
	h := journalHeader(uint64(p.count)*PageSize, uint32(len(originals)), 0, 0)
	if err = writeAt(f, h, 0); err != nil {
		return err
	}
	if err = p.fault("journal-created"); err != nil {
		return err
	}
	hash := crc32.NewIEEE()
	off := int64(PageSize)
	for _, id := range ids(originals) {
		b := make([]byte, PageSize+8)
		put(b, 0, id)
		copy(b[4:], originals[id])
		put(b, PageSize+4, checksum(b[:PageSize+4]))
		if err = writeAt(f, b, off); err != nil {
			return err
		}
		hash.Write(b)
		off += int64(len(b))
	}
	if err = p.fault("journal-body-written"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = p.fault("journal-body-synced"); err != nil {
		return err
	}
	h = journalHeader(uint64(p.count)*PageSize, uint32(len(originals)), 1, hash.Sum32())
	if err = writeAt(f, h, 0); err != nil {
		return err
	}
	if err = p.fault("journal-ready-written"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = p.fault("journal-ready-synced"); err != nil {
		return err
	}
	if err = syncDir(p.path); err != nil {
		return err
	}
	return p.fault("journal-dir-synced")
}
func (p *Pager) recover() error {
	f, err := os.Open(p.path + "-journal")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	h := make([]byte, PageSize)
	read, readErr := f.ReadAt(h, 0)
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if read == 0 {
		// Creation may crash before any header bytes are written.
		if err = os.Remove(p.path + "-journal"); err != nil {
			return err
		}
		return syncDir(p.path)
	}
	if read < 40 || !bytes.Equal(h[:8], journalMagic) || get(h, 8) != formatVersion || get(h, 12) != PageSize {
		return ErrCorrupt
	}
	if get(h, 28) == 0 && get(h, 36) == 0 {
		// The body and even the initial header may be incomplete. No database
		// writes are allowed until both ready markers are written and synced.
		if err = os.Remove(p.path + "-journal"); err != nil {
			return err
		}
		return syncDir(p.path)
	}
	if read != PageSize || !valid(h) || get(h, 28) != 1 || get(h, 36) != 0x52454144 {
		return ErrCorrupt
	}
	length := binary.LittleEndian.Uint64(h[16:])
	n, ready := get(h, 24), get(h, 28)
	if length < PageSize || length%PageSize != 0 || length/PageSize > math.MaxUint32 || n == 0 || uint64(n) > length/PageSize || ready > 1 {
		return ErrCorrupt
	}
	if ready == 1 {
		st, e := f.Stat()
		if e != nil {
			return e
		}
		if st.Size() != PageSize+int64(n)*(PageSize+8) {
			return ErrCorrupt
		}
		// Validate every record before making recovery writes. A corrupt journal
		// remains intact, so opening it cannot partially overwrite the database.
		hash := crc32.NewIEEE()
		seen := make(map[uint32]bool)
		var header []byte
		for i := uint32(0); i < n; i++ {
			b := make([]byte, PageSize+8)
			if _, e = f.ReadAt(b, PageSize+int64(i)*(PageSize+8)); e != nil {
				return ErrCorrupt
			}
			id := get(b, 0)
			if uint64(id) >= length/PageSize || seen[id] || get(b, PageSize+4) != checksum(b[:PageSize+4]) || !valid(b[4:PageSize+4]) {
				return ErrCorrupt
			}
			seen[id] = true
			hash.Write(b)
			if id == 0 {
				header = bytes.Clone(b[4 : PageSize+4])
			}
		}
		if hash.Sum32() != get(h, 32) || !valid(header) || !bytes.Equal(header[:8], dbMagic) || get(header, 8) != formatVersion || get(header, 12) != PageSize || uint64(get(header, 16))*PageSize != length {
			return ErrCorrupt
		}
		for i := uint32(0); i < n; i++ {
			b := make([]byte, PageSize+8)
			if _, e = f.ReadAt(b, PageSize+int64(i)*(PageSize+8)); e != nil {
				return e
			}
			if e = writeAt(p.file, b[4:PageSize+4], int64(get(b, 0))*PageSize); e != nil {
				return e
			}
		}
		if err = p.file.Truncate(int64(length)); err != nil {
			return err
		}
		if err = p.file.Sync(); err != nil {
			return err
		}
	}
	if err = os.Remove(p.path + "-journal"); err != nil {
		return err
	}
	return syncDir(p.path)
}
