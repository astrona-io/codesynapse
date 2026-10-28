package model

import (
	"crypto/sha256"
	"encoding/hex"
)

// FindingFingerprint is the stable key of a security finding: its rule, the
// short name of the symbol that owns it (the file when nothing does) and its
// file, without line numbers, which every edit above the finding shifts.
// The comparison of findings per change and the triage people record both
// use it, so a triage decision follows the finding when lines move.
func FindingFingerprint(rule, owner, file string) string {
	sum := sha256.Sum256([]byte(rule + "\x00" + owner + "\x00" + file))
	return hex.EncodeToString(sum[:])
}

// FindingOwnerName is the owner part of FindingFingerprint for a finding in
// file owned by owner (nil when no symbol owns it).
func FindingOwnerName(owner *Node, file string) string {
	if owner == nil {
		return file
	}
	return owner.ShortName()
}
