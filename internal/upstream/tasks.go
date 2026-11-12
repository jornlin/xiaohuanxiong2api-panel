package upstream

import (
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
		}
	}
	return out, nil
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
