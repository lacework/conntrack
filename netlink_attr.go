package conntrack

// Netlink attr parsing.

import (
	"encoding/binary"
	"errors"
)

const attrHdrLength = 4

type Attr struct {
	Msg            []byte
	Typ            int
	IsNested       bool
	IsNetByteorder bool
}

func parseAttrs(b []byte, attrs []Attr) (int, error) {

	idx := 0
	for len(b) >= attrHdrLength {
		var use bool
		use, b = parseAttr(b, &attrs[idx])
		if use {
			idx++
		}
	}
	if len(b) != 0 {
		return 0, errors.New("leftover attr bytes")
	}
	return idx, nil
}

func parseAttr(b []byte, attr *Attr) (bool, []byte) {

	l := binary.LittleEndian.Uint16(b[0:2])
	// length is header + payload
	l -= uint16(attrHdrLength)

	typ := binary.LittleEndian.Uint16(b[2:4])

	attr.Msg = b[attrHdrLength : attrHdrLength+int(l)]
	attr.Typ = int(typ & NLA_TYPE_MASK)
	attr.IsNested = typ&NLA_F_NESTED > 0
	attr.IsNetByteorder = typ&NLA_F_NET_BYTEORDER > 0

	return true, b[rtaAlignOf(attrHdrLength+int(l)):]
}
