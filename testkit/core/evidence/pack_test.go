package evidence

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func sealed(t *testing.T) *Dir {
	d := &Dir{Root: filepath.Join(t.TempDir(), "r20261005-080000-abcd")}
	if _, err := d.WriteFile("report.html", []byte("<p>ok</p>")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteFile("TC-A/logs/svc.log", []byte("connected to db user=app\n")); err != nil {
		t.Fatal(err)
	}
	if err := d.Seal(&Manifest{RunID: "r20261005-080000-abcd"}); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPackIsReproducibleAndRefusesTampering(t *testing.T) {
	d := sealed(t)
	z1, z2 := filepath.Join(t.TempDir(), "a.zip"), filepath.Join(t.TempDir(), "b.zip")
	h1, err := Pack(d.Root, z1)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := Pack(d.Root, z2)
	if h1 != h2 {
		t.Fatal("packing twice gave different zips")
	}
	zr, err := zip.OpenReader(z1)
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 3 || zr.File[0].Name != "r20261005-080000-abcd/manifest.json" {
		t.Fatalf("%v", zr.File)
	}
	zr.Close()
	_ = os.WriteFile(d.Path("report.html"), []byte("edited"), 0o644)
	if _, err := Pack(d.Root, z1); err == nil {
		t.Fatal("tampered bundle packed")
	}
}

func TestScanSecrets(t *testing.T) {
	d := sealed(t)
	_, _ = d.WriteFile("TC-A/output/req.json", []byte("{\n \"Authorization\": \"Bearer abcdefghijkl\"\n}\npassword=hunter22\n"))
	f, err := ScanSecrets(d.Root, map[string]string{"DB_PASSWORD": "hunter22", "SHORT": "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 2 || f[0].Line != 2 || f[1].What != "value of DB_PASSWORD" {
		t.Fatalf("%+v", f)
	}
}
