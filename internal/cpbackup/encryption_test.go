package cpbackup

import (
	"bytes"
	"context"
	"filippo.io/age"
	"strings"
	"testing"
)

type captureWriter struct {
	data  []byte
	calls int
}

func (w *captureWriter) Put(_ context.Context, _ string, b []byte, _ string) error {
	w.calls++
	w.data = append([]byte(nil), b...)
	return nil
}
func TestEncryptedBeforePutAndAuthenticatedRestore(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	a := New([]string{"confidential-organization"}, nil)
	store := &captureWriter{}
	if err = PutEncrypted(context.Background(), store, "backup", a, id.Recipient().String()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(store.data, []byte("confidential-organization")) || !bytes.HasPrefix(store.data, []byte("age-encryption.org/")) {
		t.Fatal("plaintext uploaded")
	}
	got, err := DecryptArchive(store.data, strings.NewReader(id.String()))
	if err != nil || got.SHA256 != a.SHA256 {
		t.Fatal("round trip", err)
	}
	wrong, _ := age.GenerateX25519Identity()
	if _, err = DecryptArchive(store.data, strings.NewReader(wrong.String())); err == nil {
		t.Fatal("wrong identity accepted")
	}
	store.data[len(store.data)-1] ^= 1
	if _, err = DecryptArchive(store.data, strings.NewReader(id.String())); err == nil {
		t.Fatal("tampered envelope accepted")
	}
	if err = PutEncrypted(context.Background(), store, "backup", a, ""); err == nil || store.calls != 1 {
		t.Fatal("invalid recipient uploaded")
	}
	plaintext, _ := Encode(a)
	if _, err = DecryptArchive(plaintext, strings.NewReader(id.String())); err == nil {
		t.Fatal("plaintext accepted")
	}
}
