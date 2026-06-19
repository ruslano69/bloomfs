package dir

import (
	"fmt"
	"testing"
)

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("file_%06d.dat", i)
	}
	return out
}

func TestAddFindDelete(t *testing.T) {
	d := New()
	all := names(10000) // > 2*MaxFilesPerSegment forces multiple segments

	for i, n := range all {
		if !d.Add(n, InodeID(i+1)) {
			t.Fatalf("Add(%q) returned false", n)
		}
	}
	if got := d.Len(); got != len(all) {
		t.Fatalf("Len = %d, want %d", got, len(all))
	}
	if d.Segments() < 3 {
		t.Fatalf("expected >=3 segments for %d files, got %d", len(all), d.Segments())
	}

	// Every present file resolves to its inode.
	for i, n := range all {
		got, ok := d.Find(n)
		if !ok || got != InodeID(i+1) {
			t.Fatalf("Find(%q) = (%d,%v), want (%d,true)", n, got, ok, i+1)
		}
	}

	// Absent files are reported absent — no false negatives (a Bloom false
	// positive must still fall through to the index and return false).
	for i := 0; i < len(all); i++ {
		n := fmt.Sprintf("absent_%06d.dat", i)
		if _, ok := d.Find(n); ok {
			t.Fatalf("Find(%q) reported present", n)
		}
	}

	// Delete the even-indexed files; the rest must survive (filter rebuilt).
	for i := 0; i < len(all); i += 2 {
		if !d.Delete(all[i]) {
			t.Fatalf("Delete(%q) returned false", all[i])
		}
	}
	for i, n := range all {
		_, ok := d.Find(n)
		want := i%2 == 1
		if ok != want {
			t.Fatalf("after delete Find(%q) present=%v, want %v", n, ok, want)
		}
	}
	if got := d.Len(); got != len(all)/2 {
		t.Fatalf("Len after deletes = %d, want %d", got, len(all)/2)
	}

	// Duplicate Add is rejected.
	if d.Add(all[1], 99) {
		t.Fatalf("duplicate Add accepted")
	}
}

func benchDir(n int) (*Directory, []string) {
	d := New()
	ns := names(n)
	for i, name := range ns {
		d.Add(name, InodeID(i+1))
	}
	return d, ns
}

// BenchmarkFindHit measures lookup of present files (Bloom admit + index hit).
func BenchmarkFindHit(b *testing.B) {
	d, ns := benchDir(8000) // 2 segments
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := d.Find(ns[i%len(ns)]); !ok {
			b.Fatal("miss on present key")
		}
	}
}

// BenchmarkFindMiss measures lookup of absent files (Bloom rejects, ideally
// without ever touching the index).
func BenchmarkFindMiss(b *testing.B) {
	d, _ := benchDir(8000)
	miss := make([]string, 8000)
	for i := range miss {
		miss[i] = fmt.Sprintf("absent_%06d.dat", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := d.Find(miss[i%len(miss)]); ok {
			b.Fatal("hit on absent key")
		}
	}
}
