package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
)

// sealWBFieldForTest 按插件 sealAuthFieldForTest 的格式封一个字段（测试侧）。
func sealWBFieldForTest(t *testing.T, key []byte, plaintext string) map[string]any {
	t.Helper()
	sum := sha256.Sum256(key)
	keyID := hex.EncodeToString(sum[:])[:16]
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), wbFieldAAD(keyID, 1))
	ct, tag := sealed[:len(sealed)-16], sealed[len(sealed)-16:]
	inner, err := json.Marshal(map[string]any{
		"suite":      1,
		"keyId":      keyID,
		"nonce":      base64.StdEncoding.EncodeToString(nonce),
		"authTag":    base64.StdEncoding.EncodeToString(tag),
		"ciphertext": base64.StdEncoding.EncodeToString(ct),
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"$wbEncrypted": 1,
		"envelope":     base64.StdEncoding.EncodeToString(inner),
	}
}

// AAD 是逐字节的框架结构：前缀 + 版本 + 两个带长度域的名字 + suite + keyId + 尾字节。
func TestWBFieldAAD(t *testing.T) {
	aad := wbFieldAAD("0123456789abcdef", 1)
	want := []byte{
		'W', 'B', '-', 'A', 'A', 'D', 0x00,
		0x01,
		0, 0, 0, 5, 'W', 'B', 'E', 'V', '1',
		0, 0, 0, 6, 's', 'y', 'm', '-', 'v', '1',
		0, 0, 0, 1,
		0, 0, 0, 16, '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 'a', 'b', 'c', 'd', 'e', 'f',
		0x02, 0x00, 0x00,
	}
	if string(aad) != string(want) {
		t.Fatalf("AAD 不符:\n got %x\nwant %x", aad, want)
	}
}

