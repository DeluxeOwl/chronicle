package accountv2

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

func encrypt(plaintext []byte, key []byte) ([]byte, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(c)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	// We prepend the nonce to the ciphertext.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(ciphertext []byte, key []byte) ([]byte, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(c)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

type CryptoTransformer struct {
	key []byte
}

func NewCryptoTransformer(key []byte) *CryptoTransformer {
	return &CryptoTransformer{
		key: key,
	}
}

func (t *CryptoTransformer) TransformForWrite(
	ctx context.Context,
	event AccountEvent,
) (AccountEvent, error) {
	if opened, isOpened := event.(*accountOpened); isOpened {
		fmt.Println("Received \"accountOpened\" event")

		encryptedName, err := encrypt([]byte(opened.HolderName), t.key)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt holder name: %w", err)
		}
		opened.HolderName = base64.StdEncoding.EncodeToString(encryptedName)

		fmt.Printf("Holder name after encryption and encoding: %s\n", opened.HolderName)
	}

	return event, nil
}

func (t *CryptoTransformer) TransformForRead(
	ctx context.Context,
	event AccountEvent,
) (AccountEvent, error) {
	if opened, isOpened := event.(*accountOpened); isOpened {
		fmt.Printf("Holder name before decoding: %s\n", opened.HolderName)

		decoded, err := base64.StdEncoding.DecodeString(opened.HolderName)
		if err != nil {
			return nil, fmt.Errorf("failed to decode encrypted name: %w", err)
		}

		fmt.Printf("Holder name before decryption: %s\n", decoded)
		decryptedName, err := decrypt(decoded, t.key)
		if err != nil {
			// This happens if the key is wrong (or "deleted")
			return nil, fmt.Errorf("failed to decrypt holder name: %w", err)
		}
		opened.HolderName = string(decryptedName)
		fmt.Printf("Holder name after decryption: %s\n", opened.HolderName)
	}

	return event, nil
}
