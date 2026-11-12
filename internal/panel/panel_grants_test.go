package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/scheduler"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// newGrantsPanel 组装一个带调度器的面板：新手礼包端点要真走 scheduler（领取逻辑只有一份）。
func newGrantsPanel(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks":
			_, _ = io.WriteString(w, `{"code":0,"data":{"invite":{"invite_code":"inv-1",
				"register_reward_points":1500,"remaining_reward_invitees":20},
				"new_user_task_states":[{"key":"register_pack","status":"completed"},
					{"key":"desktop_login","status":"completed"},
					{"key":"mobile_login","status":"unfinished"}],
				"invitee_task_states":[]}}`)
		case "/api/web/desktop/v1/login/points/grant":
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
		case "/api/web/mobile/v1/login/points/grant":
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt-1", RefreshToken: "drt-1",
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		MachineID: "m-1", UID: "u-1", Nickname: "测试号",
	}})
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})
	pn := New(Config{
		Pool: p, Upstream: up, Scheduler: sch, AuthDir: t.TempDir(), Version: "test",
		Models: func() []map[string]any { return []map[string]any{} },
	})
	return pn, p
}

// TestPanelGrantsClaim 全池/单号领新手礼包：如实回报每笔的 granted，并带上账号池快照。
func TestPanelGrantsClaim(t *testing.T) {
	pn, _ := newGrantsPanel(t)

	code, body := call(t, pn, http.MethodPost, "/panel/api/grants", "{}")
	if code != http.StatusOK {
		t.Fatalf("POST /panel/api/grants = %d (%v)", code, body)
	}
	grants, _ := body["grants"].([]any)
	if len(grants) != 2 {
		t.Fatalf("全池领取返回 %d 笔，期望 2 笔（桌面端 + 移动端）: %v", len(grants), body)
	}
	for _, raw := range grants {
		g, _ := raw.(map[string]any)
		if g["uid"] != "u-1" {
			t.Errorf("面板口径要带 uid: %v", g)
		}
	}
	if _, ok := body["accounts"]; !ok {
		t.Error("领取后应回账号池快照（前端据此刷新表格）")
	}

	code, body = call(t, pn, http.MethodPost, "/panel/api/accounts/u-1/grants", "{}")
	if code != http.StatusOK {
		t.Fatalf("单号领取 = %d (%v)", code, body)
	}
	grants, _ = body["grants"].([]any)
	if len(grants) != 2 {
		t.Fatalf("单号领取返回 %d 笔，期望 2 笔（手动领取每次都真问上游）: %v", len(grants), body)
	}

	// 未知 uid：404，不是一个空结果。
	if code, _ := call(t, pn, http.MethodPost, "/panel/api/accounts/u-nope/grants", "{}"); code != http.StatusNotFound {
		t.Errorf("未知 uid = %d，期望 404", code)
	}
}

// TestPanelTasksStatus 任务状态只读透传：带上分值表，失败项按号回报 error。
func TestPanelTasksStatus(t *testing.T) {
	pn, _ := newGrantsPanel(t)

	code, body := call(t, pn, http.MethodGet, "/panel/api/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("GET /panel/api/tasks = %d (%v)", code, body)
	}
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v", body["accounts"])
	}
	row, _ := accounts[0].(map[string]any)
	if row["uid"] != "u-1" {
		t.Errorf("uid = %v", row["uid"])
	}
	tasks, _ := row["new_user_tasks"].([]any)
	if len(tasks) != 3 {
		t.Fatalf("新人任务 %d 项，期望 3 项: %v", len(tasks), tasks)
	}
	last, _ := tasks[2].(map[string]any)
	if last["key"] != "mobile_login" || last["status"] != "unfinished" || last["points"] != float64(3000) {
		t.Errorf("移动端礼包状态应如实透传（含本地分值）: %v", last)
	}
	inv, _ := row["invite"].(map[string]any)
	if inv["code"] != "inv-1" {
		t.Errorf("邀请码没透传: %v", inv)
	}
}
