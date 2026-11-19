package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// 网关限流会拉起全局闸门；等待必须尊重 context，不能被 30s 安全阀拖住。
func TestWaitGateHonorsContext(t *testing.T) {
	c := New()
	c.noteChatReject(403, "u-1", "", []byte(`{"message":"too many requests"}`))
	c.throttleMu.Lock()
	gated := time.Now().Before(c.gateUntil)
	c.throttleMu.Unlock()
	if !gated {
		t.Fatal("403 限流未拉起闸门")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	c.WaitGate(ctx)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("context 到期后仍等待了 %s", d)
	}
}

// 普通权限 403 不应拉起全局闸门（否则一个模型权限错误会拖慢整池）。
func TestPlain403DoesNotGate(t *testing.T) {
	c := New()
	c.noteChatReject(403, "u-1", "", []byte(`{"message":"insufficient permissions"}`))
	c.throttleMu.Lock()
	gated := time.Now().Before(c.gateUntil)
	c.throttleMu.Unlock()
	if gated {
		t.Fatal("权限 403 不应触发全局闸门")
	}
}

// 单账号频率 429（线上 code=200108）不应拉全局闸门：换号即可，
// 全局暂停会把一个号的限流放大成整池变慢。
func TestAccountRateLimitDoesNotGate(t *testing.T) {
	c := New()
	c.noteChatReject(429, "u-1", "", []byte(`{"error":{"code":200108,"message":"您的模型请求频率超限，请稍后重试"}}`))
	c.throttleMu.Lock()
	gated := time.Now().Before(c.gateUntil)
	c.throttleMu.Unlock()
	if gated {
		t.Fatal("账号级 429 不应拉起全局闸门")
	}
	// 模型排队（serviceAvailable=false）是单模型窗口，也不该停掉其它模型。
	c.noteChatReject(429, "u-2", "", []byte(`{"message":"busy","serviceAvailable":false}`))
	c.throttleMu.Lock()
	gated = time.Now().Before(c.gateUntil)
	c.throttleMu.Unlock()
	if gated {
		t.Fatal("模型排队 429 不应触发全局闸门")
	}
	// 网关级标记（X-TT-System-Error）才是全局的。
	c.noteChatReject(403, "u-3", "3", []byte(`{"message":"forbidden"}`))
	c.throttleMu.Lock()
	gated = time.Now().Before(c.gateUntil)
	c.throttleMu.Unlock()
	if !gated {
		t.Fatal("X-TT-System-Error 403 应拉起闸门")
	}
}

// X-TT-System-Error 头是网关级限流标记，正文可能为空：必须归 ErrBusy，
// 否则 handler 会把它当权限错误，轮完所有号后仍把 403 甩给客户端。
func TestChat403WithTTHeaderIsBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-TT-System-Error", "3")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New()
	c.SetAgentHosts(srv.URL, srv.URL)
	_, status, _, err := c.ChatStream(&auth.Auth{Data: auth.Data{UID: "u-1"}}, []byte(`{}`), "")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d", status)
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrBusy || !ue.Throttled {
		t.Fatalf("err = %+v，期望 ErrBusy+Throttled", err)
	}
}
