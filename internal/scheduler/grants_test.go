package scheduler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// TestGrantsClaimedWhenCheckinAlreadyDone 回归：签到已签（already 短路）时，
// 一次性新手礼包仍然要领。
//
// 踩过的坑：每日 300 分是上游 00:00 自动入账的，CheckinStatus 几乎总是 checked_in=true，
// 签到流程走 already 短路、根本到不了 claim 分支。此前两笔礼包挂在 claim 成功路径上
// （claimWithRetry → claimMobileGrant），于是移动端那 3000 一次都没被试过——
// 实测账号 7982218 只有注册礼包 + 桌面端到账，余额差 3000。
func TestGrantsClaimedWhenCheckinAlreadyDone(t *testing.T) {
	desktopCalls, mobileCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":6300,"daily_points":300}}`)
		case "/bills":
			// 今天已有 daily_grant：CheckinStatus 判「已签」，签到流程走 already。
			today := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[{"biz_type":"daily_grant",`+
				`"points":300,"grant_at":"`+today+`T00:00:00+08:00"}]}}`)
		case "/api/web/desktop/v1/login/points/grant":
			desktopCalls++
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
		case "/api/web/mobile/v1/login/points/grant":
			mobileCalls++
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true,"popup":{"source":"mobile_login","points":3000}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt", RefreshToken: "drt",
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		MachineID: "m-1", UID: "u-1",
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	res, err := s.CheckinUID("u-1")
	if err != nil {
		t.Fatalf("CheckinUID 出错: %v", err)
	}
	if res.Status != "already" {
		t.Fatalf("状态 = %q，期望 already（%s）", res.Status, res.Reason)
	}
	if desktopCalls != 1 || mobileCalls != 1 {
		t.Fatalf("已签短路时礼包领取次数 desktop=%d mobile=%d，期望各 1 次", desktopCalls, mobileCalls)
	}
	if len(res.Grants) != 2 {
		t.Fatalf("res.Grants = %+v，期望两笔", res.Grants)
	}
	var mobileGranted bool
	for _, g := range res.Grants {
		if g.Key == "mobile_login" {
			mobileGranted = g.Granted
		}
	}
	if !mobileGranted {
		t.Errorf("移动端礼包的 granted 没如实回报: %+v", res.Grants)
	}
	// 礼包是一次性收入，不能混进每日签到收益的口径里。
	if res.Credits != 0 {
		t.Errorf("Credits = %d，期望 0（礼包不计入签到收益）", res.Credits)
	}

	// 再签一次：进程内已记「问过上游」，不该重复打这两条端点。
	if _, err := s.CheckinUID("u-1"); err != nil {
		t.Fatalf("第二次 CheckinUID 出错: %v", err)
	}
	if desktopCalls != 1 || mobileCalls != 1 {
		t.Errorf("第二轮重复领礼包了 desktop=%d mobile=%d", desktopCalls, mobileCalls)
	}
}

// TestGrantsNoDuplicateOnFailure 回归：礼包领取失败时，同一轮里不能再打一次。
//
// 踩过的坑：claimWithRetry 里也调了一次 claimNewcomerGrants，失败路径不落标记，
// 于是紧随其后的统一补领又来一遍——同一轮里同一端点被打两次，正是这个文件反复
// 强调要避免的秒级重复请求（9074 风控会因为连着打而延长限流窗口）。
func TestGrantsNoDuplicateOnFailure(t *testing.T) {
	cases := []struct {
		name        string
		failDesktop bool
		failMobile  bool
		wantDesktop int
		wantMobile  int
	}{
		{name: "移动端失败", failMobile: true, wantDesktop: 1, wantMobile: 1},
		{name: "桌面端失败", failDesktop: true, wantDesktop: 1, wantMobile: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desktopCalls, mobileCalls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/balance":
					_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":100,"daily_points":300}}`)
				case "/bills":
					// 流水里没有 daily_grant → 签到走 claim 分支（要试一次每日领取），
					// 礼包步骤紧跟在它后面——重复请求就发生在这两处之间。
					_, _ = io.WriteString(w, `{"code":0,"data":{"items":[]}}`)
				case "/api/web/desktop/v1/login/points/grant":
					desktopCalls++
					if tc.failDesktop {
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"code":500000,"message":"boom"}`)
						return
					}
					_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
				case "/api/web/mobile/v1/login/points/grant":
					mobileCalls++
					if tc.failMobile {
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"code":500000,"message":"boom"}`)
						return
					}
					_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			up := upstream.New()
			up.SetAPIHosts(srv.URL, srv.URL)
			up.SetAgentHosts(srv.URL, srv.URL)
			p := pool.New("")
			p.Add(&auth.Auth{Data: auth.Data{
				AccessToken: "dt", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
				MachineID: "m-1", UID: "u-dup",
			}})
			s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

			// 桌面端失败时 CheckinUID 本身会如实返回错误（每日领取没成），这是既有口径，
			// 不是本轮要断言的：这里只关心「同一轮里别再打第二次」。
			if _, err := s.CheckinUID("u-dup"); err != nil && !tc.failDesktop {
				t.Fatalf("CheckinUID 出错: %v", err)
			}
			// 桌面端：签到流程的每日领取打一次；失败时不补打。移动端：只由礼包步骤打一次。
			if desktopCalls != tc.wantDesktop {
				t.Errorf("桌面端端点调用 %d 次，期望 %d 次（同一轮不得重复）", desktopCalls, tc.wantDesktop)
			}
			if mobileCalls != tc.wantMobile {
				t.Errorf("移动端端点调用 %d 次，期望 %d 次（同一轮不得重复）", mobileCalls, tc.wantMobile)
			}
		})
	}
}

