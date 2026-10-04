package evidence

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSealAndVerify(t *testing.T) {
	out := t.TempDir()
	id := NewRunID(time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(id, "r20261004-180000-") || !ValidRunID(id) {
		t.Fatalf("run id %q", id)
	}
	d, err := Open(out, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteJSON("TC-1/case.json", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteFile("TC-1/logs/sut.log", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := d.Seal(&Manifest{RunID: id}); err != nil {
		t.Fatal(err)
	}
	if p, err := Verify(d.Root); err != nil || len(p) != 0 {
		t.Fatalf("verify clean: %v %v", p, err)
	}
	_ = os.WriteFile(d.Path("TC-1/logs/sut.log"), []byte("tampered\n"), 0o644)
	_ = os.WriteFile(d.Path("extra.txt"), []byte("x"), 0o644)
	p, _ := Verify(d.Root)
	if strings.Join(p, "|") != "added after seal: extra.txt|modified: TC-1/logs/sut.log" {
		t.Fatalf("verify tampered: %v", p)
	}
}
