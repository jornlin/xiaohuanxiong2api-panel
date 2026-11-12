package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// TestTasksParsesRealPayload 用上游真实响应（2026-11-12 抓包，uid 7982218）验解析：
// 只有 GET，响应里的 new_user_task_states 是 [{key,status}]，分值得靠本地表补。
func TestTasksParsesRealPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("任务状态应当只读：收到 %s", r.Method)
		}
		_, _ = io.WriteString(w, `{"code":0,"message":"success","data":{
			"invite":{"granted_points":0,"invite_code":"c32ce479531e42a88ff6f230cdeabbf7",
				"marketing_text":"","register_reward_points":1500,
				"registered_invitee_count":0,"remaining_reward_invitees":20},
			"invitee_task_states":[],
			"new_user_task_states":[
				{"key":"register_pack","status":"completed"},
				{"key":"desktop_login","status":"completed"},
				{"key":"mobile_login","status":"completed"}]}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	got, err := up.Tasks(&auth.Auth{Data: auth.Data{AccessToken: "dt", UID: "u-1"}})
	if err != nil {
		t.Fatalf("Tasks 出错: %v", err)
	}
	if len(got.NewUser) != 3 {
		t.Fatalf("新人任务 %d 项，期望 3 项: %+v", len(got.NewUser), got.NewUser)
	}
	if got.NewUser[0].Key != "register_pack" || got.NewUser[0].Points != 3000 {
		t.Errorf("第一项 = %+v，期望 register_pack/3000（分值来自本地表）", got.NewUser[0])
	}
	if got.NewUser[2].Key != "mobile_login" || got.NewUser[2].Status != "completed" {
		t.Errorf("第三项 = %+v", got.NewUser[2])
	}
	if got.Invite.Code != "c32ce479531e42a88ff6f230cdeabbf7" || got.Invite.RegisterRewardPoints != 1500 ||
		got.Invite.RemainingRewardInvitees != 20 {
		t.Errorf("邀请信息解析错: %+v", got.Invite)
	}
}

// TestTasksNormalizes 缺 status 记 unfinished、无名项跳过、未知 key 不给分值。
func TestTasksNormalizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"new_user_task_states":[
			{"key":"active_7d"},
			{"key":"brand_new_task","status":"in_progress"},
			{"status":"completed"},
			"不是对象"],"invitee_task_states":null}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	got, err := up.Tasks(&auth.Auth{Data: auth.Data{AccessToken: "dt", UID: "u-1"}})
	if err != nil {
		t.Fatalf("Tasks 出错: %v", err)
	}
	if len(got.NewUser) != 2 {
		t.Fatalf("归一后 %d 项，期望 2 项（无 key 的丢弃）: %+v", len(got.NewUser), got.NewUser)
	}
	if got.NewUser[0].Status != "unfinished" || got.NewUser[0].Points != 1000 {
		t.Errorf("缺 status 应记 unfinished 且补分值: %+v", got.NewUser[0])
	}
	if got.NewUser[1].Points != 0 {
		t.Errorf("表外 key 不该编分值: %+v", got.NewUser[1])
	}
	if got.Invitee == nil {
		t.Error("null 数组要归一成空切片，不是 nil（面板省一次判空）")
	}
}

// TestClaimDesktopGrantReportsGranted 桌面端礼包：granted 原样回报，且**不**碰本地签到台账
// （它只是那条端点的一次性礼包语义，签到台账由 CheckinClaim 负责）。
func TestClaimDesktopGrantReportsGranted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/web/desktop/v1/login/points/grant" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("x-client-platform"); got != "desktop-windows-x64" {
			t.Errorf("桌面端礼包的 x-client-platform = %q", got)
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true,"popup":{"source":"desktop_login","points":3000}}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	a := &auth.Auth{Data: auth.Data{AccessToken: "dt", UID: "u-1"}}
	granted, err := up.ClaimDesktopGrant(a)
	if err != nil {
		t.Fatalf("ClaimDesktopGrant 出错: %v", err)
	}
	if !granted {
		t.Error("granted=true 没如实回报")
	}
	if a.CheckedInToday() {
		t.Error("一次性礼包不该顺手记「今天已签」")
	}
}