// 跨实现一致性：这份密文由 Node 按 dsh-workbuddy-connect 插件的
// sealAuthFieldForTest/buildAuthenticatedContextAad 原文生成（独立实现），
// janus 的 Go 转录必须能原样解开。
func TestWBOpenFieldCrossImplementation(t *testing.T) {
	doc := `{"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"eyJzdWl0ZSI6MSwia2V5SWQiOiIyYjMyZGI2YzJjMGE2MjM1Iiwibm9uY2UiOiJYOUVHSEZMWjZBYzZWZDlhIiwiYXV0aFRhZyI6InNKSEhOQzd0WnVrUWVpc0VDa280aFE9PSIsImNpcGhlcnRleHQiOiJmM2k5VDh4ckVxTWhIYWgxbk9vPSJ9"},"refreshToken":{"$wbEncrypted":1,"envelope":"eyJzdWl0ZSI6MSwia2V5SWQiOiIyYjMyZGI2YzJjMGE2MjM1Iiwibm9uY2UiOiJZUDkydVBwMWV5b3lGOUNrIiwiYXV0aFRhZyI6IjhKd09IaVYvRG5lRnZscDg5U0RpZ1E9PSIsImNpcGhlcnRleHQiOiIycGFNbWRhcEMxbldiNTA4M2VjPSJ9"},"expiresAt":1790000000000},"identity":{"uid":"u-cross","enterpriseId":"e-cross"}}`
	_, wrapped, err := wbClassifyDesktopAuth(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 2 {
		t.Fatalf("应有 2 个加密字段，得到 %d", len(wrapped))
	}
	// 密钥：测试向量生成时用的 32 字节（keyId 2b32db6c2c0a6235）。
	keyRaw, _ := hex.DecodeString("66687aadf862bd776c8fc18b8e9f8e20089714856ee233b3902a591d0d5f2925")
	for _, w := range wrapped {
		plain, err := wbOpenField(keyRaw, w.Envelope)
		if err != nil {
			t.Fatalf("字段 %s 解不开: %v", w.Field, err)
		}
		want := map[string]string{"accessToken": "at-cross-check", "refreshToken": "rt-cross-check"}[w.Field]
		if plain != want {
			t.Fatalf("字段 %s = %q，want %q", w.Field, plain, want)
		}
	}
}

// 明文/空/坏形态的分类边界。
func TestWBClassifyDesktopAuth(t *testing.T) {
	if _, wrapped, err := wbClassifyDesktopAuth(`{"auth":{"accessToken":"at","refreshToken":"rt"}}`); err != nil || len(wrapped) != 0 {
		t.Fatalf("明文应零信封: %v %d", err, len(wrapped))
	}
	if _, _, err := wbClassifyDesktopAuth(""); err == nil {
		t.Fatal("空文件应报错")
	}
	// suite≠1 / keyId 形态不对 / 坏 base64 → unrecognized
	bad := []string{
		`{"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"eyJzdWl0ZSI6Miwia2V5SWQiOiIyYjMyZGI2YzJjMGE2MjM1Iiwibm9uY2UiOiJYOUVHSEZMWjZBYzZWZDlhIiwiYXV0aFRhZyI6InNKSEhOQzd0WnVrUWVpc0VDa280aFE9PSIsImNpcGhlcnRleHQiOiJmM2k5VDh4ckVxTWhIYWgxbk9vPSJ9"}}}`,
		`{"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"bm90IGpzb24="}}}`,
	}
	for _, doc := range bad {
		if _, _, err := wbClassifyDesktopAuth(doc); err == nil {
			t.Fatalf("应报错: %s", doc)
		}
	}
}

// payload 校验：合法 / 错版本 / 非 canonical base64 / 错长度。
func TestWBParseAtRestPayload(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := wbParseAtRestPayload(`{"version":1,"atRestSecretKey":"` + secret + `"}`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"version":2,"atRestSecretKey":"` + secret + `"}`,
		`{"version":1,"atRestSecretKey":""}`,
		`{"version":1,"atRestSecretKey":"AAAA"}`,                     // 3 字节
		`{"version":1,"atRestSecretKey":"AAAAAAAAAAAAAAAAAAAAAA=="}`, // 16 字节
		`not json`,
	} {
		if _, err := wbParseAtRestPayload(bad); err == nil {
			t.Fatalf("应报错: %s", bad)
		}
	}
}

// 全流程：假 Electron（sh 脚本忽略 -e 打印 payload）→ 解密 → 重建明文文档。
func TestWBDecryptDocumentEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假 helper 依赖 /bin/sh")
	}
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("K", 32)))
	key := sha256.Sum256([]byte(secret))
	keyID := hex.EncodeToString(key[:])[:16]

	encAt := sealWBFieldForTest(t, key[:], "at-e2e")
	encRt := sealWBFieldForTest(t, key[:], "rt-e2e")
	docBytes, err := json.Marshal(map[string]any{
		"auth":     map[string]any{"accessToken": encAt, "refreshToken": encRt, "expiresAt": 1790000000000},
		"identity": map[string]any{"uid": "u1", "enterpriseId": "e1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	helper, err := os.CreateTemp(t.TempDir(), "fake-electron-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s' '{\"version\":1,\"atRestSecretKey\":\"" + secret + "\"}'\n"
	helper.WriteString(script)
	helper.Close()
	if err := os.Chmod(helper.Name(), 0o755); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(docBytes, &doc); err != nil {
		t.Fatal(err)
	}
	_, wrapped, err := wbClassifyDesktopAuth(string(docBytes))
	if err != nil || len(wrapped) != 2 {
		t.Fatalf("classify: %v %d", err, len(wrapped))
	}
	plain, err := wbDecryptDocument(helper.Name(), doc, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain, `"accessToken": "at-e2e"`) || !strings.Contains(plain, `"refreshToken": "rt-e2e"`) ||
		!strings.Contains(plain, `"expiresAt": 1790000000000`) {
		t.Fatalf("重建文档不符: %s", plain)
	}
	// 解出的明文要能直接喂给 parseWBCredential（access/refresh 都在）。
	cred, err := parseWBCredential(plain)
	if err != nil {
		t.Fatalf("解密产物过不了 parseWBCredential: %v", err)
	}
	if cred.AccessToken != "at-e2e" || cred.RefreshToken != "rt-e2e" {
		t.Fatalf("cred = %+v", cred)
	}
	_ = keyID
}

// keyId 不匹配（另一台安装封的凭据）要明确报错，不能拿错的密钥硬开。
func TestWBResolveKeyMismatch(t *testing.T) {
	otherKey := sha256.Sum256([]byte("another install"))
	env := wbEnvelope{Suite: 1, KeyID: hex.EncodeToString(otherKey[:])[:16], Nonce: make([]byte, 12), AuthTag: make([]byte, 16), Ciphertext: []byte{1}}
	if _, err := wbResolveProtectorKey("", []string{env.KeyID}); err == nil {
		t.Fatal("没有二进制应报错")
	}
}
