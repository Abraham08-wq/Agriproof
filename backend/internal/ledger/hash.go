// Package ledger defines how ledger entries are hash-chained.
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// GenesisHash is the prev_hash of the first entry in every group.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

type Entry struct {
	GroupID   string
	Seq       int64
	Type      string
	MemberID  string // empty when not linked to a member
	Amount    int64
	Reference string
	PrevHash  string
	CreatedAt time.Time
}

// Normalize truncates the timestamp to the precision PostgreSQL stores (microseconds, UTC),
// so the hash computed before insert equals the hash recomputed after reading back.
func Normalize(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// Hash returns the hex SHA-256 of the entry's canonical form.
func Hash(e Entry) string {
	fields := []string{
		e.PrevHash,
		e.GroupID,
		strconv.FormatInt(e.Seq, 10),
		e.Type,
		e.MemberID,
		strconv.FormatInt(e.Amount, 10),
		e.Reference,
		Normalize(e.CreatedAt).Format(time.RFC3339Nano),
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(sum[:])
}

// VerifyResult reports whether a chain of entries is intact.
type VerifyResult struct {
	Valid     bool   `json:"valid"`
	Entries   int    `json:"entries"`
	HeadHash  string `json:"head_hash"`
	BrokenAt  int64  `json:"broken_at_seq,omitempty"`
	BrokenWhy string `json:"reason,omitempty"`
}

// Verify recomputes every hash. stored[i] must be ordered by Seq starting at 1.
func Verify(entries []Entry, storedHashes []string) VerifyResult {
	prev := GenesisHash
	for i, e := range entries {
		if e.Seq != int64(i+1) {
			return VerifyResult{Valid: false, Entries: len(entries), BrokenAt: e.Seq, BrokenWhy: "sequence gap"}
		}
		if e.PrevHash != prev {
			return VerifyResult{Valid: false, Entries: len(entries), BrokenAt: e.Seq, BrokenWhy: "previous hash mismatch"}
		}
		h := Hash(e)
		if h != storedHashes[i] {
			return VerifyResult{Valid: false, Entries: len(entries), BrokenAt: e.Seq, BrokenWhy: "entry was modified"}
		}
		prev = h
	}
	return VerifyResult{Valid: true, Entries: len(entries), HeadHash: prev}
}
