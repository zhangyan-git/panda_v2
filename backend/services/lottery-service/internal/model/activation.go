package model

import "time"

// Activation 对应 lottery_activations 表：一家门店开通了抽奖。
//
// 开通是一个**动作**（有操作人、有时间），不是从活动推导出来的状态。推导会让门店在
// 两次开奖之间的空档里、或活动被临时停用时显示成「没开通」——抽奖中心会对着一个其实
// 完全正常的状态亮空页（见 migrations/lottery/001 建表注释）。
//
// 所有 UUID 列在这里都是 string：与 payment / account 两个服务的模型一致，读取列一律
// `::text`（pgx 把 uuid 扫进 string 需要这一步，少了它 Scan 会报类型不匹配）。
// 可空的 UUID 用 *string，与「给了但为空」区分开。
type Activation struct {
	ID string `db:"id"`
	// 门店 ID，属于商户服务，本库仅作跨库值引用，不建外键。
	LocationID string `db:"location_id"`
	// 门店名的展示快照，开通时写入。列表页显示旧名字是已知代价，比每次列表多跳一次
	// 跨服务读划算。
	LocationName string `db:"location_name"`
	// enabled / disabled，见下面的常量。
	Status string `db:"status"`
	Remark string `db:"remark"`
	// 操作人（后台管理员）。人工动作要能回答「谁在什么时候开的」，所以这两列不是备注。
	ActivatedBy *string   `db:"activated_by"`
	ActivatedAt time.Time `db:"activated_at"`
	// 与 status 由 CHECK 绑定：disabled ⇔ 非空。停用时间不是一个可以忘的字段。
	DeactivatedAt *time.Time `db:"deactivated_at"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`
}

// 开通状态，与 lottery_activations.status 的 CHECK 逐字一致。
const (
	ActivationEnabled  = "enabled"
	ActivationDisabled = "disabled"
)

// IsEnabled 是「这家店今天能不能进抽奖中心」的唯一判据。
func (a *Activation) IsEnabled() bool { return a.Status == ActivationEnabled }
