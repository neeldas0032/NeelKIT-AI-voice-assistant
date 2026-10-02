// Package id generates sortable, prefixed identifiers.
//
// Ids are ULID-shaped: 48 bits of millisecond timestamp followed by 80 bits of
// randomness, Crockford base32 encoded, with a type prefix. Two properties
// matter: they sort by creation time as plain strings (so "ORDER BY id" needs
// no extra index) and the prefix makes an id from the wrong table obvious in a
// log line instead of silently querying nothing.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Prefixes used across the system.
const (
	PrefixUser         = "usr"
	PrefixKey          = "key"
	PrefixConversation = "conv"
	PrefixSession      = "sess"
	PrefixMessage      = "msg"
	PrefixTranscript   = "tr"
	PrefixTurn         = "turn"
	PrefixCall         = "call"
	PrefixAudit        = "aud"
	PrefixRequest      = "req"
	PrefixTicket       = "rt"
)

// New returns a new prefixed identifier, e.g. "conv_01J9ZQ2K7X8N...".
func New(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	// Big-endian timestamp in the high 48 bits keeps lexical order == time order.
	binary.BigEndian.PutUint64(b[0:8], ms<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand failure is unrecoverable and must never be papered over
		// with a weaker source: a predictable id is a security defect.
		panic("id: crypto/rand unavailable: " + err.Error())
	}
	var sb strings.Builder
	sb.Grow(len(prefix) + 1 + 26)
	sb.WriteString(prefix)
	sb.WriteByte('_')
	sb.WriteString(encode(b))
	return sb.String()
}

func encode(b [16]byte) string {
	out := make([]byte, 26)
	// 128 bits -> 26 base32 chars (130 bits); the first char carries 2 spare bits.
	var acc uint64
	var bits uint
	pos := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[pos] = crockford[acc&0x1f]
			pos--
			acc >>= 5
			bits -= 5
		}
	}
	for pos >= 0 {
		out[pos] = crockford[acc&0x1f]
		pos--
		acc >>= 5
	}
	return string(out)
}
