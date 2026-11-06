package ledger

import (
	"testing"
	"time"
)

func buildChain(n int) ([]Entry, []string) {
	var entries []Entry
	var hashes []string
	prev := GenesisHash
	base := time.Date(2026, 10, 29, 10, 0, 0, 123456789, time.UTC)
	for i := 1; i <= n; i++ {
		e := Entry{
			GroupID: "g1", Seq: int64(i), Type: "contribution", MemberID: "m1",
			Amount: int64(i * 100), Reference: "REF", PrevHash: prev, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		h := Hash(e)
		entries = append(entries, e)
		hashes = append(hashes, h)
		prev = h
	}
	return entries, hashes
}

func TestVerifyIntactChain(t *testing.T) {
	entries, hashes := buildChain(5)
	r := Verify(entries, hashes)
	if !r.Valid || r.HeadHash != hashes[4] {
		t.Fatalf("expected valid chain, got %+v", r)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	entries, hashes := buildChain(5)
	entries[2].Amount = 999999 // someone edits an old contribution
	r := Verify(entries, hashes)
	if r.Valid || r.BrokenAt != 3 {
		t.Fatalf("expected tampering at seq 3, got %+v", r)
	}
}

func TestVerifyDetectsDeletion(t *testing.T) {
	entries, hashes := buildChain(5)
	entries = append(entries[:1], entries[2:]...)
	hashes = append(hashes[:1], hashes[2:]...)
	r := Verify(entries, hashes)
	if r.Valid {
		t.Fatalf("expected deletion to be detected, got %+v", r)
	}
}

func TestNormalizeMatchesPostgresPrecision(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.FixedZone("EAT", 3*3600))
	n := Normalize(ts)
	if n.Nanosecond() != 123456000 || n.Location() != time.UTC {
		t.Fatalf("unexpected normalize result %v", n)
	}
}
