package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadS3Credentials(t *testing.T) {
	t.Setenv("TACHYON_S3_ACCESS_KEY_ID", "")
	t.Setenv("TACHYON_S3_SECRET_KEY", "")
	if _, err := loadS3Credentials("", ""); err == nil {
		t.Fatal("empty credentials accepted")
	}
	t.Setenv("TACHYON_S3_ACCESS_KEY_ID", "id")
	t.Setenv("TACHYON_S3_SECRET_KEY", "sec")
	c, err := loadS3Credentials("", "")
	if err != nil || c.AccessKey != "id" || c.SecretKey != "sec" {
		t.Fatalf("env: %v", err)
	}
	dir := t.TempDir()
	kf := filepath.Join(dir, "key")
	if err := os.WriteFile(kf, []byte("filesecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = loadS3Credentials("", kf)
	if err != nil || c.SecretKey != "filesecret" || c.AccessKey != "id" {
		t.Fatalf("file: %v", err)
	}
	if _, err := loadS3Credentials("", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}
