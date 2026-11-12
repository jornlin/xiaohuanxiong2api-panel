package upstream

import (
	"net/http"
	"strings"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// 短信验证码登录（官方网页端同款通道）。
//
// 端点、字段名、字段加密都取自 raccoon-lab/office 的登录 chunk（2026-11-12 实测打通）：
//
//	GET  {auth_base}/option_sms_nations                 → {data:{nations:[{name,code}]}}
//	POST {auth_base}/send_sms        {nation_code, phone, captcha_param}
//	POST {auth_base}/login_with_sms  {nation_code, phone, sms_code, [utm_source, inviter_user_id, registration_attribution]}
//
// phone（以及 login_with_password 的 password）必须是 EncryptSmsField 的密文，
// 明文会被上游拒（`params_encryted_error` / `param phone encrypted error`）。
//
// 两条端点的验证码要求不同，这是实测结论，别互相推断：
//   - login_with_sms **不需要**图形验证码：密文手机号 + 随便一个验证码回的是
//     `sms_code_verify_error`（100007），说明请求已被正常处理。
//   - send_sms 的 captcha_param 是官方网页端用阿里云滑块换来的回执，能不能省
//     取决于上游策略；本仓按「能省就省、省不掉就把上游原话报出来」处理。
//
// utm_source / inviter_user_id / registration_attribution 是官方页的投放归因字段：
// 实测只带 nation_code / phone / sms_code 就能走到验码那一步，所以默认不发。
const (
	SmsNationsPath = "/option_sms_nations"
	SendSmsPath    = "/send_sms"
	LoginSmsPath   = "/login_with_sms"
)

// 短信通道的业务码（实测）。
const (
	CodeParamsInvalid   = 100002 // 参数不合法（手机号格式等）
	CodeParamsEncrypted = 100003 // 字段该加密却没加密 / 加密方式不对
	CodeSmsCodeVerify   = 100007 // 验证码不对或已过期
)

// SmsNation 是一个可选的短信区号。
type SmsNation struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

// SmsNations 拉一次可选区号（公开端点，不需要登录态）。
// 失败时返回内置兜底表：区号列表不是关键路径，不该因为一次网络抖动让整个登录页不可用。
func (c *Client) SmsNations() []SmsNation {
	resp, err := c.doJSON(http.MethodGet, c.authBase()+SmsNationsPath, nil, c.clientIDHeaders())
	if err != nil {
		return defaultSmsNations()
	}
	data, err := payloadOf(resp, "区号查询")
	if err != nil {
		return defaultSmsNations()
	}
	items, _ := data["nations"].([]any)
	out := make([]SmsNation, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		code := text(item, "code")
		if code == "" {
			continue
		}
		out = append(out, SmsNation{Name: orDefault(text(item, "name"), code), Code: code})
	}
	if len(out) == 0 {
		return defaultSmsNations()
	}
	return out
}

// defaultSmsNations 是实测的四个区号（上游 /option_sms_nations 当前就回这几个）。
func defaultSmsNations() []SmsNation {
	return []SmsNation{
		{Name: "中国大陆", Code: "86"},
		{Name: "香港", Code: "852"},
		{Name: "澳门", Code: "853"},
		{Name: "日本", Code: "81"},
	}
}

// SendSmsCode 给手机号发登录验证码。captchaParam 为空表示不带滑块回执。
func (c *Client) SendSmsCode(nation, phone, captchaParam string) error {
	enc, err := EncryptSmsField(phone)
	if err != nil {
		return err
	}
	body := map[string]any{"nation_code": nation, "phone": enc}
	if captchaParam != "" {
		body["captcha_param"] = captchaParam
	}
	resp, err := c.doJSON(http.MethodPost, c.authBase()+SendSmsPath, body, c.clientIDHeaders())
	if err != nil {
		return err
	}
	if _, err := payloadOf(resp, "发送验证码"); err != nil {
		return smsFriendly(resp, err)
	}
	return nil
}

// LoginWithSms 用手机号 + 验证码登录，返回可直接入池的凭证。
//
// uid 以 user_info 为准（与 exchangeCode 同口径）：登录响应里的 user_id 可能是
// 平台 id（pu-…，iam 域），直接用会让同一个号在池子里出现两行。
func (c *Client) LoginWithSms(nation, phone, code string) (*auth.Auth, error) {
	enc, err := EncryptSmsField(phone)
	if err != nil {
		return nil, err
	}
	// sms_code 是明文（官方页就是这么发的，实测也走到了验码那一步）。
	body := map[string]any{"nation_code": nation, "phone": enc, "sms_code": code}
	resp, err := c.doJSON(http.MethodPost, c.authBase()+LoginSmsPath, body, c.clientIDHeaders())
	if err != nil {
		return nil, err
	}
	data, err := payloadOf(resp, "验证码登录")
	if err != nil {
		return nil, smsFriendly(resp, err)
	}
	access := text(data, "access_token", "accessToken")
	refresh := text(data, "refresh_token", "refreshToken")
	if access == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊验证码登录未返回 access_token"}
	}
	account := &auth.Auth{Data: auth.Data{
		AccessToken:  stripBearerToken(access),
		RefreshToken: stripBearerToken(refresh),
		UID:          text(data, "user_id", "uid", "userId"),
		Nickname:     text(data, "office_org_name", "office_identity", "nickname"),
		ExpiresAt:    auth.JWTExpiry(access),
	}}
	if uid, nick, _, err := c.GetUserInfo(account); err == nil {
		if uid != "" {
			account.UID = uid
		}
		if nick != "" {
			account.Nickname = nick
		}
	}
	if account.UID == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊验证码登录未返回用户标识"}
	}
	if account.Nickname == "" {
		account.Nickname = account.UID
	}
	return account, nil
}

