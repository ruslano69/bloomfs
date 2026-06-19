package fs

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// E8: many goroutines appending to one file must not lose, duplicate or
// interleave records. Append holds the write lock across read-size + write, so
// each record lands contiguously at a unique offset. Run with -race.
func TestAppendConcurrency(t *testing.T) {
	f, err := Format(block.NewMem(16384), testKey())
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.Create(f.Root(), "log")
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 32
	const perGoroutine = 50
	var wg sync.WaitGroup
	errc := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				rec := []byte(fmt.Sprintf("g%02d-i%03d;", g, i)) // fixed-width token
				if _, err := f.Append(id, rec); err != nil {
					errc <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("append: %v", err)
	}

	data, err := f.ReadFile(id)
	if err != nil {
		t.Fatal(err)
	}
	const tokenLen = len("g00-i000;")
	want := goroutines * perGoroutine
	if len(data) != want*tokenLen {
		t.Fatalf("file is %d bytes, want %d (lost/duplicated/torn records)", len(data), want*tokenLen)
	}

	// Every record must appear exactly once, none torn (a torn record would parse
	// as an unexpected token).
	seen := make(map[string]bool, want)
	for _, tok := range strings.Split(strings.TrimSuffix(string(data), ";"), ";") {
		if seen[tok] {
			t.Fatalf("duplicate record %q", tok)
		}
		seen[tok] = true
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			tok := fmt.Sprintf("g%02d-i%03d", g, i)
			if !seen[tok] {
				t.Fatalf("missing record %q", tok)
			}
		}
	}
}

// Append returns the offset each record was written at; offsets must be unique,
// contiguous and equal to the running size.
func TestAppendOffsets(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.Create(f.Root(), "f")
	var want uint64
	for i := 0; i < 10; i++ {
		rec := []byte(fmt.Sprintf("rec-%d|", i))
		off, err := f.Append(id, rec)
		if err != nil {
			t.Fatal(err)
		}
		if off != want {
			t.Fatalf("append %d returned off %d, want %d", i, off, want)
		}
		want += uint64(len(rec))
	}
	if in, _ := f.inodes.Get(id); in.Size != want {
		t.Fatalf("final size %d, want %d", in.Size, want)
	}
}
