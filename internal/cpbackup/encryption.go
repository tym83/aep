package cpbackup

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"filippo.io/age"
)

type ObjectWriter interface {
	Put(context.Context, string, []byte, string) error
}

// PutEncrypted uses a fresh age file key per archive, wrapped for the public recipient.
// The backup process never needs the recovery identity.
func PutEncrypted(ctx context.Context, store ObjectWriter, key string, a Archive, recipient string) error {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return fmt.Errorf("age recipient: %w", err)
	}
	plaintext, err := Encode(a)
	if err != nil {
		return err
	}
	var encrypted bytes.Buffer
	w, err := age.Encrypt(&encrypted, r)
	if err != nil {
		return err
	}
	if _, err = w.Write(plaintext); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	if encrypted.Len() > 256<<20 {
		return fmt.Errorf("encrypted archive exceeds 256 MiB")
	}
	return store.Put(ctx, key, encrypted.Bytes(), "application/octet-stream")
}

func DecryptArchive(ciphertext []byte, identityFile io.Reader) (Archive, error) {
	identities, err := age.ParseIdentities(identityFile)
	if err != nil {
		return Archive{}, err
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), identities...)
	if err != nil {
		return Archive{}, err
	}
	b, err := io.ReadAll(io.LimitReader(r, (256<<20)+1))
	if err != nil {
		return Archive{}, err
	}
	if len(b) > 256<<20 {
		return Archive{}, fmt.Errorf("decrypted archive exceeds 256 MiB")
	}
	return Decode(b)
}
