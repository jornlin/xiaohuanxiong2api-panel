package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// newSmsPanel 组装一个只关心登录链路的面板（空账号池 + 假上游）。
func newSmsPanel(t *testing.T, handler http.HandlerFunc) *Panel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.SetAgentHosts(srv.URL, srv.URL)
	up.SetAPIHosts(srv.URL, srv.URL)
	return New(Config{
		Pool: pool.New(""), Upstream: up, AuthDir: t.TempDir(), Version: "test",
		Models: func() []map[string]any { return []map[string]any{} },
	})
}

// TestPanelSmsSend 发码端点：手机号密文上行、空 captcha 不带字段、明显笔误本地就拦。
func TestPanelSmsSend(t *testing.T) {
	var got map[string]string
	pn := newSmsPanel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send_sms" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
	})

	code, body := call(t, pn, http.MethodPost, "/panel/api/login/sms/send",
		`{"nation_code":"86","phone":"13800000001"}`)
	if code != http.StatusOK {
		t.Fatalf("发码 = %d (%v)", code, body)
	}
	if back, err := upstream.DecryptSmsField(got["phone"]); err != nil || back != "13800000001" {
		t.Errorf("面板没有把手机号加密后再发给上游: %q %v", got["phone"], err)
	}
	if _, sent := got["captcha_param"]; sent {
		t.Error("空滑块回执不该发给上游")
	}

	// 带区号的手机号是常见笔误：本地就该报清楚，别浪费一次上游请求。
	if code, _ := call(t, pn, http.MethodPost, "/panel/api/login/sms/send",
		`{"nation_code":"86","phone":"+8613800000001"}`); code != http.StatusBadRequest {
		t.Errorf("带 + 的手机号 = %d，期望 400", code)
	}
	if code, _ := call(t, pn, http.MethodPost, "/panel/api/login/sms/send",
		`{"nation_code":"86","phone":""}`); code != http.StatusBadRequest {
		t.Errorf("空手机号 = %d，期望 400", code)
	}
}

// TestPanelSmsLogin 验证码登录：账号落盘到 auths/ 并入池（与其它登录路径同一套 saveAccount）。
func TestPanelSmsLogin(t *testing.T) {
	var gotCode, gotPhone string
	pn := newSmsPanel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login_with_sms":
			var body map[string]string
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			gotCode, gotPhone = body["sms_code"], body["phone"]
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"at-sms","refresh_token":"rt-sms"}}`)
		case "/user_info":
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"7982218","name":"RaccoonJayden"}}`)
		case "/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":9300,"daily_points":300}}`)
		case "/bills":
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[]}}`)
		default:
			http.NotFound(w, r)
		}
	})

	code, body := call(t, pn, http.MethodPost, "/panel/api/login/sms", `{"phone":"13800000001","sms_code":"123456"}`)
	if code != http.StatusOK {
		t.Fatalf("验证码登录 = %d (%v)", code, body)
	}
	if body["uid"] != "7982218" {
		t.Errorf("uid = %v", body["uid"])
	}
	if gotCode != "123456" {
		t.Errorf("上游收到的 sms_code = %q（应明文透传）", gotCode)
	}
	if back, err := upstream.DecryptSmsField(gotPhone); err != nil || back != "13800000001" {
		t.Errorf("上游收到的手机号密文不对: %q %v", back, err)
	}

	// 落盘到 auths/<uid>.json（重启后不丢），并已入池。
	path := filepath.Join(pn.cfg.AuthDir, "7982218.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("凭证没落盘: %v", err)
	}
	if _, ok := pn.cfg.Pool.Status("7982218"); !ok {
		t.Error("账号没入池")
	}

	// 验证码非数字：本地拦下，不打上游。
	if code, _ := call(t, pn, http.MethodPost, "/panel/api/login/sms", `{"phone":"13800000001","sms_code":"12ab"}`); code != http.StatusBadRequest {
		t.Errorf("非数字验证码 = %d，期望 400", code)
	}
}

// TestPanelSmsNations 区号端点永远可用（上游挂了也有兜底表）。
func TestPanelSmsNations(t *testing.T) {
	pn := newSmsPanel(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 上游连区号都不给
	})
	code, body := call(t, pn, http.MethodGet, "/panel/api/login/sms/nations", "")
	if code != http.StatusOK {
		t.Fatalf("区号 = %d", code)
	}
	nations, _ := body["nations"].([]any)
	if len(nations) == 0 {
		t.Fatalf("区号为空: %v", body)
	}
	first, _ := nations[0].(map[string]any)
	if first["code"] != "86" {
		t.Errorf("第一条 = %v，期望 86", first)
	}
}
