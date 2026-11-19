// ops.go 账号运维端点：单号刷 token、凭证脱敏预览、批量导入、签到状态。
//
// 凭证一律走 upstream.PrepareAccount（PAT 兑换 / 凭证解析 → userinfo → 落盘 → 入池），
// 不在面板里另造一套换票逻辑。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/scheduler"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// maskSecret 只回前缀 + 长度，绝不回完整 token（与 overview 同口径）。
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "«" + strconv.Itoa(len(s)) + " 字符»"
	}
	return s[:8] + "…«" + strconv.Itoa(len(s)) + " 字符»"
}

// accountRefreshToken 单号刷 token（只动这一个号，省掉整池保活）。
func (p *Panel) accountRefreshToken(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if err := p.cfg.Upstream.RefreshToken(a); err != nil {
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	// 刷新成功即落盘（不落盘 = 进程重启后带着旧 refreshToken 回来，被判 session 失效）。
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "换到新票但落盘失败")
		return
	}
	p.cfg.Pool.ClearSessionDead(uid) // 新票在手，session 失效计数清零（同调度器口径）
	st, _ := p.cfg.Pool.Status(uid)
	log.Printf("panel: refresh token uid=%s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st, "expires_at": a.ExpiresAtValue()})
}

// accountCredential 盘上凭证的脱敏预览（排障用：这份文件到底是谁、什么时候过期）。
func (p *Panel) accountCredential(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	path, err := authPath(p.cfg.AuthDir, uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad uid")
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "凭证文件不存在")
		return
	}
	a, err := auth.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "凭证文件解析失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": a.UID, "nickname": a.Nickname, "realm": a.Realm(),
		"file":          path,
		"expires_at":    a.ExpiresAtValue(),
		"access_token":  maskSecret(a.JWT()),
		"refresh_token": maskSecret(a.RefreshTokenValue()),
		"machine_id":    a.MachineIDValue(),
	})
}

// credentialOnDiskNewer 盘中该 uid 已有一份**更新且未过期**的凭证吗。
// 旧备份覆盖刚登录的新会话 = 真实事故（PR #183 accounts.go:139-155 同一条）：旧 token 顶掉
// 新票 → session dead。新件到期时间未知/已过期时无从比较，不拦。
func credentialOnDiskNewer(dir, uid string, newExpire int64) bool {
	if newExpire <= time.Now().Unix() {
		return false
	}
	path, err := authPath(dir, uid)
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	a, err := auth.Parse(raw)
	if err != nil {
		return false
	}
	old := a.ExpiresAtValue()
	return old > newExpire && old > time.Now().Unix()
}

// accountImport 批量导入账号：body 是「账号 JSON」或裸的 PAT 文本。
// 与「添加账号」共用 upstream.PrepareAccount → saveAccount 这条管线。
func (p *Panel) accountImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	trim := strings.TrimSpace(string(raw))
	if trim == "" {
		writeErr(w, http.StatusBadRequest, "请求体为空")
		return
	}
	payload := map[string]any{}
	lower := strings.ToLower(trim)
	switch {
	case strings.HasPrefix(trim, "{"):
		if err := json.Unmarshal([]byte(trim), &payload); err != nil {
			writeErr(w, http.StatusBadRequest, "账号 JSON 解析失败："+err.Error())
			return
		}
	case strings.HasPrefix(lower, "pt-"):
		payload["pat"] = trim
	case strings.HasPrefix(lower, "office-raccoon://"):
		payload["callback"] = trim
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		// 环回回调链接（http://127.0.0.1:<port>/auth/callback?code=…）。面板部署在服务器/
		// 局域网时浏览器打不开这条地址，但把整条链接粘进来照样能换票。
		if !strings.Contains(lower, "/auth/callback") {
			writeErr(w, http.StatusBadRequest, "http(s) 链接只认 …/auth/callback?code=… 这种回调地址")
			return
		}
		payload["callback"] = trim
	case strings.HasPrefix(lower, "ac_"):
		payload["code"] = trim
	case strings.HasPrefix(trim, "eyJ"):
		payload["access_token"] = trim
	default:
		writeErr(w, http.StatusBadRequest,
			"无法识别的内容：请粘贴小浣熊账号 JSON（access_token / refresh_token），或 office-raccoon:// 授权回调链接")
		return
	}
	creds, err := p.cfg.Upstream.PrepareAccount(r.Context(), payload, newMachineID())
	if err != nil {
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	// 已知 uid 才做「旧备份不覆盖」预检；未知时交给上游确认。
	if creds.UID != "" && credentialOnDiskNewer(p.cfg.AuthDir, creds.UID, creds.ExpiresAtValue()/1000) {
		log.Printf("panel: import skipped uid=%s（盘中凭证更新）", creds.UID)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "action": "skipped_existing_newer", "uid": creds.UID,
			"message": "盘中已有更新的凭证，未覆盖",
		})
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "imported", "uid": uid, "nickname": nick})
}

// grantsAll 全池领一次性新手礼包（桌面端 + 移动端，各 3000 分）。
// 强制问上游：进程内的「已问过」标记只是省一次幂等请求，不该拦下显式点击。
func (p *Panel) grantsAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	grants, err := p.cfg.Scheduler.RunNewcomerNow()
	if errors.Is(err, scheduler.ErrRoundBusy) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	log.Printf("panel: newcomer grants（全池），本次问上游 %d 笔", len(grants))
	p.writeOverview(w, map[string]any{"grants": grants})
}

