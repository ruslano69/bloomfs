package fs

import (
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/ruslano69/bloomfs/block"
)

// E9: a directory stream opened with OpenDir is a frozen snapshot — names added
// or removed after the open do not appear or vanish mid-iteration.
func TestReaddirSnapshotStable(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	for i := 0; i < 10; i++ {
		if _, err := f.Create(root, fmt.Sprintf("f%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	d, err := f.OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	// Read the first page, then mutate the directory heavily.
	first := d.Next(3)
	if len(first) != 3 {
		t.Fatalf("first page = %d names, want 3", len(first))
	}
	f.Create(root, "added-after-open")
	f.Unlink(root, "f00")
	f.Unlink(root, "f09")

	// Drain the rest; the whole stream must equal the original 10 names exactly.
	got := append([]string{}, first...)
	for {
		page := d.Next(4)
		if page == nil {
			break
		}
		got = append(got, page...)
	}
	sort.Strings(got)
	if len(got) != 10 {
		t.Fatalf("snapshot yielded %d names, want 10 (mutations leaked into the stream)", len(got))
	}
	for i, name := range got {
		want := fmt.Sprintf("f%02d", i)
		if name != want {
			t.Fatalf("snapshot[%d] = %q, want %q", i, name, want)
		}
	}
}

// Next paginates exactly: every name appears once across pages, no gaps or dups,
// and n <= 0 drains the remainder.
func TestReaddirPagination(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	const n = 25
	for i := 0; i < n; i++ {
		f.Create(root, fmt.Sprintf("e%02d", i))
	}

	d, _ := f.OpenDir(root)
	seen := map[string]bool{}
	d.Next(0) // n<=0 on a fresh handle drains everything
	// reopen for the real paginated walk
	d, _ = f.OpenDir(root)
	for {
		page := d.Next(7)
		if page == nil {
			break
		}
		if len(page) > 7 {
			t.Fatalf("page of %d exceeds requested 7", len(page))
		}
		for _, name := range page {
			if seen[name] {
				t.Fatalf("duplicate name across pages: %q", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != n {
		t.Fatalf("paginated walk saw %d names, want %d", len(seen), n)
	}
}

// The snapshot is safe to iterate while writers mutate the same directory
// concurrently (run with -race).
func TestReaddirSnapshotConcurrentMutation(t *testing.T) {
	f, err := Format(block.NewMem(16384), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	for i := 0; i < 50; i++ {
		f.Create(root, fmt.Sprintf("base%03d", i))
	}

	d, _ := f.OpenDir(root)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			name := fmt.Sprintf("churn%03d", i)
			f.Create(root, name)
			f.Unlink(root, name)
		}
	}()

	count := 0
	for {
		page := d.Next(5)
		if page == nil {
			break
		}
		count += len(page)
	}
	wg.Wait()
	if count != 50 {
		t.Fatalf("snapshot count = %d, want 50 (frozen at open)", count)
	}
}
