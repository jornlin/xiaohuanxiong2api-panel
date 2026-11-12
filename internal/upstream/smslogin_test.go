package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// testJWT 是形状正确的假 JWT（exp=2030-01-01），payload 与真实票同构。
const testJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
	"eyJleHAiOjE4OTM0NTYwMDAsInN1YiI6Ijc5ODIyMTgiLCJ1c2VyX2lkIjoiNzk4MjIxOCJ9.sig"

// TestSendSmsCodeEncryptsPhone 发码请求必须带密文手机号（明文会被上游拒），
// 且 captcha_param 只在真的有时才发。
func TestSendSmsCodeEncryptsPhone(t *testing.T) {
	type reqBody struct {
		Nation  string `json:"nation_code"`
		Phone   string `json:"phone"`
		Captcha string `json:"captcha_param"`
	}
	var got reqBody
	var gotRaw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != SendSmsPath {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_ = json.Unmarshal(raw, &gotRaw)
		_, _ = io.WriteString(w, `{"code":0,"message":"success","data":{}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAgentHosts(srv.URL, srv.URL)

	if err := up.SendSmsCode("86", "13800000001", ""); err != nil {
		t.Fatalf("SendSmsCode 出错: %v", err)
	}
	if got.Nation != "86" {
		t.Errorf("nation_code = %q", got.Nation)
	}
	if got.Phone == "13800000001" || got.Phone == "" {
		t.Errorf("phone 没有加密: %q", got.Phone)
	}
	if back, err := DecryptSmsField(got.Phone); err != nil || back != "13800000001" {
		t.Errorf("密文解不回原号码: %q %v", back, err)
	}
	if _, sent := gotRaw["captcha_param"]; sent {
		t.Error("空 captcha_param 不该出现在请求体里")
	}

	if err := up.SendSmsCode("86", "13800000001", "captcha-token"); err != nil {
		t.Fatalf("带滑块回执时出错: %v", err)
	}
	if got.Captcha != "captcha-token" {
		t.Errorf("captcha_param = %q，期望原样透传", got.Captcha)
	}
}

// TestLoginWithSmsBuildsAccount 验证码登录：手机号加密、sms_code 明文（官方页就是这么发的），
// uid 以 user_info 为准。
func TestLoginWithSmsBuildsAccount(t *testing.T) {
	var gotPhone, gotCode, gotNation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case LoginSmsPath:
			var body map[string]string
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			gotPhone, gotCode, gotNation = body["phone"], body["sms_code"], body["nation_code"]
			// access_token 用真 JWT 形状（带 exp）：到期时间是从 token 里解出来的，
			// 拿个随便的字符串会让 expires_at 静默为 0。
			_, _ = io.WriteString(w, `{"code":0,"message":"success","data":{"access_token":"`+testJWT+`",
				"refresh_token":"rt-1","user_id":"pu-platform-id","office_org_name":"登录企业"}}`)
		case "/user_info":
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"7982218","name":"RaccoonJayden"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := New()
	up.SetAgentHosts(srv.URL, srv.URL)

	acc, err := up.LoginWithSms("86", "13800000001", "123456")
	if err != nil {
		t.Fatalf("LoginWithSms 出错: %v", err)
	}
	if gotNation != "86" || gotCode != "123456" {
		t.Errorf("nation/sms_code = %q/%q，期望 86/123456", gotNation, gotCode)
	}
	if back, err := DecryptSmsField(gotPhone); err != nil || back != "13800000001" {
		t.Errorf("登录请求里的手机号密文不对: %q %v", back, err)
	}
	if acc.AccessToken != testJWT || acc.RefreshToken != "rt-1" {
		t.Errorf("凭证没落到账号上: %+v", acc.Data)
	}
	if acc.UID != "7982218" {
		t.Errorf("uid = %q，期望 user_info 的 office id（登录响应里的 pu- 是平台 id）", acc.UID)
	}
	if acc.Nickname != "RaccoonJayden" {
		t.Errorf("nickname = %q", acc.Nickname)
	}
	if acc.ExpiresAt != 1893456000000 {
		t.Errorf("expires_at = %d，期望从 JWT 的 exp 解出 1893456000000", acc.ExpiresAt)
	}
}

// TestLoginWithSmsFriendlyErrors 上游业务码换成人话，面板直接展示。
func TestLoginWithSmsFriendlyErrors(t *testing.T) {
	cases := []struct {
		name string
		code int
		want string
	}{
		{"验证码不对", CodeSmsCodeVerify, "验证码不正确或已过期"},
		{"手机号不合法", CodeParamsInvalid, "手机号不合法"},
		{"加密被拒", CodeParamsEncrypted, "加密方式与官方不一致"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 上游真实形态：HTTP 400 + 业务码（payloadOf 会先把状态码归成通用错误，
				// 所以翻译必须按响应体里的业务码走）。
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":`+strconv.Itoa(tc.code)+`,"message":"upstream english message","details":"more detail"}`)
			}))
			defer srv.Close()

			up := New()
			up.SetAgentHosts(srv.URL, srv.URL)
			_, err := up.LoginWithSms("86", "13800000001", "000000")
			if err == nil {
				t.Fatal("业务失败应当报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), tc.want)
			}
			// 上游原话要留着，排障时不至于只剩一句人话。
			if !strings.Contains(err.Error(), "upstream english message") {
				t.Errorf("上游原文被吞了: %q", err.Error())
			}
		})
	}
}

// TestSendSmsCaptchaErrorExplained 发码被人机校验挡住时，要说清「为什么面板做不到」
// 以及「去哪儿取码」——上游这条失败没有已收录的业务码（官方 web 端也只用一句
// captchaVerifyFailed），所以按 message 文本判。
func TestSendSmsCaptchaErrorExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":100008,"message":"captcha_verify_error","details":"captcha verify failed"}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAgentHosts(srv.URL, srv.URL)

	err := up.SendSmsCode("86", "13800000001", "")
	if err == nil {
		t.Fatal("应当报错")
	}
	for _, want := range []string{"人机校验", "官方页面", "不需要滑块"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 %q 缺少 %q", err.Error(), want)
		}
	}
}

// TestSmsNationsFallback 区号拉不到时用内置兜底表（不该让登录页不可用）。
func TestSmsNationsFallback(t *testing.T) {
	up := New()
	up.SetAgentHosts("http://127.0.0.1:1", "http://127.0.0.1:1") // 必然连不上
	nations := up.SmsNations()
	if len(nations) == 0 {
		t.Fatal("兜底表为空")
	}
	if nations[0].Code != "86" {
		t.Errorf("兜底第一条 = %+v，期望中国大陆 86", nations[0])
	}
}

// TestSmsNationsFallbackOnBadPayload 上游回了但形状不对（比如 nations 不是数组）也走兜底。
func TestSmsNationsFallbackOnBadPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"nations":"不是数组"}}`)
	}))
	defer srv.Close()

	up := New()
	up.SetAgentHosts(srv.URL, srv.URL)
	if got := up.SmsNations(); len(got) != 4 {
		t.Errorf("形状不对时应用兜底表（4 条），得到 %+v", got)
	}
}
