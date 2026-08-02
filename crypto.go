package agileconfig

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

var cryptoRead = rand.Read

func md5Upper(content string) string {
	ascii := make([]byte, 0, len(content))
	for _, char := range content {
		if char > 127 {
			ascii = append(ascii, '?')
			continue
		}
		ascii = append(ascii, byte(char))
	}
	sum := md5.Sum(ascii)
	return stringsToUpper(hex.EncodeToString(sum[:]))
}

func stringsToUpper(value string) string {
	result := []byte(value)
	for i, char := range result {
		if char >= 'a' && char <= 'f' {
			result[i] = char - ('a' - 'A')
		}
	}
	return string(result)
}

func cacheKey(secret string) []byte {
	first := sha1.Sum([]byte(secret))
	second := sha1.Sum(first[:])
	return second[:aes.BlockSize]
}

func encryptCache(secret string, plain []byte) (string, error) {
	block, err := aes.NewCipher(cacheKey(secret))
	if err != nil {
		return "", err
	}
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	encrypted := make([]byte, len(plain))
	for offset := 0; offset < len(plain); offset += aes.BlockSize {
		block.Encrypt(encrypted[offset:offset+aes.BlockSize], plain[offset:offset+aes.BlockSize])
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func decryptCache(secret, encoded string) ([]byte, error) {
	encrypted, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(encrypted) == 0 || len(encrypted)%aes.BlockSize != 0 {
		return nil, errors.New("invalid encrypted cache length")
	}
	block, err := aes.NewCipher(cacheKey(secret))
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(encrypted))
	for offset := 0; offset < len(encrypted); offset += aes.BlockSize {
		block.Decrypt(plain[offset:offset+aes.BlockSize], encrypted[offset:offset+aes.BlockSize])
	}
	padding := int(plain[len(plain)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plain) {
		return nil, errors.New("invalid encrypted cache padding")
	}
	for _, value := range plain[len(plain)-padding:] {
		if int(value) != padding {
			return nil, errors.New("invalid encrypted cache padding")
		}
	}
	return plain[:len(plain)-padding], nil
}