// TestClaimNewcomerNowForcesUpstream 面板上的手动领取不受进程内「已问过」标记限制：
// 显式点一次就该真问上游一次（标记只是省幂等请求，不是「已领到」的凭据）。
func TestClaimNewcomerNowForcesUpstream(t *testing.T) {
	mobileCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/web/desktop/v1/login/points/grant":
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
		case "/api/web/mobile/v1/login/points/grant":
			mobileCalls++
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(), UID: "u-2",
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	for i := 1; i <= 2; i++ {
		grants, err := s.ClaimNewcomerNow("u-2")
		if err != nil {
			t.Fatalf("第 %d 次 ClaimNewcomerNow 出错: %v", i, err)
		}
		if len(grants) != 2 {
			t.Fatalf("第 %d 次返回 %+v，期望两笔", i, grants)
		}
		for _, g := range grants {
			if g.UID != "u-2" {
				t.Errorf("面板口径要带 uid: %+v", g)
			}
			if g.Key != "desktop_login" && g.Key != "mobile_login" {
				t.Errorf("未知礼包 key: %+v", g)
			}
		}
	}
	if mobileCalls != 2 {
		t.Errorf("手动领取调用上游 %d 次，期望 2 次（每次点击都真问）", mobileCalls)
	}
}

// TestRunNewcomerNowSkipsDisabled 禁用账号不参与全池领取（与签到同口径）。
func TestRunNewcomerNowSkipsDisabled(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true,"popup":null}}`)
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(), UID: "u-3",
	}})
	p.Disable("u-3", "测试禁用")
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	if grants := s.RunNewcomerNow(); len(grants) != 0 {
		t.Fatalf("禁用账号不该领礼包: %+v", grants)
	}
	if calls != 0 {
		t.Errorf("禁用账号仍然打了上游 %d 次", calls)
	}
}

// TestDesktopGrantReportedOnClaimPath 回归：桌面端那笔礼包是被「每日登录领取」顺带领掉的
// （同一条端点），这条路径也必须把结果报出来 —— 否则面板/日志只显示移动端那笔，
// 看着像漏领了 3000 分，而这正是用户最需要一眼看清的事。
func TestDesktopGrantReportedOnClaimPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":5000,"daily_points":300}}`)
		case "/bills":
			// 流水空 → 今天还没签、也没有 daily_grant → 走 default 分支（要试每日领取）
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[]}}`)
		case "/api/web/desktop/v1/login/points/grant":
			// 首次：桌面端礼包真发出去了
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true,"popup":{"source":"desktop_login","points":3000}}}`)
		case "/api/web/mobile/v1/login/points/grant":
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(), UID: "u-report",
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	res, err := s.CheckinUID("u-report")
	if err != nil {
		t.Fatalf("CheckinUID 出错: %v", err)
	}
	var desktop, mobile *NewcomerGrant
	for i := range res.Grants {
		switch res.Grants[i].Key {
		case "desktop_login":
			desktop = &res.Grants[i]
		case "mobile_login":
			mobile = &res.Grants[i]
		}
	}
	if desktop == nil {
		t.Fatalf("签到分支领到的桌面端礼包没被报出来：res.Grants=%+v", res.Grants)
	}
	if !desktop.Granted {
		t.Errorf("桌面端礼包 granted=true 没如实回报：%+v", *desktop)
	}
	if mobile == nil {
		t.Errorf("移动端那笔也应当在同一轮回报：%+v", res.Grants)
	}
	if res.Credits != 0 {
		t.Errorf("Credits=%d，礼包不该并进每日签到收益", res.Credits)
	}
}