// smsFriendly 把短信通道的失败翻成人话再往上抛（BusinessError.Error() 就是 Message，
// 面板原样展示）。
//
// 为什么按**响应体**里的业务码翻、而不是看 payloadOf 返回的错误类型：上游这些失败是
// HTTP 400 + 业务码（不是 2xx 带非零 code），payloadOf 在状态码那一步就把它们归成通用
// *Error 了，等拿到错误再判类型已经晚了——实测漏翻过一次，面板上只剩英文原话。
// 业务码认不出来时保留 payloadOf 的原错误（含 HTTP 状态与上游原话）。
func smsFriendly(resp *apiResponse, err error) error {
	raw := businessMessageOf(resp.Body)
	code, hasCode := businessCodeOf(resp.Body)

	// 人机校验（阿里云滑块）单独说清楚：发码这一步上游要 captcha_param，而回执只有
	// 官方页面里的滑块能拿到（面板是内网地址，不在那个 SceneId 的域白名单里）。
	// 这条失败**没有**已收录的业务码（官方 web 端也只把它归成一句 captchaVerifyFailed），
	// 所以按 message 文本判——它是稳定的英文标识。
	if strings.Contains(raw, "captcha") || strings.Contains(businessCodeText(resp.Body), "captcha") {
		return &BusinessError{Code: code, Message: "上游要求人机校验（滑块），面板这边拿不到回执；" +
			"请到官方页面点「验证码登录」取码（xiaohuanxiong.com/account 或官方客户端），" +
			"再把手机号 + 验证码填到这里登录（登录这条不需要滑块）"}
	}

	if !hasCode {
		return err
	}
	msg, known := map[int]string{
		CodeSmsCodeVerify:   "验证码不正确或已过期，请重新获取",
		CodeParamsInvalid:   "手机号不合法，检查区号与号码是否对得上",
		CodeParamsEncrypted: "上游拒绝了加密字段，加密方式与官方不一致",
	}[code]
	if !known {
		return err
	}
	if raw != "" {
		msg += "（上游：" + raw + "）"
	}
	return &BusinessError{Code: code, Message: msg}
}
