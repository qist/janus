// workbuddy_decrypt.go —— `janus workbuddy-decrypt` 子命令：把 WorkBuddy 5.6
// 桌面版的加密凭据文件解成明文，供 WORKBUDDY_AUTH_FILE 使用。
//
// 加密格式与密钥方法转录自 dsh-workbuddy-connect 插件（desktop-credential-
// protection.ts，其实现已对 5.6.2 实测验证）：
//
//   - 凭据文件：CodeBuddyExtension/Data/Public/auth/workbuddy-desktop[-ai].info，
//     5.6 起字段静态加密：accessToken/refreshToken 变成
//     {$wbEncrypted:1, envelope:<base64(JSON{suite,keyId,nonce,authTag,ciphertext})>}；
//   - 密钥：WorkBuddy 改版 Electron 的私有 workbuddyStorage 绑定里有个密封的
//     {version:1, atRestSecretKey}，用**它自己的二进制**以 ELECTRON_RUN_AS_NODE=1
//     跑一行脚本即可打印；protectorKey = sha256(atRestSecretKey)，
//     keyId = sha256(protectorKey) 前 16 个 hex（用于自校验）；
//   - 字段解密：AES-256-GCM（nonce 12B / tag 16B / AAD 见 wbFieldAAD），suite 1
//     是字段框架（WBEV1）的唯一方案。
//
// 因此解密必须在**装了 WorkBuddy 桌面版**的机器上执行：桌面机跑
// `janus workbuddy-decrypt -o out.json`，把 out.json 放到 janus 主机配
// WORKBUDDY_AUTH_FILE。明文的 refreshToken 可无限续期，导一次长期可用。

package workbuddy

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// wbHelperScript 是喂给 WorkBuddy Electron 的解封脚本（与插件一致）：
// 进程当成纯 Node 跑（ELECTRON_RUN_AS_NODE=1），调私有绑定打印密封 payload。
const wbHelperScript = `process.stdout.write(String(process._linkedBinding("electron_browser_workbuddy_storage").loggerGet()))`

// wbEnvelope 是信封内层 JSON 的解码形态。
type wbEnvelope struct {
	Suite      int    `json:"suite"`
	KeyID      string `json:"keyId"`
	Nonce      []byte `json:"nonce"`
	AuthTag    []byte `json:"authTag"`
	Ciphertext []byte `json:"ciphertext"`
}

// wbWrappedField 是一个仍包在信封里的凭据字段。
type wbWrappedField struct {
	Field    string
	Envelope wbEnvelope
}

