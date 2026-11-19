package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// balancePayload 是 2026-11-19 线上抓到的真实余额响应（uid 7982218）。
const balancePayload = `{"code":0,"message":"success","data":{
	"available_points":47524,
	"daily_points":0,
	"monthly_points":0,
	"reward_points":47524,
	"topup_points":0,
	"enterprise_topup_points":0,
	"total_chargeable_points":47524,
	"enterprise_reserved":false,
	"topup_frozen":false,
	"owner_type":"users",
	"owner_id":7982218}}`

func newBalanceServer(t *testing.T, payload string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/balance" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	return up
}

// TestBalanceParsesChargeableAndOwner 新口径字段必须真的被解析出来：
// total_chargeable_points / frozen / owner 归属都是面板与选号要用的。
func TestBalanceParsesChargeableAndOwner(t *testing.T) {
	up := newBalanceServer(t, balancePayload)
	got, err := up.UserEntUsageDetail(&auth.Auth{Data: auth.Data{UID: "7982218"}})
	if err != nil {
		t.Fatalf("UserEntUsageDetail: %v", err)
	}
	if got.Remain != 47524 || got.Reward != 47524 || got.Daily != 0 {
		t.Errorf("余额桶 = %+v", got)
	}
	if !got.ChargeableKnown || got.Chargeable != 47524 {
		t.Errorf("可扣费积分 = %d known=%v，期望 47524/true", got.Chargeable, got.ChargeableKnown)
	}
	if !got.BucketsExact {
		t.Error("四桶齐全时应标记为权威拆分")
	}
	if got.Total != 0 {
		t.Errorf("国内积分没有总额，Total 应为 0（假分母修复），得到 %d", got.Total)
	}
	if got.TopupFrozen || got.EnterpriseReserved {
		t.Error("frozen/reserved 应为 false")
	}
	if got.OwnerID != "7982218" || got.OwnerMismatch {
		t.Errorf("归属 = %q mismatch=%v", got.OwnerID, got.OwnerMismatch)
	}
}

// TestBalanceOwnerMismatch 余额归属与本地 uid 不同 = 凭证串号，必须显式标记。
func TestBalanceOwnerMismatch(t *testing.T) {
	up := newBalanceServer(t, balancePayload)
	got, err := up.UserEntUsageDetail(&auth.Auth{Data: auth.Data{UID: "1111111"}})
	if err != nil {
		t.Fatalf("UserEntUsageDetail: %v", err)
	}
	if !got.OwnerMismatch {
		t.Fatal("owner_id=7982218、本地 uid=1111111，应判定为串号")
	}
}

// TestBalanceMissingBucketsAreZero 上游省略字段时必须按 0 处理：
// mustInt 的 -1 一旦写进池子，汇总会变负、构成条百分比算出负宽度。
func TestBalanceMissingBucketsAreZero(t *testing.T) {
	up := newBalanceServer(t, `{"code":0,"data":{"available_points":900}}`)
	got, err := up.UserEntUsageDetail(&auth.Auth{Data: auth.Data{UID: "u-1"}})
	if err != nil {
		t.Fatalf("UserEntUsageDetail: %v", err)
	}
	if got.Daily != 0 || got.Monthly != 0 || got.Reward != 0 || got.Topup != 0 {
		t.Errorf("缺失桶应钳成 0，得到 %+v", got)
	}
	if got.BucketsExact {
		t.Error("字段缺失时不是权威拆分")
	}
	if got.ChargeableKnown || got.Chargeable != 900 {
		t.Errorf("无 total_chargeable_points 时应回落到 available（known=false），得到 %d/%v",
			got.Chargeable, got.ChargeableKnown)
	}
}

// TestBills403QuietWindow v1 /bills 403 后应静默 30 分钟，不再逐号重打。
func TestBills403QuietWindow(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":403,"message":"forbidden"}`)
	}))
	defer srv.Close()
	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	a := &auth.Auth{Data: auth.Data{UID: "u-1"}}

	if exp, pts := up.earliestBatchExpiry(a); exp != 0 || pts != 0 {
		t.Fatalf("403 时不应有到期信息：%d/%d", exp, pts)
	}
	up.earliestBatchExpiry(a) // 第二次：静默窗口内不应再发请求
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("静默窗口内仍在重打 v1 bills：命中 %d 次，期望 1 次", n)
	}
}
