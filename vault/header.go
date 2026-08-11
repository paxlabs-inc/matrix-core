package vault

import "encoding/binary"

// Magic identifies a Centra AI vault ciphertext object. It is the first four bytes
// of every sealed record, file, and stream, letting sniffing readers tell a
// vault object from a legacy plaintext JSON/JSONL line.
var Magic = [4]byte{'M', 'X', 'V', '1'}

// FormatVersion is the current self-describing container version.
const FormatVersion uint8 = 1

// ADSchemaV1 is the current associated-data canonical encoding version.
const ADSchemaV1 uint8 = 1

// Shape distinguishes the three ciphertext container shapes.
type Shape uint8

const (
	ShapeRecord Shape = 1 // one independent AEAD line/value (JSONL, Pebble value)
	ShapeFile   Shape = 2 // whole-file AEAD (tmp+rename atomic JSON)
	ShapeStream Shape = 3 // chunked streaming AEAD (media, snapshots)
)

// Header is the self-describing prefix of every ciphertext object. It carries
// no raw key material — only a key id (which user-key version to use) and the
// DEK wrapped under that user key. The marshaled header is bound as associated
// data on every AEAD operation, so tampering with any header field (downgrading
// the shape, swapping the wrapped DEK, relabeling the key id) fails the open.
type Header struct {
	Format     uint8
	ADSchema   uint8
	Shape      Shape
	UKID       string // user-key version id; selects the UK on read
	WrappedDEK []byte // DEK wrapped under the user key (never raw)
}

func (h Header) marshal() []byte {
	buf := make([]byte, 0, 4+3+1+len(h.UKID)+2+len(h.WrappedDEK))
	buf = append(buf, Magic[:]...)
	buf = append(buf, h.Format, h.ADSchema, byte(h.Shape))
	buf = append(buf, byte(len(h.UKID)))
	buf = append(buf, h.UKID...)
	var l2 [2]byte
	binary.BigEndian.PutUint16(l2[:], uint16(len(h.WrappedDEK)))
	buf = append(buf, l2[:]...)
	buf = append(buf, h.WrappedDEK...)
	return buf
}

// unmarshalHeader parses a header from the front of b and returns it, the
// number of bytes consumed, and any error. A magic mismatch yields ErrNotVault
// so callers can fall through to a legacy plaintext parse.
func unmarshalHeader(b []byte) (Header, int, error) {
	var h Header
	if len(b) < 4 || b[0] != Magic[0] || b[1] != Magic[1] || b[2] != Magic[2] || b[3] != Magic[3] {
		return h, 0, ErrNotVault
	}
	if len(b) < 4+3+1 {
		return h, 0, ErrTruncated
	}
	off := 4
	h.Format = b[off]
	h.ADSchema = b[off+1]
	h.Shape = Shape(b[off+2])
	off += 3
	if h.Format != FormatVersion {
		return h, 0, ErrUnsupported
	}
	ukLen := int(b[off])
	off++
	if len(b) < off+ukLen+2 {
		return h, 0, ErrTruncated
	}
	h.UKID = string(b[off : off+ukLen])
	off += ukLen
	dekLen := int(binary.BigEndian.Uint16(b[off : off+2]))
	off += 2
	if len(b) < off+dekLen {
		return h, 0, ErrTruncated
	}
	h.WrappedDEK = append([]byte(nil), b[off:off+dekLen]...)
	off += dekLen
	return h, off, nil
}

// IsVault reports whether b begins with the vault magic. Sniffing readers use
// it to route bytes to the vault decoder or the legacy plaintext parser.
func IsVault(b []byte) bool {
	return len(b) >= 4 && b[0] == Magic[0] && b[1] == Magic[1] && b[2] == Magic[2] && b[3] == Magic[3]
}
