// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package secret encrypts the saved database passwords with AES-256-GCM. The key is derived with
// PBKDF2-HMAC-SHA256 from a password and a salt, so the key itself is never stored.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// iterations follows the OWASP recommendation for PBKDF2-HMAC-SHA256.
const iterations = 600_000

// ErrWrongKey means a value cannot be decrypted, usually because the password or salt changed.
var ErrWrongKey = errors.New("the saved password cannot be decrypted; was the crypto key changed?")

// Box encrypts and decrypts short texts.
type Box struct {
	aead cipher.AEAD
}

// New derives the key. The salt is hex, at least 8 bytes (16 characters).
func New(password, saltHex string) (*Box, error) {
	if password == "" {
		return nil, errors.New("the crypto password must not be empty")
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return nil, fmt.Errorf("the crypto salt must be hex: %w", err)
	}
	if len(salt) < 8 {
		return nil, errors.New("the crypto salt must be at least 8 bytes (16 hex characters)")
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Encrypt returns base64 of a random nonce followed by the sealed text.
func (b *Box) Encrypt(plain string) string {
	nonce := make([]byte, b.aead.NonceSize())
	rand.Read(nonce) //nolint:errcheck // never fails
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil))
}

// Decrypt reverses Encrypt; a value sealed with another key gives ErrWrongKey.
func (b *Box) Decrypt(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", ErrWrongKey
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", ErrWrongKey
	}
	return string(plain), nil
}
