package digestexecution_test

// Independent unkeyed BLAKE3 reference model (32-byte output), following
// https://github.com/BLAKE3-team/BLAKE3/blob/master/ reference_impl/reference_impl.rs.
// Kept area-local because the scaffold has no BLAKE3 dependency.
import (
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"testing"
)

var iv = [8]uint32{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19}

type b3output struct {
	cv          [8]uint32
	block       [16]uint32
	counter     uint64
	size, flags uint32
}

func (o b3output) compress(root bool) [16]uint32 {
	counter, flags := o.counter, o.flags
	if root {
		counter = 0
		flags |= 8
	}
	s := [16]uint32{o.cv[0], o.cv[1], o.cv[2], o.cv[3], o.cv[4], o.cv[5], o.cv[6], o.cv[7], iv[0], iv[1], iv[2], iv[3], uint32(counter), uint32(counter >> 32), o.size, flags}
	m := o.block
	g := func(a, b, c, d int, x, y uint32) {
		s[a] += s[b] + x
		s[d] = bits.RotateLeft32(s[d]^s[a], -16)
		s[c] += s[d]
		s[b] = bits.RotateLeft32(s[b]^s[c], -12)
		s[a] += s[b] + y
		s[d] = bits.RotateLeft32(s[d]^s[a], -8)
		s[c] += s[d]
		s[b] = bits.RotateLeft32(s[b]^s[c], -7)
	}
	for r := 0; r < 7; r++ {
		g(0, 4, 8, 12, m[0], m[1])
		g(1, 5, 9, 13, m[2], m[3])
		g(2, 6, 10, 14, m[4], m[5])
		g(3, 7, 11, 15, m[6], m[7])
		g(0, 5, 10, 15, m[8], m[9])
		g(1, 6, 11, 12, m[10], m[11])
		g(2, 7, 8, 13, m[12], m[13])
		g(3, 4, 9, 14, m[14], m[15])
		p := [16]int{2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8}
		var next [16]uint32
		for i := range next {
			next[i] = m[p[i]]
		}
		m = next
	}
	for i := 0; i < 8; i++ {
		s[i] ^= s[i+8]
		s[i+8] ^= o.cv[i]
	}
	return s
}
func (o b3output) chain() [8]uint32 {
	s := o.compress(false)
	var c [8]uint32
	copy(c[:], s[:8])
	return c
}
func b3tree(b []byte, chunk uint64) b3output {
	if len(b) > 1024 {
		n := 1
		for n*2*1024 < len(b) {
			n *= 2
		}
		left := b3tree(b[:n*1024], chunk).chain()
		right := b3tree(b[n*1024:], chunk+uint64(n)).chain()
		o := b3output{cv: iv, size: 64, flags: 4}
		copy(o.block[:8], left[:])
		copy(o.block[8:], right[:])
		return o
	}
	cv := iv
	first := true
	for {
		n := len(b)
		if n > 64 {
			n = 64
		}
		var block [64]byte
		copy(block[:], b[:n])
		o := b3output{cv: cv, counter: chunk, size: uint32(n)}
		for i := range o.block {
			o.block[i] = binary.LittleEndian.Uint32(block[i*4:])
		}
		if first {
			o.flags |= 1
		}
		if len(b) <= 64 {
			o.flags |= 2
			return o
		}
		cv = o.chain()
		b = b[64:]
		first = false
	}
}
func blake3(b []byte) []byte {
	s := b3tree(b, 0).compress(true)
	out := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint32(out[i*4:], s[i])
	}
	return out
}
func TestBlake3ReferenceVectors(t *testing.T) {
	for input, want := range map[string]string{"": "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262", "abc": "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"} {
		if got := hex.EncodeToString(blake3([]byte(input))); got != want {
			t.Fatalf("BLAKE3(%q)=%s want %s", input, got, want)
		}
	}
}

func TestBlake3ChunkBoundaryVectors(t *testing.T) {
	for n, want := range map[int]string{
		63:   "e9bc37a594daad83be9470df7f7b3798297c3d834ce80ba85d6e207627b7db7b",
		64:   "4eed7141ea4a5cd4b788606bd23f46e212af9cacebacdc7d1f4c6dc7f2511b98",
		65:   "de1e5fa0be70df6d2be8fffd0e99ceaa8eb6e8c93a63f2d8d1c30ecb6b263dee",
		1023: "10108970eeda3eb932baac1428c7a2163b0e924c9a9e25b35bba72b28f70bd11",
		1024: "42214739f095a406f3fc83deb889744ac00df831c10daa55189b5d121c855af7",
		1025: "d00278ae47eb27b34faecf67b4fe263f82d5412916c1ffd97c8cb7fb814b8444",
		2048: "e776b6028c7cd22a4d0ba182a8bf62205d2ef576467e838ed6f2529b85fba24a",
		2049: "5f4d72f40d7a5f82b15ca2b2e44b1de3c2ef86c426c95c1af0b6879522563030",
		4096: "015094013f57a5277b59d8475c0501042c0b642e531b0a1c8f58d2163229e969",
		4097: "9b4052b38f1c5fc8b1f9ff7ac7b27cd242487b3d890d15c96a1c25b8aa0fb995",
	} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i % 251)
		}
		if got := hex.EncodeToString(blake3(b)); got != want {
			t.Fatalf("BLAKE3 vector length=%d got=%s want=%s", n, got, want)
		}
	}
}
