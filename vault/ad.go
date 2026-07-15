package vault

import "encoding/binary"

// AD is the associated data bound to a ciphertext object. It is authenticated
// but not encrypted, and — critically — it is NOT stored in the object: a reader
// reconstructs it from its own knowledge of where the record lives (which user,
// store, conversation/run, sequence, schema). A record moved between users,
// stores, conversations, or positions therefore reconstructs to different AD and
// fails authentication.
type AD struct {
	User   string // user DID
	Store  string // store type, e.g. "neo.conversation"
	Stream string // conversation id / run id / file role
	Seq    uint64 // record sequence (0 for whole-file/stream objects)
	Schema string // logical schema/type version of the payload
}

// encode produces a canonical, unambiguous byte encoding of the AD under the
// given schema version. Every field is length-prefixed so no two distinct ADs
// collide. The schema byte provides domain separation across AD layouts.
func (a AD) encode(schema uint8) []byte {
	strs := []string{a.User, a.Store, a.Stream, a.Schema}
	n := 1 + 8
	for _, s := range strs {
		n += 4 + len(s)
	}
	buf := make([]byte, 0, n)
	buf = append(buf, schema)
	var l [4]byte
	for _, s := range strs {
		binary.BigEndian.PutUint32(l[:], uint32(len(s)))
		buf = append(buf, l[:]...)
		buf = append(buf, s...)
	}
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], a.Seq)
	buf = append(buf, seq[:]...)
	return buf
}