// RunDecrypt 是 `janus workbuddy-decrypt` 的入口。
func RunDecrypt(args []string) {
	fs := flag.NewFlagSet("workbuddy-decrypt", flag.ExitOnError)
	bin := fs.String("bin", "", "WorkBuddy 桌面版可执行文件路径（默认按平台自动发现；找不到时必填）")
	in := fs.String("in", "", "加密凭据文件路径（默认按平台自动发现两个产品的凭据）")
	variant := fs.String("variant", "", "限定产品：cn（WorkBuddy）/ ai（WorkBuddy AI）；默认两个都试")
	out := fs.String("o", "", "输出文件（默认打到 stdout）")
	fs.Parse(args)

	// 1. 定位凭据文件。
	path, text, err := wbFindAuthFile(*in, *variant)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "reading %s\n", path)

	// 2. 分类：明文直接输出；加密走解密链；其余报错。
	doc, wrapped, err := wbClassifyDesktopAuth(text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", path, err)
		os.Exit(1)
	}
	var plain string
	if len(wrapped) == 0 {
		fmt.Fprintln(os.Stderr, "file is already plaintext")
		plain = strings.TrimSpace(text)
	} else {
		plain, err = wbDecryptDocument(*bin, doc, wrapped)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	// 3. 输出：默认 stdout；-o 落盘（0600，凭据敏感）。
	if *out == "" {
		fmt.Println(plain)
		fmt.Fprintln(os.Stderr, "\ndone. 把上面的 JSON 存成文件，在 janus 侧配 WORKBUDDY_AUTH_FILE 指向它即可。")
		return
	}
	if err := os.WriteFile(*out, []byte(plain+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "error: 写输出文件失败:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "done. wrote %s — 把它放到 janus 主机并配 WORKBUDDY_AUTH_FILE 指向它即可。\n", *out)
}

// wbFindAuthFile 定位凭据文件：显式路径优先，否则按平台候选逐一探测。
// 两个产品共用 CodeBuddyExtension 目录、只有文件名不同；-variant 可二选一。
func wbFindAuthFile(explicit, variant string) (string, string, error) {
	if explicit != "" {
		b, err := os.ReadFile(explicit)
		if err != nil {
			return "", "", fmt.Errorf("读取 -in 失败: %w", err)
		}
		return explicit, string(b), nil
	}
	var names []string
	switch variant {
	case "cn":
		names = []string{"workbuddy-desktop.info"}
	case "ai":
		names = []string{"workbuddy-desktop-ai.info"}
	case "":
		names = []string{"workbuddy-desktop.info", "workbuddy-desktop-ai.info"}
	default:
		return "", "", fmt.Errorf("-variant 只认 cn 或 ai，收到了 %q", variant)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	var dirs []string
	switch runtime.GOOS {
	case "darwin":
		dirs = []string{filepath.Join(home, "Library", "Application Support")}
	case "windows":
		// 新版写在 LOCALAPPDATA，老版本在 APPDATA（Roaming），两个都探。
		dirs = []string{
			filepath.Join(home, "AppData", "Local"),
			filepath.Join(home, "AppData", "Roaming"),
		}
	default:
		// Linux：多数发行版写 XDG config home；UOS/deepin 构建写 data home。
		dirs = []string{os.Getenv("XDG_CONFIG_HOME"), os.Getenv("XDG_DATA_HOME")}
		if dirs[0] == "" {
			dirs[0] = filepath.Join(home, ".config")
		}
		if dirs[1] == "" {
			dirs[1] = filepath.Join(home, ".local", "share")
		}
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, name := range names {
			p := filepath.Join(dir, "CodeBuddyExtension", "Data", "Public", "auth", name)
			if b, err := os.ReadFile(p); err == nil {
				return p, string(b), nil
			}
		}
	}
	return "", "", fmt.Errorf("没有找到桌面凭据文件（CodeBuddyExtension/Data/Public/auth/ 下的 %s）；"+
		"用 -in 指定路径，或确认这台机器装了并登录过 WorkBuddy 桌面版", strings.Join(names, " / "))
}

// wbClassifyDesktopAuth 解析桌面凭据文档。返回完整文档与仍在信封里的字段；
// wrapped 为空表示文档本就是明文（或为空）。unrecognized（JSON 坏、信封解不开、
// suite/keyId 形态不对）显式报错——宁可不干，也不盲猜格式。
func wbClassifyDesktopAuth(text string) (map[string]any, []wbWrappedField, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil, errors.New("文件是空的（桌面版可能还没登录）")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, nil, fmt.Errorf("不是合法 JSON: %w", err)
	}
	root := doc
	if auth, ok := doc["auth"].(map[string]any); ok {
		root = auth
	}
	var wrapped []wbWrappedField
	for _, field := range []string{"accessToken", "refreshToken"} {
		v, present := root[field]
		if !present || v == nil {
			continue
		}
		if s, ok := v.(string); ok {
			_ = s // 明文字段：留给 parseWBCredential 的常规路径
			continue
		}
		env, err := wbParseEnvelope(v)
		if err != nil {
			return nil, nil, fmt.Errorf("字段 %s 处于无法识别的形态: %w", field, err)
		}
		wrapped = append(wrapped, wbWrappedField{Field: field, Envelope: *env})
	}
	return doc, wrapped, nil
}

// wbParseEnvelope 解开 {$wbEncrypted:1, envelope:<base64(JSON...)>} 包装。
func wbParseEnvelope(v any) (*wbEnvelope, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("既不是字符串也不是信封对象")
	}
	if n, _ := obj["$wbEncrypted"].(float64); n != 1 {
		return nil, errors.New("缺少 $wbEncrypted:1 标记")
	}
	raw, ok := obj["envelope"].(string)
	if !ok || raw == "" {
		return nil, errors.New("envelope 不是 base64 字符串")
	}
	inner, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("envelope base64 解不开: %w", err)
	}
	var env wbEnvelope
	if err := json.Unmarshal(inner, &env); err != nil {
		return nil, fmt.Errorf("envelope 内层不是合法 JSON: %w", err)
	}
	if env.Suite != 1 {
		return nil, fmt.Errorf("suite=%d 不是已知的字段方案（只认 1）", env.Suite)
	}
	if len(env.Nonce) != 12 {
		return nil, fmt.Errorf("nonce 长度 %d ≠ 12", len(env.Nonce))
	}
	if len(env.AuthTag) != 16 {
		return nil, fmt.Errorf("authTag 长度 %d ≠ 16", len(env.AuthTag))
	}
	if len(env.Ciphertext) == 0 {
		return nil, errors.New("ciphertext 为空")
	}
	if !regexpHex16(env.KeyID) {
		return nil, fmt.Errorf("keyId %q 不是 16 位小写 hex", env.KeyID)
	}
	return &env, nil
}

func regexpHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// wbFieldAAD 复刻桌面版的 buildAuthenticatedContextAad（字段框架 WBEV1）：
// "WB-AAD\0" | 0x01 | len32("WBEV1") | len32("sym-v1") | suite32 | len32(keyId) | 0x02 | 0x00 | 0x00。
func wbFieldAAD(keyID string, suite int) []byte {
	var b bytes.Buffer
	b.WriteString("WB-AAD\x00")
	b.WriteByte(1)
	lp := func(s string) {
		var h [4]byte
		binary.BigEndian.PutUint32(h[:], uint32(len(s)))
		b.Write(h[:])
		b.WriteString(s)
	}
	lp("WBEV1")
	lp("sym-v1")
	var sb [4]byte
	binary.BigEndian.PutUint32(sb[:], uint32(suite))
	b.Write(sb[:])
	lp(keyID)
	b.WriteByte(2)
	b.WriteByte(0)
	b.WriteByte(0)
	return b.Bytes()
}

// wbOpenField 用 protectorKey 解开一个信封，返回明文字符串。
func wbOpenField(key []byte, env wbEnvelope) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, env.Nonce, append(append([]byte{}, env.Ciphertext...), env.AuthTag...), wbFieldAAD(env.KeyID, env.Suite))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// wbDecryptDocument 解封 helper 拿密钥 → 解字段 → 重建明文 JSON。
func wbDecryptDocument(bin string, doc map[string]any, wrapped []wbWrappedField) (string, error) {
	key, err := wbResolveProtectorKey(bin, wbKeyIDs(wrapped))
	if err != nil {
		return "", err
	}
	root := doc
	if auth, ok := doc["auth"].(map[string]any); ok {
		root = auth
	}
	for _, w := range wrapped {
		plain, err := wbOpenField(key, w.Envelope)
		if err != nil {
			return "", fmt.Errorf("解密字段 %s 失败（信封 keyId %s）: %w", w.Field, w.Envelope.KeyID, err)
		}
		root[w.Field] = plain
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// wbKeyIDs 汇总信封引用的 keyId（去重，保持出现顺序）。
func wbKeyIDs(wrapped []wbWrappedField) []string {
	var out []string
	for _, w := range wrapped {
		dup := false
		for _, id := range out {
			if id == w.Envelope.KeyID {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, w.Envelope.KeyID)
		}
	}
	return out
}

// wbResolveProtectorKey 用 WorkBuddy 自己的 Electron 解封 at-rest 密钥：
// spawn（ELECTRON_RUN_AS_NODE=1）→ {version:1,atRestSecretKey} → sha256。
// keyId 与信封对不上说明凭据是另一台安装封的，这台机器的密钥打不开它。
func wbResolveProtectorKey(bin string, wantKeyIDs []string) ([]byte, error) {
	if len(wantKeyIDs) == 0 {
		return nil, errors.New("信封没有携带任何 keyId")
	}
	electron, err := wbFindElectron(bin)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "resolving at-rest key via %s\n", electron)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, electron, "-e", wbHelperScript)
	cmd.Env = append(os.Environ(), "ELECTRON_RUN_AS_NODE=1")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("WorkBuddy key helper 启动失败（%s）: %w；确认 -bin 指向桌面版自己的可执行文件", electron, err)
	}
	payload := strings.TrimSpace(string(out))
	if payload == "" {
		return nil, fmt.Errorf("WorkBuddy key helper 没有输出（%s）；确认这是 WorkBuddy 5.6+ 的桌面版", electron)
	}
	secret, err := wbParseAtRestPayload(payload)
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(secret))
	// keyId = sha256(protectorKey) 前 16 hex（双重哈希，与插件封格式一致）。
	protectorSum := sha256.Sum256(key[:])
	keyID := hex.EncodeToString(protectorSum[:])[:16]
	matched := false
	for _, want := range wantKeyIDs {
		if want == keyID {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("本机 WorkBuddy 的当前密钥（keyId %s）与凭据信封（%s）不匹配——"+
			"凭据是被另一处安装封存的；请在封存凭据的那台机器上执行本命令",
			keyID, strings.Join(wantKeyIDs, " / "))
	}
	return key[:], nil
}

