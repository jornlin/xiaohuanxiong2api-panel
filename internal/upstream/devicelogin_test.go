package upstream

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// newDeviceLogin 造一个指向假上游的授权会话（state 固定，便于直接喂回调）。
func newDeviceLogin(t *testing.T, exchange http.HandlerFunc) (*DeviceLogin, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case LoginCodePath:
			exchange(w, r)
		case "/user_info":
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-dev","name":"授权号"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	up := New()
	up.SetAgentHosts(srv.URL, srv.URL)
	up.SetAPIHosts(srv.URL, srv.URL)
	d := &DeviceLogin{client: up, state: "st-1", machine: "m-1"}
	return d, srv
}

// TestAcceptCallbackIdempotent 同一条链接提交两次是**幂等**的，不是「会话已用过」。
//
// 踩过的坑：用户在面板上点第二次（或浏览器自动回调与手工粘贴撞车）时回 400
// 「这个登录会话已经用过了」，看起来像链接失效，于是重开登录、重复两三次才成功。
func TestAcceptCallbackIdempotent(t *testing.T) {
	d, _ := newDeviceLogin(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"at","refresh_token":"rt"}}`)
	})

	if d.Pending() {
		t.Fatal("全新会话不该已经有授权码")
	}
	if err := d.AcceptCallback("st-1", "ac-1"); err != nil {
		t.Fatalf("第一次收码出错: %v", err)
	}
	if !d.Pending() {
		t.Error("收码后 Pending 应为 true（面板据此回报 duplicate）")
	}
	if err := d.AcceptCallback("st-1", "ac-1"); err != nil {
		t.Errorf("重复提交同一个码应幂等放行，得到: %v", err)
	}
	if err := d.AcceptCallback("st-1", "ac-2"); err == nil {
		t.Error("同一个会话收到另一个码应当拒绝（说明前端串了两次登录）")
	}
	if err := d.AcceptCallback("st-other", "ac-1"); err == nil {
		t.Error("state 不匹配应当拒绝")
	}
}

// TestPollKeepsCodeOnTransientError 换票遇到网络层/5xx 失败时不能把授权码丢掉：
// 丢了这一轮就白等，用户只能重复提交，而码是一次性的。
func TestPollKeepsCodeOnTransientError(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	d, _ := newDeviceLogin(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"code":500,"message":"boom"}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"at","refresh_token":"rt"}}`)
	})

	if err := d.AcceptCallback("st-1", "ac-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Poll(t.Context()); err == nil {
		t.Fatal("第一次换票应当失败（上游 500）")
	}
	if !d.Pending() {
		t.Fatal("可重试失败后授权码被丢了：下一轮再也不可能换到票")
	}
	creds, done, err := d.Poll(t.Context())
	if err != nil || !done {
		t.Fatalf("重试换票失败: done=%v err=%v", done, err)
	}
	if creds.UID != "u-dev" {
		t.Errorf("uid = %q", creds.UID)
	}
	// 换票成功后：授权码确实清掉了（不会再换一次票），但 Pending 仍为 true ——
	// 会话进入 spent 状态，这个窗口里（换票成功到会话被删之间，含落盘）重复提交
	// 要按"已收过码"幂等回报，不能装作第一次、更不能把花掉的码装回去。
	if !d.Pending() {
		t.Error("换票成功后 Pending 仍应为 true（spent）")
	}
	if err := d.AcceptCallback("st-1", "ac-1"); err != nil {
		t.Errorf("spent 之后重复提交应幂等放行，得到 %v", err)
	}
	if creds2, done2, err := d.Poll(t.Context()); err != nil || done2 || creds2 != nil {
		t.Errorf("码已用掉后不该再换一次票: creds=%v done=%v err=%v", creds2, done2, err)
	}
}

// TestPollClearsCodeOnBusinessRejection 业务层拒绝 = 这个码废了，清掉别再重试。
func TestPollClearsCodeOnBusinessRejection(t *testing.T) {
	d, _ := newDeviceLogin(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":`+strconv.Itoa(CodeExpired)+`,"message":"authorization code expired"}`)
	})
	if err := d.AcceptCallback("st-1", "ac-used"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Poll(t.Context()); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("期望 ErrCodeExpired，得到 %v", err)
	}
	if d.Pending() {
		t.Error("失效的码该清掉，否则每轮都白打一次上游")
	}
}
