package dedup

import "testing"

func key(b byte) Key {
	var k Key
	k[0] = b
	return k
}

func TestRefCounting(t *testing.T) {
	tab := New()
	k := key(1)

	tab.Add(k, Entry{Start: 100, Count: 2})
	if tab.Len() != 1 {
		t.Fatalf("Len = %d, want 1", tab.Len())
	}
	e, ok := tab.Lookup(k)
	if !ok || e.Refs != 1 || e.Start != 100 {
		t.Fatalf("Lookup = %+v,%v", e, ok)
	}

	// Two more references (e.g. two more identical files).
	tab.Incr(k)
	tab.Incr(k)

	// Three releases; only the last frees the block.
	if _, freed := tab.Decr(k); freed {
		t.Fatal("freed too early at refs 3->2")
	}
	if _, freed := tab.Decr(k); freed {
		t.Fatal("freed too early at refs 2->1")
	}
	freedEntry, freed := tab.Decr(k)
	if !freed || freedEntry.Start != 100 {
		t.Fatalf("expected free with Start=100, got %+v,%v", freedEntry, freed)
	}
	if tab.Len() != 0 {
		t.Fatalf("Len after free = %d, want 0", tab.Len())
	}
}

func TestDecrAbsent(t *testing.T) {
	tab := New()
	if _, freed := tab.Decr(key(9)); freed {
		t.Fatal("Decr on absent key reported freed")
	}
	if ok := tab.Incr(key(9)); ok {
		t.Fatal("Incr on absent key reported ok")
	}
}
