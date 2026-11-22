// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKeyUsesTheGivenValues(t *testing.T) {
	pw, salt, generated, err := LoadOrCreateKey(t.TempDir(), "pw", "00112233445566778899aabb")
	if err != nil || generated || pw != "pw" || salt != "00112233445566778899aabb" {
		t.Fatalf("got %q %q %v %v", pw, salt, generated, err)
	}
}

func TestLoadOrCreateKeyRejectsOnlyOne(t *testing.T) {
	if _, _, _, err := LoadOrCreateKey(t.TempDir(), "pw", ""); err == nil {
		t.Fatal("only a password must be rejected")
	}
	if _, _, _, err := LoadOrCreateKey(t.TempDir(), "", "00112233445566778899aabb"); err == nil {
		t.Fatal("only a salt must be rejected")
	}
}

func TestLoadOrCreateKeyGeneratesOnceAndKeepsIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data") // does not exist yet
	pw1, salt1, generated, err := LoadOrCreateKey(dir, "", "")
	if err != nil || !generated || pw1 == "" {
		t.Fatalf("first start: %q %q %v %v", pw1, salt1, generated, err)
	}
	if _, err := New(pw1, salt1); err != nil {
		t.Fatalf("the generated key is not usable: %v", err)
	}
	pw2, salt2, generated, err := LoadOrCreateKey(dir, "", "")
	if err != nil || generated || pw2 != pw1 || salt2 != salt1 {
		t.Fatalf("second start must read the same key: %q %q %v %v", pw2, salt2, generated, err)
	}
	other, _, _, _ := LoadOrCreateKey(t.TempDir(), "", "")
	if other == pw1 {
		t.Fatal("two installations must not share a key")
	}
	if info, err := os.Stat(filepath.Join(dir, KeyFile)); err != nil || info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("the key file must only be readable by the owner: %v %v", info, err)
	}
}

func TestLoadOrCreateKeyDamagedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyFile), []byte("only-one-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadOrCreateKey(dir, "", ""); err == nil {
		t.Fatal("a damaged key file must be an error")
	}
}
