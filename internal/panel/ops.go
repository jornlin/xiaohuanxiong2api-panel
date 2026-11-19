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
		UID        string `json:"uid"`
		Nickname   string `json:"nickname,omitempty"`
		Phone      string `json:"phone,omitempty"`
		NationCode string `json:"nation_code,omitempty"`
		// InvitedBy 该号注册时绑定的邀请码（空 = 未绑定邀请人，注册后无法补绑）。
		InvitedBy         string `json:"invited_by,omitempty"`
		Code              string `json:"code"`
		RegisteredInvitee int64  `json:"registered_invitees"`
		GrantedPoints     int64  `json:"granted_points"`
		RewardPerInvitee  int64  `json:"register_reward_points"`
		Remaining         int64  `json:"remaining_reward_invitees"`
		// Link 官方邀请链接（与官网「邀请好友」分享同形）。
		Link string `json:"link,omitempty"`
		// MarketingText 上游下发的活动/规则提示（当前无活动时为空），邀请页原样展示。
		MarketingText string `json:"marketing_text,omitempty"`
		Error         string `json:"error,omitempty"`
	}
	// 逐号 /tasks 是 N 次上游请求（号多时很可观）：60 秒内直接复用上一份结果。
	p.invitesMu.Lock()
	if len(p.invitesJSON) > 0 && time.Since(p.invitesAt) < 60*time.Second {
		body := p.invitesJSON
		p.invitesMu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	p.invitesMu.Unlock()

	out := []row{}
	summary := map[string]int64{
		"accounts": 0, "with_code": 0, "invited_total": 0, "granted_total": 0,
		"slots_left": 0, "bound": 0, "unbound": 0,
	}
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if len(out) > 0 {
			time.Sleep(300 * time.Millisecond) // 号与号之间留节奏，避免网关频率风控
		}
		rw := row{
			UID: st.UID, Nickname: st.Nickname,
			Phone: st.Phone, NationCode: st.NationCode, InvitedBy: st.InvitedBy,
		}
		tasks, err := p.cfg.Upstream.Tasks(a)
		if err != nil {
			rw.Error = publicErr(err)
		} else {
			rw.Code = tasks.Invite.Code
			rw.RegisteredInvitee = tasks.Invite.RegisteredInvitees
			rw.GrantedPoints = tasks.Invite.GrantedPoints
			rw.RewardPerInvitee = tasks.Invite.RegisterRewardPoints
			rw.Remaining = tasks.Invite.RemainingRewardInvitees
			rw.MarketingText = tasks.Invite.MarketingText
			if rw.Code != "" {
				// 官方邀请链接：面板此前拼成 /register?invite_code=…，与官网分享链接不一致。
				rw.Link = "https://xiaohuanxiong.com/events/refer?code=" + rw.Code +
					"&utm_source=friend_invitation_action"
			}
		}
		summary["accounts"]++
		if rw.Code != "" {
			summary["with_code"]++
		}
		summary["invited_total"] += rw.RegisteredInvitee
		summary["granted_total"] += rw.GrantedPoints
		if rw.Remaining > 0 {
			summary["slots_left"] += rw.Remaining
		}
		if rw.InvitedBy != "" {
			summary["bound"]++
		} else {
			summary["unbound"]++
		}
		out = append(out, rw)
	}
	resp := map[string]any{"ok": true, "summary": summary, "accounts": out}
	if raw, err := json.Marshal(resp); err == nil {
		p.invitesMu.Lock()
		p.invitesAt, p.invitesJSON = time.Now(), raw
		p.invitesMu.Unlock()
	}
	writeJSON(w, http.StatusOK, resp)
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
		st, err := p.cfg.Upstream.CheckinStatus(a)
		switch {
		case err != nil:
			rw.Error = publicErr(err)
			failed++
		case st.GrantErr != nil:
			// 流水接口不可用（v1 已废弃 403 / 网络抖动）：签到状态未知，
			// 既不算已签也不算未签，别让接口问题渲染成一片「未签到」。
			rw.Error = "积分流水查询失败：" + publicErr(st.GrantErr)
			failed++
		default:
			rw.Checked, rw.Credits, rw.Enable = st.CheckedIn, st.Credits, st.Enable
			if st.CheckedIn {
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

// profilesSync 批量补全手机号：对缺少 phone 的账号逐个查一次 user_info
// （只读、一次请求、号与号之间 1.5s），补到账号文件并落盘。
//
// 授权码/浏览器授权入池的号没有 loginid，面板里只剩「随机昵称 + 7 位 uid」；
// 手机号是唯一能让人认出是哪个号的信息。已有 phone 的号跳过，重复点击不重复打。
func (p *Panel) profilesSync(w http.ResponseWriter, r *http.Request) {
	type row struct {
		UID        string `json:"uid"`
		Phone      string `json:"phone,omitempty"`
		NationCode string `json:"nation_code,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	out := []row{}
	updated := 0
	first := true
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 缺手机号或缺邀请归因的号都要补（两个字段都齐才跳过）。
		phone, _ := a.PhoneValue()
		if phone != "" && a.InvitedByValue() != "" {
			continue
		}
		if !first {
			time.Sleep(1500 * time.Millisecond) // 与余额刷新同节奏，避免网关频率风控
		}
		first = false
		pr, err := p.cfg.Upstream.FetchProfile(a)
		if err != nil {
			out = append(out, row{UID: st.UID, Error: publicErr(err)})
			continue
		}
		if pr.Phone == "" && pr.InvitedBy == "" {
			out = append(out, row{UID: st.UID, Error: "上游未返回手机号/邀请归因"})
			continue
		}
		if pr.Phone != "" {
			a.SetPhone(pr.Phone, pr.NationCode)
		}
		if pr.InvitedBy != "" {
			a.SetInvitedBy(pr.InvitedBy)
		}
		if serr := a.SaveAtomic(); serr != nil {
			out = append(out, row{UID: st.UID, Phone: pr.Phone, Error: "落盘失败：" + serr.Error()})
			continue
		}
		updated++
		out = append(out, row{UID: st.UID, Phone: pr.Phone, NationCode: pr.NationCode})
	}
	log.Printf("panel: profiles sync 补全 %d 个账号手机号（失败 %d）", updated, len(out)-updated)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated, "results": out})
}

// bindInvite 给单个账号补绑邀请码（POST /panel/api/accounts/{uid}/bind_invite）。
//
// 走网页端同一条 /auth/v1/iam/confirm。注意：上游对部分注册来源的号会**静默不生效**
// （返回 success 但归因不落地、邀请人计数不变），所以这里回读 user_info 并如实回报
// attribution_updated，不拿上游的 success 冒充「已绑定」。
func (p *Panel) bindInvite(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		InviteCode string `json:"invite_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	code := strings.TrimSpace(body.InviteCode)
	if len(code) < 8 || len(code) > 64 {
		writeErr(w, http.StatusBadRequest, "邀请码长度不对（8-64 位）")
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	if cur := a.InvitedByValue(); cur != "" {
		writeErr(w, http.StatusConflict, "该账号已绑定邀请人: "+cur)
		return
	}
	if err := p.cfg.Upstream.ConfirmAttribution(a, code); err != nil {
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	updated := false
	invitedBy := ""
	if pr, perr := p.cfg.Upstream.FetchProfile(a); perr == nil {
		invitedBy = pr.InvitedBy
		if pr.InvitedBy != "" {
			a.SetInvitedBy(pr.InvitedBy)
			if serr := a.SaveAtomic(); serr != nil {
				log.Printf("panel: 邀请补绑后落盘失败 uid=%s: %v", uid, serr)
			}
			updated = true
		}
	}
	short := code
	if len(short) > 8 {
		short = short[:8]
	}
	log.Printf("panel: 邀请补绑 uid=%s code=%s… 上游=success 归因落地=%v", uid, short, updated)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "upstream": "success", "attribution_updated": updated,
		"invited_by": invitedBy,
	})
}
