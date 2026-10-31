// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const (
	// vaultIterations 为主密码派生密钥的 PBKDF2 迭代次数。
	vaultIterations = 600_000
	// vaultCheck 是加密后存盘的已知明文，用于校验主密码是否正确。
	vaultCheck = "relay-hub-vault-check-v1"
)

// Vault 持有由主密码派生的对称密钥，只存在于进程内存中。
// 站点访问令牌一律先经 Seal 加密再落盘；进程退出即锁定。
type Vault struct {
	key []byte
}

func deriveKey(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, vaultIterations, 32)
}

func newVault(key []byte) *Vault { return &Vault{key: key} }

func (v *Vault) ready() bool { return v != nil && len(v.key) == 32 }

func (v *Vault) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal 加密并返回 hex(nonce+ciphertext)。
func (v *Vault) Seal(plaintext []byte) (string, error) {
	aead, err := v.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(aead.Seal(nonce, nonce, plaintext, nil)), nil
}

// Open 解密 Seal 产生的数据。
func (v *Vault) Open(hexBlob string) ([]byte, error) {
	if !v.ready() {
		return nil, errors.New("密码库未解锁")
	}
	aead, err := v.aead()
	if err != nil {
		return nil, err
	}
	blob, err := hex.DecodeString(hexBlob)
	if err != nil {
		return nil, errors.New("密文格式错误")
	}
	ns := aead.NonceSize()
	if len(blob) < ns+aead.Overhead()+1 {
		return nil, errors.New("密文长度异常")
	}
	return aead.Open(nil, blob[:ns], blob[ns:], nil)
}

func (v *Vault) sealString(s string) (string, error) { return v.Seal([]byte(s)) }

func (v *Vault) openString(s string) (string, error) {
	b, err := v.Open(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
