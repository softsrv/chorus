package app_test

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/softsrv/chorus/internal/app"
)

func TestTokenCipher_KeyValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		keyLen  int
		wantErr bool
	}{
		{name: "empty key", keyLen: 0, wantErr: true},
		{name: "short key 16 bytes", keyLen: 16, wantErr: true},
		{name: "short key 31 bytes", keyLen: 31, wantErr: true},
		{name: "exact 32 bytes", keyLen: 32, wantErr: false},
		{name: "long key 33 bytes", keyLen: 33, wantErr: true},
		{name: "long key 64 bytes", keyLen: 64, wantErr: true},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := make([]byte, tc.keyLen)
			cipher, err := app.NewTokenCipher(key)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NewTokenCipher with key length %d: expected error, got nil", tc.keyLen)
				}
				if cipher != nil {
					t.Fatalf("expected nil cipher on error, got %v", cipher)
				}
			} else {
				if err != nil {
					t.Fatalf("NewTokenCipher with key length %d: unexpected error: %v", tc.keyLen, err)
				}
				if cipher == nil {
					t.Fatalf("expected non-nil cipher, got nil")
				}
			}
		})
	}

	t.Run("nil key rejected", func(t *testing.T) {
		t.Parallel()
		cipher, err := app.NewTokenCipher(nil)
		if err == nil {
			t.Fatal("expected error with nil key, got nil")
		}
		if cipher != nil {
			t.Fatalf("expected nil cipher, got %v", cipher)
		}
	})
}

func TestTokenCipher_RoundTrip(t *testing.T) {
	t.Parallel()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cipher, err := app.NewTokenCipher(key)
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}

	testPlaintexts := []string{
		"",
		"ya29.a0AfH6SM...",
		"1//04...",
		"standard-access-token-value-12345",
		strings.Repeat("a", 1024),
	}

	for _, pt := range testPlaintexts {
		enc, err := cipher.Encrypt(pt)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", pt, err)
		}

		dec, err := cipher.Decrypt(enc)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}

		if dec != pt {
			t.Fatalf("round-trip mismatch: got %q, want %q", dec, pt)
		}
	}
}

func TestTokenCipher_DifferentKeyFails(t *testing.T) {
	t.Parallel()

	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	if _, err := rand.Read(key1); err != nil {
		t.Fatalf("generate key1: %v", err)
	}
	if _, err := rand.Read(key2); err != nil {
		t.Fatalf("generate key2: %v", err)
	}

	cipher1, err := app.NewTokenCipher(key1)
	if err != nil {
		t.Fatalf("NewTokenCipher(key1): %v", err)
	}
	cipher2, err := app.NewTokenCipher(key2)
	if err != nil {
		t.Fatalf("NewTokenCipher(key2): %v", err)
	}

	plaintext := "secret-google-refresh-token"
	blob, err := cipher1.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("cipher1.Encrypt: %v", err)
	}

	_, err = cipher2.Decrypt(blob)
	if err == nil {
		t.Fatal("decryption under different key succeeded, expected error")
	}
}

func TestTokenCipher_RandomNonce(t *testing.T) {
	t.Parallel()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cipher, err := app.NewTokenCipher(key)
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}

	plaintext := "constant-token-string"
	enc1, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("first Encrypt: %v", err)
	}

	enc2, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("second Encrypt: %v", err)
	}

	if bytes.Equal(enc1, enc2) {
		t.Fatal("two encryptions of identical plaintext produced identical ciphertexts; nonce must be random")
	}

	// Verify both decrypt back to plaintext
	dec1, err := cipher.Decrypt(enc1)
	if err != nil || dec1 != plaintext {
		t.Fatalf("dec1 mismatch or error: %v, got %q", err, dec1)
	}

	dec2, err := cipher.Decrypt(enc2)
	if err != nil || dec2 != plaintext {
		t.Fatalf("dec2 mismatch or error: %v, got %q", err, dec2)
	}
}

func TestTokenCipher_CorruptCiphertext(t *testing.T) {
	t.Parallel()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cipher, err := app.NewTokenCipher(key)
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}

	// Short blob (< 12 bytes nonce)
	shortBlob := []byte("short")
	if _, err := cipher.Decrypt(shortBlob); err == nil {
		t.Fatal("expected error decrypting blob shorter than nonce size, got nil")
	}

	// Tampered ciphertext
	blob, err := cipher.Encrypt("some-valid-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	tampered := make([]byte, len(blob))
	copy(tampered, blob)
	tampered[len(tampered)-1] ^= 0x01

	if _, err := cipher.Decrypt(tampered); err == nil {
		t.Fatal("expected error decrypting tampered ciphertext, got nil")
	}
}
