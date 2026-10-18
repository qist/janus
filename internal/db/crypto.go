package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// crypto.go —— 凭据加密与密钥派生。
//
// 主密钥来源（优先级）：
//  1. JANUS_DB_KEY（bootstrap env）
//  2. <数据目录>/master.key（首启自动生成，0600）
//
// 派生方式：sha256(master) 作为 AES-256 密钥；不同用途用不同标签做域分离。

var (
	keyMu     sync.RWMutex
	masterKey []byte
)

// ConfigureKey 设置主密钥。secret 为空时从 keyFile 读取，仍不存在则生成并落盘。
// 返回实际使用的密钥来源描述，便于启动日志。
func ConfigureKey(secret, keyFile string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret != "" {
		setMaster([]byte(secret))
		return "env:JANUS_DB_KEY", nil
	}
	if keyFile != "" {
		if raw, err := os.ReadFile(keyFile); err == nil {
			if s := strings.TrimSpace(string(raw)); s != "" {
				setMaster([]byte(s))
				return "file:" + keyFile, nil
			}
		}
		buf := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, buf); err != nil {
			return "", err
		}
		enc := base64.RawURLEncoding.EncodeToString(buf)
		if err := os.MkdirAll(filepath.Dir(keyFile), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(keyFile, []byte(enc+"\n"), 0o600); err != nil {
			return "", err
		}
		setMaster([]byte(enc))
		return "file:" + keyFile + " (generated)", nil
	}
	// 没有落盘位置：进程内随机（重启后旧凭据不可解，仅测试场景）
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	setMaster(buf)
	return "ephemeral", nil
}

func setMaster(b []byte) {
	h := sha256.Sum256(b)
	keyMu.Lock()
	masterKey = h[:]
	keyMu.Unlock()
}

func master() []byte {
	keyMu.RLock()
	defer keyMu.RUnlock()
	if len(masterKey) == 32 {
		return masterKey
	}
	h := sha256.Sum256([]byte("janus-unconfigured-master"))
	return h[:]
}

// deriveKey 用标签做域分离，避免同一主密钥在多个用途间复用。
func deriveKey(label string) []byte {
	m := hmac.New(sha256.New, master())
	m.Write([]byte(label))
	return m.Sum(nil)
}

// EncryptBlob 用 AES-256-GCM 加密任意明文（nonce 前置）。
func EncryptBlob(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(deriveKey("credential"))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// DecryptBlob 解密 EncryptBlob 的输出。
func DecryptBlob(blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(deriveKey("credential"))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

// ---------- 密码哈希（PBKDF2-HMAC-SHA256） ----------

const passwordIter = 120000

// HashPassword 生成 "pbkdf2$iter$salt$hash" 形式的密码摘要。
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		copy(salt, []byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	dk := pbkdf2SHA256([]byte(pw), salt, passwordIter, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", passwordIter, hex.EncodeToString(salt), hex.EncodeToString(dk))
}

// VerifyPassword 校验密码。
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2SHA256 是 PBKDF2-HMAC-SHA256 的最小实现（RFC 8018）。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, numBlocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// RandomToken 生成 n 字节随机数的 URL-safe 编码（用于 key / id）。
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())))
		return hex.EncodeToString(sum[:n])
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashKey 返回 API key 明文的 sha256 十六进制摘要。
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
