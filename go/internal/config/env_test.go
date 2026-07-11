package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvOnlyFillsMissingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("# comment\nA=from-file\nexport B='two words'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A", "explicit")
	_ = os.Unsetenv("B")
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("A") != "explicit" || os.Getenv("B") != "two words" {
		t.Fatalf("A=%q B=%q", os.Getenv("A"), os.Getenv("B"))
	}
}

func TestLoadDotEnvMissingAndInvalid(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(path); err == nil {
		t.Fatal("无等号行应报错")
	}
}
