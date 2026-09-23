package model

import (
	"fmt"
	"strings"
	"time"
)

// Campaign 对应 lottery_campaigns 表：一个抽奖活动。
//
// 活动的**粒度**不用账号范围那种多态指针（一个档位列 + 一组目标引用），而是「门店来自开通记录 + 一个可空的
// MachineID」：为空是门店级，非空是本店某台咖啡机级。两列写法要靠服务层自觉才能保证
// 「这台设备属于这个门店」，而这里结构上就写不出来——活动挂在哪家店完全由 ActivationID
// 决定，没有第二个地方可以填错（见 migrations/lottery）。
type Campaign struct {
	ID string `db:"id"`
	// 开通记录，同时决定了这个活动属于哪家门店。
	ActivationID string `db:"activation_id"`
	// 咖啡机 ID，属于咖啡机服务，本库仅作跨库值引用；NULL = 整个门店。
	MachineID *string `db:"machine_id"`
	// 期次号的前缀（round_no = '{code}-{seq:04d}'），也是后台认这个活动的短名。
	Code string `db:"code"`
	Name string `db:"name"`
	// 开期时把 ParticipantTarget 冻结到期次上，之后改活动不会回头改一期正在跑的奖。
	ParticipantTarget int32  `db:"participant_target"`
	Description       string `db:"description"`
	// 开通抽奖时按内置模板自动建的那一个。一个开通记录只能有一个
	// （lottery_campaigns_one_default_per_activation 部分唯一索引）。
	IsDefault bool `db:"is_default"`
	// draft / enabled / paused / ended，见下面的常量。
	Status    string    `db:"status"`
	CreatedBy *string   `db:"created_by"`
	UpdatedBy *string   `db:"updated_by"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// 活动状态，与 lottery_campaigns.status 的 CHECK 逐字一致。
const (
	// CampaignDraft 是新建但未启用：不开期，抽奖中心也看不到。
	CampaignDraft = "draft"
	// CampaignEnabled 是启用中：可以开期、可以参与。
	CampaignEnabled = "enabled"
	// CampaignPaused 是暂停：不参与新的期次滚动，但**正在跑的那一期照跑到开奖**。
	// 「暂停」停的是下一期，不是这一期——已经收了 N 个人的参与，不能因为运营点了暂停
	// 就把他们手里的卡吞掉。
	CampaignPaused = "paused"
	// CampaignEnded 是终态：**只能被人为结束**（活动不再有时间窗口，所以没有自动结束
	// 这条路了），不再开期。
	CampaignEnded = "ended"
)

// IsMachineScoped 是「这个活动只管一台设备」。
//
// 抽奖中心把它显示成「XX 咖啡机专用」，门店级活动显示在这个列表的上面。
func (c *Campaign) IsMachineScoped() bool { return c.MachineID != nil && *c.MachineID != "" }

// CampaignCodePattern 是活动短名的形状，与 lottery_campaigns.code 的 CHECK 一致
// （^[A-Z0-9]{1,16}$）。放在模型里是为了让服务层能在写库之前就给出人话的报错，
// 而不是把一条 23514 翻成 500。
func ValidCampaignCode(code string) bool {
	code = strings.TrimSpace(code)
	if len(code) == 0 || len(code) > 16 {
		return false
	}
	for _, r := range code {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// RoundNo 是这一期的对外单号：{code}-{seq:04d}。
//
// 用活动短名 + 四位序号而不是 UUID：抽奖中心上要显示「LAKE-202608-12 期」，
// 客服在电话里要能把它念出来。seq 在活动内唯一（lottery_rounds_seq_unique），
// 拼出来的 round_no 全局唯一（lottery_rounds_no_unique）。
func RoundNo(code string, seq int32) string {
	return fmt.Sprintf("%s-%04d", code, seq)
}
