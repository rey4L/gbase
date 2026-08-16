package storage

import "testing"

func FuzzTreePage(f *testing.F) {
	seed := pageBytes(leafPage)
	sealTreePage(seed)
	seal(seed)
	f.Add(seed)
	f.Add([]byte("GBT1"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > PageSize {
			return
		}
		page := make([]byte, PageSize)
		copy(page, data)
		sealTreePage(page)
		seal(page)
		p := &Pager{}
		tx := &Tx{p: p, count: 2, free: map[uint32]bool{}, dirty: map[uint32][]byte{1: page}}
		tree := Tree{Tx: tx, Root: 1}
		node, err := tree.readNode(1)
		if err != nil {
			return
		}
		if node.kind != leafPage && node.kind != branchPage {
			t.Fatal("invalid node kind accepted")
		}
		if nodeSize(node) > treeEnd {
			t.Fatal("oversized node accepted")
		}
	})
}
