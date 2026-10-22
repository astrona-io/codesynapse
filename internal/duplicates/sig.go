package duplicates

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"strings"
)

// Near-miss detection compares MinHash signatures of token shingles, so
// copies that were edited a little still look alike. A signature is
// SigSlots values of 16 bits each, written as hex.
const (
	SigSlots = 32
	shingle  = 3
	// NearThreshold is the estimated share of shared shingles from which two
	// different functions count as edited copies of each other.
	NearThreshold = 0.7
)

// Hash is the exact structural fingerprint of a normalised token stream.
func Hash(tokens []string) string {
	sum := sha256.Sum256([]byte(strings.Join(tokens, ";") + ";"))
	return hex.EncodeToString(sum[:8])
}

// Sig is the MinHash signature of the 3-token shingles of tokens.
func Sig(tokens []string) string {
	var mins [SigSlots]uint64
	for i := range mins {
		mins[i] = ^uint64(0)
	}
	n := max(1, len(tokens)-shingle+1)
	for i := range n {
		h := fnv.New64a()
		for _, t := range tokens[i:min(i+shingle, len(tokens))] {
			h.Write([]byte(t)) //nolint:errcheck // hash.Hash.Write never fails
			h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never fails
		}
		base := h.Sum64()
		for s := range mins {
			if v := mix(base + uint64(s+1)*0x9e3779b97f4a7c15); v < mins[s] {
				mins[s] = v
			}
		}
	}
	out := make([]byte, 0, SigSlots*2)
	for _, m := range mins {
		out = append(out, byte(m>>56), byte(m>>48&0xff))
	}
	return hex.EncodeToString(out)
}

// mix is the splitmix64 finaliser: independent-looking hashes of one input.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// decodeSig returns the slots of a signature, or nil when it is malformed.
func decodeSig(s string) []uint16 {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != SigSlots*2 {
		return nil
	}
	out := make([]uint16, SigSlots)
	for i := range out {
		out[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return out
}

// Similarity estimates how much of two functions' shingles are shared (0 to
// 1) from their signatures; 0 for a malformed signature.
func Similarity(a, b string) float64 {
	x, y := decodeSig(a), decodeSig(b)
	if x == nil || y == nil {
		return 0
	}
	same := 0
	for i := range x {
		if x[i] == y[i] {
			same++
		}
	}
	return float64(same) / SigSlots
}