// accountGrants 单号领一次性新手礼包。
func (p *Panel) accountGrants(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	grants, err := p.cfg.Scheduler.ClaimNewcomerNow(uid)
	switch {
	case errors.Is(err, scheduler.ErrDisabled):
		writeErr(w, http.StatusConflict, "account disabled")
		return
	case errors.Is(err, scheduler.ErrNotFound):
		writeErr(w, http.StatusNotFound, "account not found")
		return
	case errors.Is(err, scheduler.ErrRoundBusy):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		// 与 accountCheckin 同口径：将来多出别的错误类型时，别静默当成成功回 200。
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "grants": grants, "account": st})
}

// invites 池里各账号的邀请码（只读）：面板「添加账号 → 手机验证码」用它拼注册链接。
//
// 邀请奖励记在**邀请人**账上（1500 分/位），所以链接里的码必须来自池里的账号 ——
// 直接把池子里的码列出来，用户就不用去官网后台翻。顺带回报已邀人数与已得积分，
// 注册完立刻能看出这次邀请算没算上（上游没有补绑接口，只在注册那一刻生效）。
func (p *Panel) invites(w http.ResponseWriter, r *http.Request) {
	type row struct {
		UID               string `json:"uid"`
		Nickname          string `json:"nickname,omitempty"`
		Code              string `json:"code"`
		RegisteredInvitee int64  `json:"registered_invitees"`
		GrantedPoints     int64  `json:"granted_points"`
		RewardPerInvitee  int64  `json:"register_reward_points"`
		// MarketingText 上游下发的活动/规则提示（当前无活动时为空），邀请页原样展示。
		MarketingText string `json:"marketing_text,omitempty"`
		Error         string `json:"error,omitempty"`
	}
	out := []row{}
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		rw := row{UID: st.UID, Nickname: st.Nickname}
		tasks, err := p.cfg.Upstream.Tasks(a)
		if err != nil {
			rw.Error = publicErr(err)
		} else {
			rw.Code = tasks.Invite.Code
			rw.RegisteredInvitee = tasks.Invite.RegisteredInvitees
			rw.GrantedPoints = tasks.Invite.GrantedPoints
			rw.RewardPerInvitee = tasks.Invite.RegisterRewardPoints
			rw.MarketingText = tasks.Invite.MarketingText
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// inviteRecords 邀请记录（**只读**，零额度消耗）：GET /panel/api/invite_records?uid=&offset=
//
// 官网「赚取积分」页的邀请记录列表同款端点（/points/v1/invite-records，20 条/页）。
// 面板只做展示：被邀请好友的脱敏标识、注册时间、7 日活跃天数、四项任务的到账进度、
// 已发放给邀请人的积分、是否计入 20 人奖励名额。奖品归属与判定全在上游。
func (p *Panel) inviteRecords(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "需要 uid（邀请人账号）")
		return
	}
	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "offset 不是合法数字")
			return
		}
		offset = n
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	page, err := p.cfg.Upstream.InviteRecords(a, offset)
	if err != nil {
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	nickname := ""
	for _, st := range p.cfg.Pool.List() {
		if st.UID == uid {
			nickname = st.Nickname
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "nickname": nickname,
		"items": page.Items, "paging": page.Paging,
	})
}

// tasksStatus 任务状态（**只读**，零额度消耗）：透传上游 /points/v1/tasks。
//
// 面板据此显示「新手任务领了几项」与邀请码——只靠余额猜「礼包到底领没领到」
// 是这次踩坑的起点（移动端那 3000 一直没领，账面上看只是一个对不上的数字）。
func (p *Panel) tasksStatus(w http.ResponseWriter, r *http.Request) {
	type row struct {
		UID      string                `json:"uid"`
		Nickname string                `json:"nickname,omitempty"`
		NewUser  []upstream.TaskState  `json:"new_user_tasks"`
		Invitee  []upstream.TaskState  `json:"invitee_tasks"`
		Invite   upstream.InviteStatus `json:"invite"`
		Error    string                `json:"error,omitempty"`
	}
	out := []row{}
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		rw := row{UID: st.UID, Nickname: st.Nickname}
		tasks, err := p.cfg.Upstream.Tasks(a)
		if err != nil {
			rw.Error = publicErr(err)
		} else {
			rw.NewUser, rw.Invitee, rw.Invite = tasks.NewUser, tasks.Invitee, tasks.Invite
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// checkinStatus 批量签到状态（**只读**，只查 status 不 claim）：面板点「签到状态」时查一次。
func (p *Panel) checkinStatus(w http.ResponseWriter, r *http.Request) {
	type row struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname,omitempty"`
		Realm    string `json:"realm,omitempty"`
		Checked  bool   `json:"checked_in"`
		Credits  int64  `json:"credits,omitempty"`
		Enable   bool   `json:"enable"`
		Skipped  string `json:"skipped,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	out := []row{}
	checked, unchecked, failed := 0, 0, 0
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		rw := row{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm}
		ci, credits, enable, err := p.cfg.Upstream.CheckinStatus(a)
		switch {
		case err != nil:
			rw.Error = publicErr(err)
			failed++
		default:
			rw.Checked, rw.Credits, rw.Enable = ci, credits, enable
			if ci {
				checked++
			} else {
				unchecked++
			}
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "checked": checked, "unchecked": unchecked, "failed": failed, "accounts": out,
	})
}
