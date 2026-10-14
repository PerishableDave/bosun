package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	got, err := Parse([]byte("# comment\n\nA=1\nexport B = two words \nC=\"quoted # not a comment\"\nD='x'\nE=\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two words", "C": "quoted # not a comment", "D": "x", "E": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := Parse([]byte("not a pair\n")); err == nil {
		t.Error("want error for a line without =")
	}
	if _, err := Parse([]byte("1BAD=x\n")); err == nil {
		t.Error("want error for a bad key")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if set, err := Load(path); err != nil || set != nil {
		t.Fatalf("missing file: %v %v", set, err)
	}
	t.Setenv("BOSUN_ENVFILE_TEST_PRESET", "kept")
	if err := os.WriteFile(path, []byte("BOSUN_ENVFILE_TEST_NEW=secret\nBOSUN_ENVFILE_TEST_PRESET=overridden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "must be 0600") {
		t.Fatalf("0644 file: err = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("BOSUN_ENVFILE_TEST_NEW") })
	set, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set[0] != "BOSUN_ENVFILE_TEST_NEW" {
		t.Fatalf("set = %v", set)
	}
	if os.Getenv("BOSUN_ENVFILE_TEST_NEW") != "secret" || os.Getenv("BOSUN_ENVFILE_TEST_PRESET") != "kept" {
		t.Fatal("environment not as expected")
	}
}
