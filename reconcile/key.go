package reconcile

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/secretcrypto"
)

// KeyName is the file, beside reconcile.yaml, holding the key the node seals
// its watches' node token and registry secrets with in its KV.
const KeyName = "reconcile.key"

func KeyPath() string {
	return filepath.Join(filepath.Dir(ConfigPath()), KeyName)
}

// LoadSealer opens the node's watch cipher, creating its key on first use.
// Without one the KV store keeps no token at all, and every roll the node
// asks of itself is refused.
func LoadSealer(path string) (*secretcrypto.Cipher, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, secretcrypto.KeyLen)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("generate reconcile key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, fmt.Errorf("write reconcile key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read reconcile key: %w", err)
	}
	return secretcrypto.NewCipherFromKey(key)
}
