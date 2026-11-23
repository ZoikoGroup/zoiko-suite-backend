// Package ledger holds the G-07 evidence-ledger arithmetic: the chain link, the
// Merkle tree a sealed package commits to, and the hash that chains packages.
//
// The chain link itself is computed by the database (000016 gdl_chain_link)
// so no caller can supply its own hashes; Link here is the same formula, used
// to VERIFY a stored link from the canonical form the database returns
// (gdl_canonical). Writer and verifier therefore share one canonical form and
// one formula, but not one implementation.
package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
)

// Genesis is prev_hash of the first decision in a tenant: 32 zero bytes.
var Genesis = make([]byte, sha256.Size)

// Link is curr_hash = sha256(prev_hash || canonical).
func Link(prevHash []byte, canonical string) []byte {
	h := sha256.New()
	h.Write(prevHash)
	h.Write([]byte(canonical))
	return h.Sum(nil)
}

// LinkValid reports whether currHash is the link of prevHash and canonical.
func LinkValid(prevHash, currHash []byte, canonical string) bool {
	return bytes.Equal(Link(prevHash, canonical), currHash)
}

// leafHash and nodeHash are RFC 6962 §2.1: a 0x00 prefix for leaves and 0x01
// for interior nodes, so a leaf can never be passed off as a node.
func leafHash(leaf []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(leaf)
	return h.Sum(nil)
}

func nodeHash(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

// MerkleRoot is the RFC 6962 Merkle Tree Hash of leaves, in order. The root
// of no leaves is sha256("").
func MerkleRoot(leaves [][]byte) []byte {
	if len(leaves) == 0 {
		h := sha256.Sum256(nil)
		return h[:]
	}
	level := make([][]byte, len(leaves))
	for i, l := range leaves {
		level[i] = leafHash(l)
	}
	return mth(level)
}

// mth splits at the largest power of two smaller than n (RFC 6962 §2.1), so a
// tree over n leaves is a prefix of the tree over n+k leaves — what lets a
// later package be checked against an earlier one.
func mth(hashed [][]byte) []byte {
	n := len(hashed)
	if n == 1 {
		return hashed[0]
	}
	k := 1
	for k*2 < n {
		k *= 2
	}
	return nodeHash(mth(hashed[:k]), mth(hashed[k:]))
}

// PackageHash chains a package to the one before it:
// sha256(prev_package_hash || merkle_root || head_hash || big-endian to_seq).
func PackageHash(prevPackageHash, merkleRoot, headHash []byte, toSeq int64) []byte {
	h := sha256.New()
	h.Write(prevPackageHash)
	h.Write(merkleRoot)
	h.Write(headHash)
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], uint64(toSeq))
	h.Write(seq[:])
	return h.Sum(nil)
}
