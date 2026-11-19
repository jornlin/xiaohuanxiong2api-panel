package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// TasksPath 是新手/邀请任务状态的只读端点（points 域）。
//
// 上游只有这一条任务相关接口，且**只有 GET**：探测 `/tasks` 的 POST/PUT 与
// `/tasks/complete`、`/onboarding/tasks`、`/newbie/tasks` 一类写端点全是 404 ——
// 没有「完成任务」这种客户端动作，积分由服务端按行为发放。所以这里只读，
// 领取得走桌面域/移动域那两条 login/points/grant。
const TasksPath = "/tasks"

// TaskPoints 是官方「赚取积分」页内置的任务分值表（前端静态表，合计 12000）。
// 上游 /tasks 只回 key + status、不带分值，展示要的分值只能在这边补。
var TaskPoints = map[string]int64{
	"register_pack": 3000, // 新人注册礼包（注册即得，服务端发）
	"desktop_login": 3000, // 桌面端礼包（桌面域 login/points/grant）
	"mobile_login":  3000, // 移动端礼包（移动域 login/points/grant）
	"active_7d":     1000, // 活跃 7 天（服务端按行为判定，客户端无入口）
	"first_payment": 2000, // 首次付费（客户端无入口）
}

// TaskState 是一项任务的完成状态。key 见 TaskPoints；status 实测取值
// unfinished / in_progress / completed / expired。
type TaskState struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	// Points 是本地表里的分值（上游响应不带）；key 不在表里时为 0。
	Points int64 `json:"points,omitempty"`
}

// InviteStatus 是邀请好友的进度快照。
type InviteStatus struct {
	Code                    string `json:"code"`
	RegisteredInvitees      int64  `json:"registered_invitees"`
	GrantedPoints           int64  `json:"granted_points"`
	RemainingRewardInvitees int64  `json:"remaining_reward_invitees"`
	RegisterRewardPoints    int64  `json:"register_reward_points"`
	// MarketingText 是上游下发的运营文案（活动/规则变更提示），没有活动时为空串。
	// 前端只做展示，不参与任何判定。
	MarketingText string `json:"marketing_text,omitempty"`
}

// TasksStatus 是一次任务状态查询的结果。
//
// 两组语义不同：NewUser 是新人任务（注册/桌面端/移动端礼包），Invitee 是邀请
// 专属任务（活跃 7 天、首次付费）。**上游只回「当前适用」的项**——实测新号只回
// 前三项，活跃/付费要满足条件才出现，所以调用方要按实际返回渲染，别拿静态表补空。
type TasksStatus struct {
	NewUser []TaskState  `json:"new_user_tasks"`
	Invitee []TaskState  `json:"invitee_tasks"`
	Invite  InviteStatus `json:"invite"`
}

// Tasks 查一次任务状态（只读，零额度消耗）。
func (c *Client) Tasks(a *auth.Auth) (TasksStatus, error) {
	resp, err := c.doJSON(http.MethodGet, c.pointsBase()+TasksPath, nil, c.authHeader(a))
	if err != nil {
		return TasksStatus{}, err
	}
	data, err := payloadOf(resp, "任务状态查询")
	if err != nil {
		return TasksStatus{}, err
	}
	out := TasksStatus{
		NewUser: taskStates(data["new_user_task_states"]),
		Invitee: taskStates(data["invitee_task_states"]),
	}
	if inv, ok := data["invite"].(map[string]any); ok {
		out.Invite = InviteStatus{
			Code:                    text(inv, "invite_code"),
			RegisteredInvitees:      nonNegative(inv["registered_invitee_count"]),
			GrantedPoints:           nonNegative(inv["granted_points"]),
			RemainingRewardInvitees: nonNegative(inv["remaining_reward_invitees"]),
			RegisterRewardPoints:    nonNegative(inv["register_reward_points"]),
			MarketingText:           text(inv, "marketing_text"),
		}
	}
	return out, nil
}

// InviteRecordsPath 是邀请记录分页端点（points 域，GET）。
//
// 官网「赚取积分」页的邀请记录列表就走这条：limit 被前端正则**固定为 20**
// （见 events/tasks 页的请求守卫），所以这里也只按 20 翻页。
const InviteRecordsPath = "/invite-records"

// InviteRecordsLimit 是上游允许的单页条数。
const InviteRecordsLimit = 20

// InviteRecordStage 是一个被邀请好友在某项任务上的进度。
// key 见 InviteStageKeys；status 实测 completed / risk_revoked / not_eligible，
// 其余值前端按「未完成」展示，不透传未知值也不报错。
type InviteRecordStage struct {
	Status string `json:"status"`
	Points int64  `json:"points,omitempty"`
}

// InviteStageKeys 是官网渲染的四个阶段（注意没有 mobile_login：移动端礼包不发邀请人奖励）。
var InviteStageKeys = []string{"register", "desktop_login", "active_7d", "first_payment"}