// wbParseAtRestPayload 校验 helper 输出：{version:1, atRestSecretKey:<canonical base64 32B>}。
func wbParseAtRestPayload(text string) (string, error) {
	var payload struct {
		Version         int    `json:"version"`
		AtRestSecretKey string `json:"atRestSecretKey"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return "", fmt.Errorf("key helper 输出不是合法 JSON: %w", err)
	}
	if payload.Version != 1 {
		return "", fmt.Errorf("payload version=%d，只认 1", payload.Version)
	}
	secret := payload.AtRestSecretKey
	if secret == "" {
		return "", errors.New("payload 里没有 atRestSecretKey")
	}
	raw, err := base64.StdEncoding.DecodeString(secret)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("atRestSecretKey 不是 base64 的 32 字节（得到 %d 字节）", len(raw))
	}
	if base64.StdEncoding.EncodeToString(raw) != secret {
		return "", errors.New("atRestSecretKey 不是 canonical base64")
	}
	return secret, nil
}

// wbFindElectron 定位 WorkBuddy 桌面版可执行文件：显式 -bin 优先；否则按平台
// 已验证的默认位置探测（国内版 WorkBuddy 与国际版 WorkBuddyAI 都在
// %LOCALAPPDATA%\Programs 下）。仍找不到时只能用 -bin 指定。
func wbFindElectron(explicit string) (string, error) {
	if explicit != "" {
		if st, err := os.Stat(explicit); err != nil || st.IsDir() {
			return "", fmt.Errorf("-bin 指定的文件不存在: %s", explicit)
		}
		return explicit, nil
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/WorkBuddy.app/Contents/MacOS/Electron",
			"/Applications/WorkBuddy AI.app/Contents/MacOS/Electron",
		}
	case "windows":
		if home != "" {
			candidates = []string{
				filepath.Join(home, "AppData", "Local", "Programs", "WorkBuddy", "WorkBuddy.exe"),
				filepath.Join(home, "AppData", "Local", "Programs", "WorkBuddyAI", "WorkBuddyAI.exe"),
			}
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	hint := "用 -bin 指向 WorkBuddy 桌面版安装目录里的可执行文件"
	switch runtime.GOOS {
	case "darwin":
		hint = "确认 /Applications 下装了 WorkBuddy（或 WorkBuddy AI），或用 -bin 指向 .app/Contents/MacOS/Electron"
	case "windows":
		hint = "默认探测 %LOCALAPPDATA%\\Programs\\WorkBuddy\\WorkBuddy.exe 与 " +
			"%LOCALAPPDATA%\\Programs\\WorkBuddyAI\\WorkBuddyAI.exe；" +
			"都没找到说明安装位置自定义了，请用 -bin 指向其 exe"
	}
	return "", fmt.Errorf("没有自动找到 WorkBuddy 桌面版可执行文件；%s", hint)
}
