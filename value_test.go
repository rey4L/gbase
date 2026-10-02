package gbase

import (
	"bytes"
	"math"
	"reflect"
	"testing"
)

func TestRecordCodec(t *testing.T) {
	want := []Value{nil, int64(math.MinInt64), int64(math.MaxInt64), float64(-1.25), "a\x00é", []byte{0, 255}}
	b, e := encodeRecord(want)
	if e != nil {
		t.Fatal(e)
	}
	got, e := decodeRecord(b)
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v %v", got, e)
	}
	for i := 0; i < len(b); i++ {
		if _, e := decodeRecord(b[:i]); e == nil {
			t.Fatalf("accepted truncation %d", i)
		}
	}
}

func TestOrderedKeys(t *testing.T) {
	sets := [][]Value{{int64(math.MinInt64), int64(-1), int64(0), int64(1), int64(math.MaxInt64)}, {float64(-1e100), float64(-1), float64(0), float64(1), float64(1e100)}, {"", "\x00", "a", "a\x00", "aa", "z"}, {[]byte{}, []byte{0}, []byte{1}, []byte{255}}}
	for _, set := range sets {
		var last []byte
		for _, v := range set {
			key, e := indexPrefix(v)
			if e != nil {
				t.Fatal(e)
			}
			if last != nil && bytes.Compare(last, key) >= 0 {
				t.Fatalf("keys unordered: %v", set)
			}
			last = key
		}
	}
}

func FuzzDecodeRecord(f *testing.F) {
	b, _ := encodeRecord([]Value{nil, int64(1), "test", []byte{0, 1}})
	f.Add(b)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		v, e := decodeRecord(b)
		if e != nil {
			return
		}
		out, e := encodeRecord(v)
		if e != nil || !bytes.Equal(b, out) {
			t.Fatalf("codec mismatch %x", b)
		}
	})
}
