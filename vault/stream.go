package vault

import (
	"crypto/cipher"
	"encoding/binary"
	"io"
)

// chunked streaming AEAD: one DEK per object (in the header), each framed chunk
// sealed under its own random nonce, with per-chunk associated data binding the
// header, the object AD, the chunk index, and a final-chunk flag. Truncation
// (missing final chunk) and reordering (wrong index) therefore fail closed.
//
// Frame layout after the header, repeated per chunk:
//
//	final:1 | frameLen:uint32 | nonce||ciphertext(frameLen bytes)

const streamMaxFrame = 1 << 30 // 1 GiB frame ceiling, guards against corrupt lengths

func chunkSuffix(index uint64, final bool) []byte {
	s := make([]byte, 9)
	binary.BigEndian.PutUint64(s[:8], index)
	if final {
		s[8] = 1
	}
	return s
}

// StreamWriter encrypts an arbitrary byte stream into framed chunks. Write
// buffers up to chunkSize; Close emits the final (possibly empty) chunk that
// marks the end of the stream. Close MUST be called for the stream to be
// readable.
type StreamWriter struct {
	w       io.Writer
	aead    cipher.AEAD
	base    []byte // headerBytes || ad.encode
	buf     []byte
	chunk   int
	index   uint64
	closed  bool
	written bool
}

const defaultChunkSize = 64 * 1024

// StreamWriter opens a streaming writer. chunkSize <= 0 uses a 64 KiB default.
func (uv *UserVault) StreamWriter(w io.Writer, ad AD, chunkSize int) (*StreamWriter, error) {
	uv.mu.RLock()
	activeID := uv.activeUKID
	activeUK := uv.uks[activeID]
	uv.mu.RUnlock()
	if activeUK == nil {
		return nil, ErrKeyUnavailable
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	dek, err := randBytes(keyLen)
	if err != nil {
		return nil, err
	}
	defer zero(dek)
	wrappedDEK, err := seal(activeUK, dek, dekWrapAAD)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	h := Header{Format: FormatVersion, ADSchema: ADSchemaV1, Shape: ShapeStream, UKID: activeID, WrappedDEK: wrappedDEK}
	hb := h.marshal()
	if _, err := w.Write(hb); err != nil {
		return nil, err
	}
	base := append(append([]byte(nil), hb...), ad.encode(ADSchemaV1)...)
	return &StreamWriter{w: w, aead: aead, base: base, chunk: chunkSize}, nil
}

func (sw *StreamWriter) emit(data []byte, final bool) error {
	nonce, err := randBytes(nonceLen)
	if err != nil {
		return err
	}
	aad := append(append([]byte(nil), sw.base...), chunkSuffix(sw.index, final)...)
	frame := make([]byte, 0, nonceLen+len(data)+sw.aead.Overhead())
	frame = append(frame, nonce...)
	frame = sw.aead.Seal(frame, nonce, data, aad)
	var hdr [5]byte
	if final {
		hdr[0] = 1
	}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(frame)))
	if _, err := sw.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := sw.w.Write(frame); err != nil {
		return err
	}
	sw.index++
	return nil
}

func (sw *StreamWriter) Write(p []byte) (int, error) {
	if sw.closed {
		return 0, io.ErrClosedPipe
	}
	sw.written = true
	n := len(p)
	sw.buf = append(sw.buf, p...)
	for len(sw.buf) >= sw.chunk {
		if err := sw.emit(sw.buf[:sw.chunk], false); err != nil {
			return 0, err
		}
		sw.buf = sw.buf[sw.chunk:]
	}
	return n, nil
}

// Close emits the final chunk (the buffered remainder, possibly empty) and marks
// the stream complete.
func (sw *StreamWriter) Close() error {
	if sw.closed {
		return nil
	}
	sw.closed = true
	return sw.emit(sw.buf, true)
}

// StreamReader decrypts a stream produced by StreamWriter. It reads until the
// final chunk; a stream that ends before its final chunk yields ErrTruncated.
type StreamReader struct {
	r     io.Reader
	aead  cipher.AEAD
	base  []byte
	index uint64
	buf   []byte
	done  bool
}

// StreamReader opens a streaming reader over r, selecting the user key named in
// the header and unwrapping the object DEK.
func (uv *UserVault) StreamReader(r io.Reader, ad AD) (*StreamReader, error) {
	h, hb, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	if h.Shape != ShapeStream {
		return nil, ErrUnsupported
	}
	uk, ok := uv.uk(h.UKID)
	if !ok {
		return nil, ErrKeyUnavailable
	}
	dek, err := open(uk, h.WrappedDEK, dekWrapAAD)
	if err != nil {
		return nil, ErrAuth
	}
	defer zero(dek)
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	base := append(append([]byte(nil), hb...), ad.encode(h.ADSchema)...)
	return &StreamReader{r: r, aead: aead, base: base}, nil
}

// next decrypts and buffers the next chunk, returning io.EOF exactly once the
// final chunk has been consumed.
func (sr *StreamReader) next() error {
	if sr.done {
		return io.EOF
	}
	var hdr [5]byte
	if _, err := io.ReadFull(sr.r, hdr[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return ErrTruncated
		}
		return err
	}
	final := hdr[0] == 1
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < uint32(nonceLen+sr.aead.Overhead()) || n > streamMaxFrame {
		return ErrTruncated
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(sr.r, frame); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return ErrTruncated
		}
		return err
	}
	nonce := frame[:nonceLen]
	ct := frame[nonceLen:]
	aad := append(append([]byte(nil), sr.base...), chunkSuffix(sr.index, final)...)
	pt, err := sr.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return ErrAuth
	}
	sr.index++
	sr.buf = append(sr.buf, pt...)
	if final {
		sr.done = true
	}
	return nil
}

func (sr *StreamReader) Read(p []byte) (int, error) {
	for len(sr.buf) == 0 {
		if err := sr.next(); err != nil {
			if err == io.EOF {
				return 0, io.EOF
			}
			return 0, err
		}
	}
	n := copy(p, sr.buf)
	sr.buf = sr.buf[n:]
	return n, nil
}

// readHeader reads a vault header from r field by field and returns it along
// with its exact marshaled bytes (for AD binding).
func readHeader(r io.Reader) (Header, []byte, error) {
	fixed := make([]byte, 8) // magic(4) + format + adSchema + shape + ukLen
	if _, err := io.ReadFull(r, fixed); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Header{}, nil, ErrTruncated
		}
		return Header{}, nil, err
	}
	if fixed[0] != Magic[0] || fixed[1] != Magic[1] || fixed[2] != Magic[2] || fixed[3] != Magic[3] {
		return Header{}, nil, ErrNotVault
	}
	ukLen := int(fixed[7])
	rest := make([]byte, ukLen+2)
	if _, err := io.ReadFull(r, rest); err != nil {
		return Header{}, nil, ErrTruncated
	}
	dekLen := int(binary.BigEndian.Uint16(rest[ukLen : ukLen+2]))
	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(r, dek); err != nil {
		return Header{}, nil, ErrTruncated
	}
	raw := make([]byte, 0, len(fixed)+len(rest)+len(dek))
	raw = append(raw, fixed...)
	raw = append(raw, rest...)
	raw = append(raw, dek...)
	h, _, err := unmarshalHeader(raw)
	if err != nil {
		return Header{}, nil, err
	}
	return h, raw, nil
}
