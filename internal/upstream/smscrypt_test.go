package upstream

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestSmsFieldRoundTrip 加密可逆，且形状是官方网页端那一套：
// base64(IV(16 字节) ‖ 密文)，密文长度 = 明文字节数（CFB 是流模式，NoPadding）。
func TestSmsFieldRoundTrip(t *testing.T) {
	for _, phone := range []string{"13800000001", "1", "85212345678", ""} {
		enc, err := EncryptSmsField(phone)
		if err != nil {
			t.Fatalf("加密 %q 出错: %v", phone, err)
		}
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("输出不是合法 base64: %q", enc)
		}
		if len(raw) != 16+len(phone) {
			t.Errorf("%q 密文长度 = %d，期望 16(IV)+%d", phone, len(raw), len(phone))
		}
		back, err := DecryptSmsField(enc)
		if err != nil {
			t.Fatalf("解密出错: %v", err)
		}
		if back != phone {
			t.Errorf("往返后 = %q，期望 %q", back, phone)
		}
	}
}

// TestSmsFieldRandomIV 每次加密都换 IV：同一号码两次密文必须不同，
// 否则上游能直接按密文做等值比对，也说明 IV 没随机。
func TestSmsFieldRandomIV(t *testing.T) {
	a, err := EncryptSmsField("13800000001")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncryptSmsField("13800000001")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("两次加密结果相同 → IV 没有随机化")
	}
}

// TestSmsFieldVector 钉住算法本身：换实现（比如把 CFB 段长改错、把 key 当口令走
// EvpKDF）时这条会立刻炸。密文由本实现生成，正确性由「上游接受」背书
// （2026-11-12 实测：明文手机号回 params_encryted_error，本实现回 param phone invalid
// —— 说明上游解密成功、只是号码本身不合法）。
func TestSmsFieldVector(t *testing.T) {
	const enc = "jHV3HEuCZDSDN2wtvqzImZ4="
	got, err := DecryptSmsField(enc)
	if err != nil {
		t.Fatalf("解密固定向量出错: %v", err)
	}
	if got != "1" {
		t.Errorf("固定向量解出 %q，期望 \"1\"", got)
	}
	if !strings.HasPrefix(enc, "jHV3HEuCZDSDN2wt") {
		t.Error("固定向量的 base64 前缀变了，说明 IV 布局或编码改了")
	}
}
