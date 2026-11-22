// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyFile is the name of the file, inside the data directory, that keeps a generated key.
const KeyFile = "crypto.key"

// LoadOrCreateKey returns the password and salt for the saved database passwords.
//
// When both are given (from the environment) they are used as they are. When neither is given, the key is
// read from KeyFile in dir; on the first start a random one is generated and written there with
// permissions that only the owner can read, so the saved passwords can still be decrypted after a restart.
// Giving only one of the two is an error, because it is almost certainly a mistake.
func LoadOrCreateKey(dir, password, saltHex string) (pw, salt string, generated bool, err error) {
	switch {
	case password != "" && saltHex != "":
		return password, saltHex, false, nil
	case password != "" || saltHex != "":
		return "", "", false, errors.New("set both DATATRANSPORTER_CRYPTO_PASSWORD and DATATRANSPORTER_CRYPTO_SALT, or neither")
	}

	path := filepath.Join(dir, KeyFile)
	if raw, err := os.ReadFile(path); err == nil {
		lines := strings.Fields(string(raw))
		if len(lines) != 2 {
			return "", "", false, fmt.Errorf("%s is damaged: expected a password and a salt", path)
		}
		return lines[0], lines[1], false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", false, err
	}

	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", "", false, err
	}
	pw, salt = rand.Text()+rand.Text(), hex.EncodeToString(saltBytes)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", false, err
	}
	// O_EXCL: if two instances start together, only one writes; the other reads the winner's file.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return LoadOrCreateKey(dir, "", "")
	}
	if err != nil {
		return "", "", false, err
	}
	_, werr := f.WriteString(pw + "\n" + salt + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", "", false, werr
	}
	return pw, salt, true, nil
}
