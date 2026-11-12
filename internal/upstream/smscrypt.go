package upstream

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// smsKey 是官方网页端给手机号/密码做对称加密的密钥（网页 bundle 里的字面量）。
//
// 出处：raccoon-lab/office 的登录 chunk，等价于这段 JS——
//
//	function enc(e, t = "senseraccoon2023") {
//	  const key = CryptoJS.enc.Utf8.parse(t);      // 16 字节 → AES-128
//	  const iv  = CryptoJS.lib.WordArray.random(16);
//	  const out = CryptoJS.AES.encrypt(e, key, {iv, mode: CryptoJS.mode.CFB, padding: CryptoJS.pad.NoPadding});
//	  return iv.concat(out.ciphertext).toString(CryptoJS.enc.Base64);   // base64(IV ‖ 密文)
//	}
//
// 同名密钥也用于 login_with_password 的 password 字段。**这是个对称密钥、写在前端里**，
// 不是安全边界，只是上游要求的传输编码：明文手机号会被上游拒（实测
// `params_encryted_error` / `param phone encrypted error`）。
const smsKey = "senseraccoon2023"

// ErrSmsEncrypt 表示手机号加密失败（熵源不可用等，正常不会发生）。
var ErrSmsEncrypt = errors.New("手机号加密失败")

// EncryptSmsField 复刻官方网页端的字段加密：AES-128-CFB + 随机 IV，IV 明文拼在密文前。
//
// CryptoJS 的 CFB 段长默认 128 位，与 Go 的 cipher.NewCFBEncrypter 等价；
// CFB 是流模式，NoPadding 天然成立（不需要补位）。
func EncryptSmsField(plain string) (string, error) {
	block, err := aes.NewCipher([]byte(smsKey))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSmsEncrypt, err)
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSmsEncrypt, err)
	}
	sealed := make([]byte, len(plain))
	cipher.NewCFBEncrypter(block, iv).XORKeyStream(sealed, []byte(plain))
	return base64.StdEncoding.EncodeToString(append(iv, sealed...)), nil
}

// DecryptSmsField 是 EncryptSmsField 的逆运算，只用于测试与排障（上游只吃密文）。
func DecryptSmsField(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(raw) < aes.BlockSize {
		return "", errors.New("密文短于一个 IV")
	}
	block, err := aes.NewCipher([]byte(smsKey))
	if err != nil {
		return "", err
	}
	iv, sealed := raw[:aes.BlockSize], raw[aes.BlockSize:]
	plain := make([]byte, len(sealed))
	cipher.NewCFBDecrypter(block, iv).XORKeyStream(plain, sealed)
	return string(plain), nil
}
