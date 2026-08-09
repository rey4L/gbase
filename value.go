package gbase

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Value is nil, int64, float64, string, or []byte.
type Value = any

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string                    { return e.Code + ": " + e.Message }
func fail(code, format string, args ...any) error { return &Error{code, fmt.Sprintf(format, args...)} }

var ErrClosed = errors.New("gbase: closed")
var ErrBusy = errors.New("gbase: active cursor")

func normalize(v any) (Value, error) {
	switch x := v.(type) {
	case nil, string:
		return x, nil
	case []byte:
		return bytes.Clone(x), nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		if uint64(x) <= math.MaxInt64 {
			return int64(x), nil
		}
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), nil
		}
	case float32:
		return normalize(float64(x))
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return x, nil
		}
	}
	return nil, fail("type", "unsupported parameter %T or out-of-range value", v)
}
func encodeRecord(values []Value) ([]byte, error) {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(values)))
	for _, v := range values {
		switch x := v.(type) {
		case nil:
			b.WriteByte(0)
		case int64:
			b.WriteByte(1)
			binary.Write(&b, binary.BigEndian, x)
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, fail("type", "nonfinite real")
			}
			b.WriteByte(2)
			binary.Write(&b, binary.BigEndian, x)
		case string:
			b.WriteByte(3)
			binary.Write(&b, binary.BigEndian, uint32(len(x)))
			b.WriteString(x)
		case []byte:
			b.WriteByte(4)
			binary.Write(&b, binary.BigEndian, uint32(len(x)))
			b.Write(x)
		default:
			return nil, fail("type", "unsupported record value %T", v)
		}
	}
	return b.Bytes(), nil
}
func decodeRecord(data []byte) ([]Value, error) {
	r := bytes.NewReader(data)
	var n uint32
	if binary.Read(r, binary.BigEndian, &n) != nil || n > uint32(len(data)) {
		return nil, fail("corrupt", "invalid record count")
	}
	out := make([]Value, n)
	for i := range out {
		tag, e := r.ReadByte()
		if e != nil {
			return nil, fail("corrupt", "truncated record")
		}
		switch tag {
		case 0:
		case 1:
			var x int64
			if binary.Read(r, binary.BigEndian, &x) != nil {
				return nil, fail("corrupt", "truncated integer")
			}
			out[i] = x
		case 2:
			var x float64
			if binary.Read(r, binary.BigEndian, &x) != nil || math.IsInf(x, 0) || math.IsNaN(x) {
				return nil, fail("corrupt", "invalid real")
			}
			out[i] = x
		case 3, 4:
			var size uint32
			if binary.Read(r, binary.BigEndian, &size) != nil || uint64(size) > uint64(r.Len()) {
				return nil, fail("corrupt", "invalid value length")
			}
			v := make([]byte, size)
			r.Read(v)
			if tag == 3 {
				out[i] = string(v)
			} else {
				out[i] = v
			}
		default:
			return nil, fail("corrupt", "unknown value tag")
		}
	}
	if r.Len() != 0 {
		return nil, fail("corrupt", "record trailing bytes")
	}
	return out, nil
}
func rowKey(id int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(id)^(1<<63))
	return b
}
func keyRow(b []byte) (int64, error) {
	if len(b) != 8 {
		return 0, fail("corrupt", "invalid row key")
	}
	return int64(binary.BigEndian.Uint64(b) ^ (1 << 63)), nil
}
func indexPrefix(v Value) ([]byte, error) {
	b := []byte{}
	var fixed [8]byte
	switch x := v.(type) {
	case nil:
		b = append(b, 0)
	case int64:
		b = append(b, 1)
		binary.BigEndian.PutUint64(fixed[:], uint64(x)^(1<<63))
		b = append(b, fixed[:]...)
	case float64:
		b = append(b, 2)
		if x == 0 {
			x = 0
		}
		bits := math.Float64bits(x)
		if bits>>63 != 0 {
			bits = ^bits
		} else {
			bits ^= 1 << 63
		}
		binary.BigEndian.PutUint64(fixed[:], bits)
		b = append(b, fixed[:]...)
	case string:
		b = append(b, 3)
		b = escaped(b, []byte(x))
	case []byte:
		b = append(b, 4)
		b = escaped(b, x)
	default:
		return nil, fail("type", "invalid index value")
	}
	if len(b)+8 > 1024 {
		return nil, fail("limit", "indexed value exceeds 1024-byte key limit")
	}
	return b, nil
}
func escaped(dst, src []byte) []byte {
	for _, x := range src {
		if x == 0 {
			dst = append(dst, 0, 255)
		} else {
			dst = append(dst, x)
		}
	}
	return append(dst, 0, 0)
}
func prefixEnd(b []byte) []byte {
	out := bytes.Clone(b)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 255 {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}
