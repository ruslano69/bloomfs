package fs

import (
	"sort"
	"testing"

	"github.com/ruslano69/bloomfs/block"
	"github.com/ruslano69/bloomfs/inode"
)

// Stat reports the fields a kernel getattr needs: size, type, mode, owner, links.
func TestStat(t *testing.T) {
	f, err := Format(block.NewMem(4096), testKey())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.Create(f.Root(), "f")
	if err := f.WriteFile(id, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Chown(id, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat(id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Ino != id || st.Size != 5 || st.Type != inode.TypeRegular {
		t.Fatalf("stat = %+v, want ino=%d size=5 type=regular", st, id)
	}
	if st.Nlink != 1 || st.Mode != 0o644 || st.UID != 1000 {
		t.Fatalf("stat = %+v, want nlink=1 mode=0644 uid=1000", st)
	}

	dst, _ := f.Stat(f.Root())
	if dst.Type != inode.TypeDir {
		t.Fatalf("root type = %d, want dir", dst.Type)
	}
}

// Readdirents returns every entry with its id and type.
func TestReaddirents(t *testing.T) {
	f, err := Format(block.NewMem(8192), testKey())
	if err != nil {
		t.Fatal(err)
	}
	root := f.Root()
	d, _ := f.Mkdir(root, "sub")
	file, _ := f.Create(root, "file")

	ents, err := f.Readdirents(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	if len(ents) != 2 {
		t.Fatalf("got %d entries, want 2", len(ents))
	}
	if ents[0].Name != "file" || ents[0].Ino != file || ents[0].Type != inode.TypeRegular {
		t.Fatalf("entry[0] = %+v, want file/%d/regular", ents[0], file)
	}
	if ents[1].Name != "sub" || ents[1].Ino != d || ents[1].Type != inode.TypeDir {
		t.Fatalf("entry[1] = %+v, want sub/%d/dir", ents[1], d)
	}
}
