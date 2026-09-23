package model

import "time"

// 变更类型。与 migrations/membership 里 membership_changes.change_type 的 CHECK
// 逐字一致——这不是重复定义，是同一个取值表的 Go 半边。（后两个 charge_failed / suspend
// 是代扣专用的，前十二个建表时就在。）
//
// auto_renew_on / off 记的是那个开关**被拨动**这件事，而拨它的人不一定是用户：签约生效时由
// 服务端跟着拨开（同一件事的两半），解约时跟着拨灭（见 repository.applyAutoRenew）。所以这两
// 行上的 operator_type 才是「谁拨的」，而行本身说的是同一件事：这个人下个月还会不会被扣。
const (
	ChangeActivate     = "activate"       // 开通（首购生效）
	ChangeRenew        = "renew"          // 续费（有效期叠加）；**代扣成功也写这个**
	ChangeExpire       = "expire"         // 到期失效
	ChangeFreeze       = "freeze"         // 权益暂停
	ChangeUnfreeze     = "unfreeze"       // 暂停恢复
	ChangeAutoRenewOn  = "auto_renew_on"  // 打开自动续费
	ChangeAutoRenewOff = "auto_renew_off" // 关闭自动续费
	ChangeSubscribe    = "subscribe"      // 签约连续包月
	ChangeUnsubscribe  = "unsubscribe"    // 解约
	ChangeRefundAdjust = "refund_adjust"  // 退款后按规则调整权益
	ChangeAdminAdjust  = "admin_adjust"   // 后台人工调整
	ChangeRevoke       = "revoke"         // 撤销会员
	ChangeChargeFailed = "charge_failed"  // 一期代扣没扣到（这一期还没成，到期日不动）
	ChangeSuspend      = "suspend"        // 连续失败到上限，停掉自动续费；协议还在，不是解约
)

// 操作人类型。system 是本服务的自动动作（到期扫描、事件消费），worker 是定时任务，
// 两者分开是为了排查时能一眼看出「这是被扫出来的还是被事件推出来的」。
const (
	OperatorUser   = "user"
	OperatorAdmin  = "admin"
	OperatorSystem = "system"
	OperatorWorker = "worker"
)

// Change 是会员身上发生过的一件事。
//
// 只增不改不删（membership_changes_append_only 触发器物理阻断，与 stock_movements 同一
// 套写法）。这张表同时承担三个用途：
//
//   - 小程序与后台的会员详情页时间线；
//   - 退款、冻结这类人工操作的留痕；
//   - 「这一单的会员权益到底改没改」的幂等凭据——membership_changes_order_unique
//     保证同一个订单的同一种变更只记一次，支付成功回调重放不会把会员续两次。
//
// 最后一条是这张表不能省的原因：没有它，事件重放就只能靠服务层自觉，而重放迟早会发生。
type Change struct {
	ID           string `db:"id"`
	MembershipID string `db:"membership_id"`
	UserID       string `db:"user_id"`
	ChangeType   string `db:"change_type"`
	// FromStatus / ToStatus 与下面那对 expire 快照是同一件事的两个维度：一个是资格状态
	// 怎么变，一个是有效期怎么变。续费只动后者（状态还是 active），冻结只动前者。
	//
	// 指针是因为两端的列都可空，而**开通那一次的 FromStatus 必须是 NULL**：它表示「之前没有
	// 这条会员」，与「有一个叫空字符串的状态」是两回事，后者会让时间线上多出一个说不清的
	// 起点。
	FromStatus *string `db:"from_status"`
	ToStatus   *string `db:"to_status"`
	// FromExpireAt / ToExpireAt 是续费叠加前后的有效期。会员的期次历史就靠这一对还原
	// ——memberships 只存当前值，续费不新建行，所以「上一期到什么时候」只在这里。
	FromExpireAt *time.Time `db:"from_expire_at"`
	ToExpireAt   *time.Time `db:"to_expire_at"`
	// PlanID / OrderID 都可能为空：后台人工调整没有套餐，到期扫描没有订单。
	PlanID  *string `db:"plan_id"`
	OrderID *string `db:"order_id"`
	// OperatorID 是操作人 ID（用户或后台账号）。system 与 worker 触发的留空。
	OperatorType string  `db:"operator_type"`
	OperatorID   *string `db:"operator_id"`
	Reason       string  `db:"reason"`
	// Remark 是后台人工填的备注，Reason 是系统给的短因由（比如 "payment_failed"）。
	// 分开是因为后台列表要把人工备注单独展示，混在一起就分不出哪句是谁写的。
	Remark string `db:"remark"`
	// Metadata 放这一笔的补充事实（原订单号、失败原因码、代扣协议号……），只写不读逻辑，
	// 排查时打开看。
	Metadata []byte `db:"metadata"`
	// RequestID 是操作留痕串，与后台操作审计（方案 11）对得上。
	RequestID  string    `db:"request_id"`
	OccurredAt time.Time `db:"occurred_at"`
	CreatedAt  time.Time `db:"created_at"`
}
