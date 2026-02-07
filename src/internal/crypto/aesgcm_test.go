package crypto

import (
	"encoding/base64"
	"testing"
)

func TestAESGCMEncryptDecrypt(t *testing.T) {
	key := make([]byte, 32)
	b64 := base64.StdEncoding.EncodeToString(key)
	c, err := NewAESGCMFromBase64Key(b64)
	if err != nil {
		t.Fatalf("NewAESGCMFromBase64Key: %v", err)
	}
	plain := []byte("secret")
	blob, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("decrypt mismatch: got=%q want=%q", got, plain)
	}
}

