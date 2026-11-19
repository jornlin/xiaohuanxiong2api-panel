package auth

import (
	"encoding/json"
	"testing"
)

// TestParseKeepsProfileFields 钉住「重启不丢资料」：Parse 必须覆盖 Data 的全部字段。
//
// 踩过的坑：Parse 曾用一份手写子集解析，漏了 phone / invited_by / 礼包日期与终态；
// 每次重启 LoadDir 都把它们丢掉，随后任意一次 SaveAtomic 就把文件覆盖成没有这些字段
// （手机号补完又消失、礼包反复重问上游）。
func TestParseKeepsProfileFields(t *testing.T) {
	raw := []byte(`{
		"access_token":"tok","refresh_token":"rt","uid":"u-1","nickname":"n",
		"phone":"13800000000","nation_code":"86","invited_by":"c32ce479",
		"credit":123,"credit_at":"2026-11-19T10:00:00Z","checked_in":"2026-11-19",
		"desktop_grant_at":"2026-11-19","mobile_grant_at":"2026-11-19",
		"desktop_granted":true,"mobile_granted":true
	}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Phone != "13800000000" || a.NationCode != "86" || a.InvitedBy != "c32ce479" {
		t.Fatalf("资料字段丢失: %+v", a.Data)
	}
	if a.DesktopGrantAt != "2026-11-19" || !a.DesktopGranted || !a.MobileGranted {
		t.Fatalf("礼包标记丢失: %+v", a.Data)
	}
	if a.Credit != 123 || a.CheckedIn != "2026-11-19" {
		t.Fatalf("额度/签到字段丢失: %+v", a.Data)
	}

	// 往返：Snapshot → JSON → Parse 仍应保留全部字段（落盘再加载的真实路径）。
	b, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	a2, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse(round-trip): %v", err)
	}
	if a2.Phone != a.Phone || a2.InvitedBy != a.InvitedBy || a2.DesktopGranted != a.DesktopGranted ||
		a2.MobileGranted != a.MobileGranted || a2.DesktopGrantAt != a.DesktopGrantAt {
		t.Fatalf("往返后字段丢失: %+v", a2.Data)
	}
}
