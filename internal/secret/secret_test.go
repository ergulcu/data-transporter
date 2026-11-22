// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	b, err := New("change-me", "00112233445566778899aabb")
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"secret", "", "şifre with spaces & symbols €"} {
		sealed := b.Encrypt(plain)
		if sealed == plain {
			t.Errorf("%q was not encrypted", plain)
		}
		got, err := b.Decrypt(sealed)
		if err != nil || got != plain {
			t.Errorf("Decrypt = %q, %v; want %q", got, err, plain)
		}
	}
	if b.Encrypt("same") == b.Encrypt("same") {
		t.Error("every encryption must use a new nonce")
	}
}

func TestAnotherKeyCannotDecrypt(t *testing.T) {
	a, _ := New("one", "00112233445566778899aabb")
	b, _ := New("two", "00112233445566778899aabb")
	c, _ := New("one", "00112233445566778899")
	sealed := a.Encrypt("secret")
	for _, box := range []*Box{b, c} {
		if _, err := box.Decrypt(sealed); !errors.Is(err, ErrWrongKey) {
			t.Errorf("err = %v", err)
		}
	}
	if _, err := a.Decrypt("not base64!"); !errors.Is(err, ErrWrongKey) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := a.Decrypt(sealed[:len(sealed)-4] + "AAAA"); !errors.Is(err, ErrWrongKey) {
		t.Errorf("a changed value must not decrypt: %v", err)
	}
}

func TestBadKeys(t *testing.T) {
	for _, k := range [][2]string{{"", "00112233445566778899aabb"}, {"pw", "zz"}, {"pw", "abcd"}} {
		if _, err := New(k[0], k[1]); err == nil {
			t.Errorf("New(%q, %q) must fail", k[0], k[1])
		}
	}
}
