package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrInvalidKey is returned when the token encryption key is not exactly 32 bytes.
	ErrInvalidKey = errors.New("token cipher key must be exactly 32 bytes")
	// ErrCiphertext is returned when ciphertext decryption or authentication fails.
	ErrCiphertext = errors.New("invalid ciphertext")
)

// TokenCipher provides AES-256-GCM encryption and decryption for sensitive tokens at rest.
type TokenCipher struct {
	aead cipher.AEAD
}

// NewTokenCipher initializes a TokenCipher with a 32-byte key for AES-256-GCM.
// Returns an error if len(key) != 32.
func NewTokenCipher(key []byte) (*TokenCipher, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return &TokenCipher{aead: aead}, nil
}

// Encrypt encrypts the plaintext string using AES-256-GCM with a random 12-byte nonce
// prepended to the resulting ciphertext.
func (c *TokenCipher) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	out := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return out, nil
}

// Decrypt decrypts the blob produced by Encrypt.
// Returns an error if the blob is too short, or if decryption or authentication fails.
func (c *TokenCipher) Decrypt(blob []byte) (string, error) {
	nonceSize := c.aead.NonceSize()
	if len(blob) < nonceSize {
		return "", ErrCiphertext
	}
	nonce := blob[:nonceSize]
	ciphertext := blob[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCiphertext, err)
	}
	return string(plaintext), nil
}
