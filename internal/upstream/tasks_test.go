package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	if got.Invite.MarketingText != "" {
		t.Errorf("没有活动时 marketing_text 应为空: %q", got.Invite.MarketingText)
	}
}

// TestTasksMarketingText 活动期上游会在 invite 里下发运营文案，要原样带出给面板展示。
func TestTasksMarketingText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{
			"invite":{"invite_code":"c1","marketing_text":"春节活动：邀请好友双倍积分",
				"register_reward_points":1500,"registered_invitee_count":0,
				"granted_points":0,"remaining_reward_invitees":20},
			"new_user_task_states":[],"invitee_task_states":[]}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	got, err := up.Tasks(&auth.Auth{Data: auth.Data{AccessToken: "dt", UID: "u-1"}})
	if err != nil {
		t.Fatalf("Tasks 出错: %v", err)
	}
	if got.Invite.MarketingText != "春节活动：邀请好友双倍积分" {
		t.Errorf("marketing_text = %q", got.Invite.MarketingText)
	}
}

// TestDailyGrantReadsV2Bills v1 /bills 已 403（2026-11-19），签到口径必须走 v2：
// 今天/历史/最近发放时间三个字段都要从 v2 的 created_at 正确解析。
func TestDailyGrantReadsV2Bills(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	today := time.Now().In(cst).Format("2006-01-02")
	old := time.Now().AddDate(0, 0, -5).In(cst).Format("2006-01-02")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/bills" {
			t.Errorf("应走 v2 流水，实际 %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("type"); got != "income" {
			t.Errorf("type = %q，期望 income（只要发放行）", got)
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"has_more":false,"items":[
			{"biz_type":"daily_grant","points":300,"created_at":"`+today+`T00:00:00+08:00"},
			{"biz_type":"daily_grant","points":300,"created_at":"`+old+`T00:00:00+08:00"},
			{"biz_type":"reward_grant","points":3000,"created_at":"`+today+`T01:00:00+08:00"}]}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	todayHit, ever, last, err := up.DailyGrant(&auth.Auth{Data: auth.Data{AccessToken: "dt", UID: "u-1"}})
	if err != nil {
		t.Fatalf("DailyGrant 出错: %v", err)
	}
	if !todayHit || !ever {
		t.Errorf("today=%v ever=%v，期望都为 true", todayHit, ever)
	}
	if got := last.In(cst).Format("2006-01-02"); got != today {
		t.Errorf("最近发放 = %s，期望今天 %s（取最新一条）", got, today)
	}
}

// TestDailyGrantDetectsStopped 上游停发：历史有、今天没有、最近一条在很久以前。
func TestDailyGrantDetectsStopped(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	old := time.Now().AddDate(0, 0, -6).In(cst).Format("2006-01-02")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"has_more":false,"items":[
			{"biz_type":"daily_grant","points":300,"created_at":"`+old+`T00:00:00+08:00"}]}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	todayHit, ever, last, err := up.DailyGrant(&auth.Auth{Data: auth.Data{AccessToken: "dt", UID: "u-1"}})
	if err != nil {
		t.Fatalf("DailyGrant 出错: %v", err)
	}
	if todayHit {
		t.Error("今天没有 daily_grant，today 应为 false")
	}
	if !ever || last.IsZero() {
		t.Errorf("ever=%v last=%v，应认得出历史发放", ever, last)
	}
	if got := last.In(cst).Format("2006-01-02"); got != old {
		t.Errorf("最近发放 = %s，期望 %s", got, old)
	}
}

// TestInviteRecordsParses 邀请记录：20 条/页的分页参数、四项阶段、奖励资格原样解析。
func TestInviteRecordsParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/invite-records" {
			http.NotFound(w, r)
			return
		}
		if q := r.URL.Query(); q.Get("paging.offset") != "20" || q.Get("paging.limit") != "20" {
			t.Errorf("分页参数 = %q/%q", q.Get("paging.offset"), q.Get("paging.limit"))
		}
		_, _ = io.WriteString(w, `{"code":0,"message":"success","data":{
			"items":[
				{"type":"invitee","key":"r1","status":"completed","masked_user":"138****1234",
				 "registered_at":"2026-11-01T10:00:00+08:00","active_days":3,
				 "stages":{"register":{"status":"completed","points":3000},
				           "desktop_login":{"status":"completed","points":3000},
				           "active_7d":{"status":"completed","points":1000},
				           "first_payment":{"status":"unfinished"}},
				 "granted_points":1500,"inviter_reward_eligible":true},
				"不是对象"],
			"paging":{"offset":20,"limit":20,"total":41}}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)

	page, err := up.InviteRecords(&auth.Auth{Data: auth.Data{AccessToken: "dt", UID: "u-1"}}, 20)
	if err != nil {
		t.Fatalf("InviteRecords 出错: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("记录 %d 条，期望 1 条（非对象项丢弃）: %+v", len(page.Items), page.Items)
	}
	rec := page.Items[0]
	if rec.MaskedUser != "138****1234" || rec.ActiveDays != 3 || rec.GrantedPoints != 1500 ||
		!rec.InviterRewardEligible || rec.RegisteredAt == "" {
		t.Errorf("记录字段解析错: %+v", rec)
	}
	if rec.Stages["register"].Status != "completed" || rec.Stages["register"].Points != 3000 {
		t.Errorf("register 阶段 = %+v", rec.Stages["register"])
	}
	if rec.Stages["first_payment"].Status != "unfinished" {
		t.Errorf("first_payment 阶段 = %+v", rec.Stages["first_payment"])
	}
	if page.Paging.Total != 41 || page.Paging.Offset != 20 || page.Paging.Limit != 20 {
		t.Errorf("分页解析错: %+v", page.Paging)
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