// InviteRecord 是一条邀请记录（一个被邀请好友）。
//
// 字段来自官网 /events/tasks 的列表渲染（2026-11-19 抓包核对）：
// masked_user（脱敏用户标识）、registered_at（ISO 时间）、active_days（7 日活跃天数）、
// stages（按任务 key 的进度）、granted_points（已发放给邀请人的积分）、
// inviter_reward_eligible（是否计入邀请人奖励名额；超出 20 人上限为 false）。
type InviteRecord struct {
	Type                  string                       `json:"type"`
	Key                   string                       `json:"key"`
	Status                string                       `json:"status"`
	MaskedUser            string                       `json:"masked_user"`
	RegisteredAt          string                       `json:"registered_at"`
	ActiveDays            int64                        `json:"active_days"`
	Stages                map[string]InviteRecordStage `json:"stages,omitempty"`
	GrantedPoints         int64                        `json:"granted_points"`
	InviterRewardEligible bool                         `json:"inviter_reward_eligible"`
}

// InviteRecordsPaging 是分页信息，原样透传给面板前端。
type InviteRecordsPaging struct {
	Offset int64 `json:"offset"`
	Limit  int64 `json:"limit"`
	Total  int64 `json:"total"`
}

// InviteRecordsPage 是一页邀请记录。
type InviteRecordsPage struct {
	Items  []InviteRecord      `json:"items"`
	Paging InviteRecordsPaging `json:"paging"`
}

// InviteRecords 拉一页邀请记录（只读，零额度消耗）。
//
// offset 必须是 InviteRecordsLimit 的整数倍（上游按 offset/limit 翻页）。
func (c *Client) InviteRecords(a *auth.Auth, offset int) (InviteRecordsPage, error) {
	if offset < 0 {
		offset = 0
	}
	q := fmt.Sprintf("?paging.offset=%d&paging.limit=%d", offset, InviteRecordsLimit)
	resp, err := c.doJSON(http.MethodGet, c.pointsBase()+InviteRecordsPath+q, nil, c.authHeader(a))
	if err != nil {
		return InviteRecordsPage{}, err
	}
	data, err := payloadOf(resp, "邀请记录查询")
	if err != nil {
		return InviteRecordsPage{}, err
	}
	out := InviteRecordsPage{Items: []InviteRecord{}}
	if pg, ok := data["paging"].(map[string]any); ok {
		out.Paging = InviteRecordsPaging{
			Offset: nonNegative(pg["offset"]),
			Limit:  nonNegative(pg["limit"]),
			Total:  nonNegative(pg["total"]),
		}
	}
	items, _ := data["items"].([]any)
	for _, raw := range items {
		obj, _ := raw.(map[string]any)
		if obj == nil {
			continue
		}
		rec := InviteRecord{
			Type:                  text(obj, "type"),
			Key:                   text(obj, "key"),
			Status:                text(obj, "status"),
			MaskedUser:            text(obj, "masked_user"),
			RegisteredAt:          text(obj, "registered_at"),
			ActiveDays:            nonNegative(obj["active_days"]),
			GrantedPoints:         nonNegative(obj["granted_points"]),
			InviterRewardEligible: boolOf(obj["inviter_reward_eligible"]),
		}
		if stages, ok := obj["stages"].(map[string]any); ok {
			rec.Stages = map[string]InviteRecordStage{}
			for key, sv := range stages {
				so, _ := sv.(map[string]any)
				rec.Stages[key] = InviteRecordStage{
					Status: text(so, "status"),
					Points: nonNegative(so["points"]),
				}
			}
		}
		out.Items = append(out.Items, rec)
	}
	return out, nil
}

// boolOf 宽容读一个布尔字段：上游有的接口用 true/false，有的用 "true"/1。
func boolOf(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	case json.Number:
		n, _ := t.Int64()
		return n != 0
	}
	return false
}

// nonNegative 读一个展示用的计数：缺字段/非数字（mustInt 回 -1）一律当 0。
// 面板拿到 -1 会原样渲染成「已邀 -1 人」，而上游这条只读接口的字段名不受我们控制，
// 少一个字段不该在界面上变成一个负数。
func nonNegative(raw any) int64 {
	if v := mustInt(raw); v > 0 {
		return v
	}
	return 0
}

// taskStates 把上游的 [{"key":…,"status":…}] 归一成 TaskState 并补上分值。
// 没 key 的项跳过（面板不展示无名任务），缺 status 记 unfinished。
func taskStates(raw any) []TaskState {
	items, _ := raw.([]any)
	out := make([]TaskState, 0, len(items))
	for _, item := range items {
		obj, _ := item.(map[string]any)
		if obj == nil {
			continue
		}
		key := text(obj, "key")
		if key == "" {
			continue
		}
		status := text(obj, "status")
		if status == "" {
			status = "unfinished"
		}
		out = append(out, TaskState{Key: key, Status: status, Points: TaskPoints[key]})
	}
	return out
}
