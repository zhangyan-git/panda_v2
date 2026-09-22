package dto

import "time"

// MembershipResponse 是一条会员资格的对外形状。
//
// 它同时服务后台详情页、后台列表、小程序会员中心三处。三处要看的字段有重叠也有差别（后台要
// 看冻结原因与操作留痕，小程序不看），但**形状是同一个**：分两套结构会让「后台显示还在有效期、
// 小程序显示已过期」这种不一致有地方发生，而它正是最不该有的那类 bug。
//
// 列表与详情也因此同形，区别只在填了哪些部分（详情多一个 Changes）。
type MembershipResponse struct {
	ID     string `json:"id"`
	UserID string `json:"userId"`
	// PlanCode / PlanName 是**成交当时的快照**，不是现查套餐。套餐后来改了名，这里不跟着变
	// ——用户当初买的就是那个名字。
	PlanCode string `json:"planCode"`
	PlanName string `json:"planName"`
	// MemberPriceMode 同样是成交快照：auto=会员本人自动享会员价，coupon=靠会员价体验券。
	// 它是「这个人现在怎么才能享到会员价」的唯一答案，前端据此决定说哪句话。
	MemberPriceMode string `json:"memberPriceMode"`
	// StoreID / StoreName 是**归属门店**：这个人是谁拉来的。
	//
	// 它不参与权益判定、不影响核销、不参与分账——后台把它展示在用户详情与会员详情上，仅此
	// 而已。StoreName 是**商户域的事实**，本库不留第二份：每次读一页由服务层现解一次
	// （见 service.StoreNames），解不出来时是空串而不报错。
	StoreID   string `json:"storeId"`
	StoreName string `json:"storeName"`
	// active / frozen / expired / revoked。
	Status   string    `json:"status"`
	StartAt  time.Time `json:"startAt"`
	ExpireAt time.Time `json:"expireAt"`
	// Active 是**算出来给前端用的**：这个会员此刻到底还作不作数（status=active 且在有效期内）。
	//
	// 它不进库、不改任何东西，纯粹是把「status 与 expire_at 一起看」这条判断从三处前端里
	// 收回来一处。到期扫描没有跑完的那一小段时间里，status 还是 active 而 expire_at 已经过了
	// ——只看 status 的前端会在那一小段时间里显示「会员有效」。
	Active bool `json:"active"`
	// AutoRenew 是用户可关的那个开关。套餐支不支持自动续费看 PlanResponse.AutoRenew，
	// 两者不是一回事：套餐说「这个产品可以签代扣」，这里说「这个人现在开着」。
	AutoRenew      bool       `json:"autoRenew"`
	AutoRenewOffAt *time.Time `json:"autoRenewOffAt"`
	RenewalCount   int32      `json:"renewalCount"`
	LastRenewedAt  *time.Time `json:"lastRenewedAt"`
	// 冻结与撤销的留痕。Reason 是系统给的短因由，Remark 是后台人工填的备注。
	FrozenAt     *time.Time `json:"frozenAt"`
	FreezeReason string     `json:"freezeReason"`
	RevokedAt    *time.Time `json:"revokedAt"`
	RevokeReason string     `json:"revokeReason"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	// Changes 是时间线，**只有详情填**。列表页空数组：一页 20 条各自的完整流水进不了列表，
	// 硬塞进去只会让列表接口变成一个没人敢改的重查询。
	Changes []ChangeResponse `json:"changes"`
}

// ChangeResponse 是会员身上发生过的一件事。
type ChangeResponse struct {
	ID string `json:"id"`
	// 取值见 model.Change*：activate / renew / expire / freeze / unfreeze / auto_renew_on /
	// auto_renew_off / subscribe / unsubscribe / refund_adjust / admin_adjust / revoke。
	//
	// 回的是**编码**而不是中文：文案归前端（它有标签表），而且同一件事在不同页面上的说法
	// 可以不一样（「开通」在时间线上、在筛选下拉里都合适，在按钮上就不合适）。
	ChangeType string `json:"changeType"`
	// From / To 两对，可能为空：续费只动有效期（状态两个都是 active），冻结只动状态。
	FromStatus   string     `json:"fromStatus"`
	ToStatus     string     `json:"toStatus"`
	FromExpireAt *time.Time `json:"fromExpireAt"`
	ToExpireAt   *time.Time `json:"toExpireAt"`
	// OrderID 为空是常态里的常态：到期扫描、后台调整都没有订单。
	OrderID *string `json:"orderId"`
	// user / admin / system / worker。
	OperatorType string    `json:"operatorType"`
	OperatorID   *string   `json:"operatorId"`
	Reason       string    `json:"reason"`
	Remark       string    `json:"remark"`
	OccurredAt   time.Time `json:"occurredAt"`
}

// MiniappMembershipResponse 是小程序「我的会员」那一屏的响应。
//
// 包一层而不是直接回 MembershipResponse，是为了让**「还不是会员」也是一个 200**：
// 绝大多数用户打开会员中心时都不是会员，把它做成 404 会让前端每一次正常访问都走错误分支，
// 而错误分支里多半会弹一个「加载失败」。包一层之后，null 是明确的、可判定的答案。
//
// 它**不复用后台的 MembershipResponse**：后台那个带着冻结原因、撤销原因、操作留痕——那些是
// 内部管理信息，不该出现在用户的手机上（与 dto.MiniappPlanResponse 同一条理由）。
type MiniappMembershipResponse struct {
	Membership *MiniappMembership `json:"membership"`
}

// MiniappMembership 是用户自己看到的那份会员。
//
// 与后台形状的差别不只是字段多少：这里没有 user_id（用户当然知道是自己），没有冻结与撤销的
// 原因（那是运营写给自己人看的），也没有时间线（那是后台对账用的）。
type MiniappMembership struct {
	ID       string `json:"id"`
	PlanName string `json:"planName"`
	// MemberPriceMode 决定这一屏上说哪句话：「你已享会员价」还是「你有 N 张会员价券」。
	MemberPriceMode string    `json:"memberPriceMode"`
	Status          string    `json:"status"`
	StartAt         time.Time `json:"startAt"`
	ExpireAt        time.Time `json:"expireAt"`
	// Active 是算出来的（status=active 且在有效期内），与后台那个同名。
	Active    bool `json:"active"`
	AutoRenew bool `json:"autoRenew"`
	// MemberPriceCouponsPerPeriod 是**成交快照**上「每期发几张」，coupon 模式下用来显示
	// 「本月可享 20 次会员价」。它不是余额——券在 coupon-service，余额要在那里查。
	//
	// **没有「这个套餐支不支持自动续费」这个字段**：memberships 上没有它，而现查套餐（按
	// plan_id）与「成交快照说了算」这条本域反复申明的规矩直接冲突——套餐今天关掉自动续费，
	// 不等于这个人当初买的那个产品不支持。凡是需要它的地方（购买页、续费入口）本来就有套餐
	// 列表，那里是它的家。
	MemberPriceCouponsPerPeriod int32 `json:"memberPriceCouponsPerPeriod"`
}

// FreezeRequest 是冻结会员的请求体。
//
// Reason 必填：冻结是**收走一个人已经在享的权益**（争议、风控），事后只有这一句话答得上来
// 「为什么这个人三月中旬突然不是会员了」。与入库作废必填原因同一条理由。
type FreezeRequest struct {
	Reason string `json:"reason"`
}

// RevokeRequest 是撤销会员的请求体。
//
// 与冻结分开：**撤销不可逆**（status 回不去，库上的状态机也只允许从 active/frozen 到
// revoked），而冻结是可以解冻的。合用一个接口会让一次手滑变成一次永久撤销。
type RevokeRequest struct {
	Reason string `json:"reason"`
}

// AdjustRequest 是后台直接改会员有效期的请求体。
//
// 这是本域破坏力最大的一个接口：它**没有任何订单、支付或流水跟着发生**，只是把一个人的
// expire_at 挪一下。所以 Reason 必填、备注可选但建议填，且每一次调用都写平台审计（方案
// §11.6「影响用户资产归属的人工操作」）。
//
// 用 ExpireAt 而不是「延长 N 天」：后台要处理的是「这个人应该在 2027-01-01 到期」这类具体
// 结论（对账、客服工单里给的就是一个日期），而「延长 N 天」要求操作的人自己先算一遍，算错的
// 那次没有任何东西能发现。延长语义由前端算完再发过来。
type AdjustRequest struct {
	ExpireAt time.Time `json:"expireAt"`
	Reason   string    `json:"reason"`
	Remark   string    `json:"remark"`
}

// GrantRequest 是后台**直接开通一个会员**的请求体。
//
// 它没有订单号，也没有金额：这是客服补偿、线下活动、渠道争议的出口，钱不在系统里过。它与
// AdjustRequest 一样是本域破坏力最大的接口之一——区别只在于一个是白送一段权益，一个是把已有的
// 那段往后挪。
type GrantRequest struct {
	UserID string `json:"userId"`
	PlanID string `json:"planId"`
	// ExpireAt 留空时按套餐自带时长算（period × periodCount）。给了就用给的：补偿场景常是
	// 「送到年底」这类具体结论，那不是「延长 N 天」能表达的。
	//
	// 与 dto.AdjustRequest 上那个字段同一条写法：类型是 time.Time，所以**没有一条「时间格式
	// 不对」的错误**——写不成时间的值在 controller 解请求体时就已经失败了。
	ExpireAt time.Time `json:"expireAt"`
	// StoreID 是**归属门店**，可留空（见 model.Membership.StoreID）。
	//
	// 它不参与任何权益判定与金额计算，只是「这个人是谁拉来的」这条留痕。后台能改的是**新开通**
	// 那一次的归属；已有会员一律拒绝开通，所以后台也就改不了老会员的归属——「会员过期后重新
	// 开通可变归属」那个例外只属于店铺码活动与在门店买咖啡这两条成交路径。
	StoreID string `json:"storeId"`
	// Reason 必填：这是白送钱，事后只有这一句话答得上来「为什么给他开」。
	Reason string `json:"reason"`
	Remark string `json:"remark"`
	// RequestID 是这次点击的**幂等键**，必填。
	//
	// 客户端在弹窗打开时生成一次、提交时带上；同一个 requestId 重发会拿回同一条会员而不是
	// 409。不能由服务端从 X-Request-Id 取——那是**追踪头**，每个请求都不同。
	RequestID string `json:"requestId"`
}

// AutoRenewRequest 是小程序开关自动续费的请求体。
//
// 它**只能关掉现在开着的那一个**，这也是唯一一个用户自己能改的会员字段。开通自动续费走的是
// 签约那条路（要代扣协议），不是这个接口——见 service 里那段说明。
type AutoRenewRequest struct {
	Enabled bool `json:"enabled"`
}

// MiniappPlanResponse 是小程序看到的套餐形状。
//
// 与 PlanResponse 的差别只有一处，但那一处是必须的：**没有 WechatPlanID，也没有 Status、
// SortOrder、CreatedAt/UpdatedAt**。签约模板 ID 是商户平台的内部标识，透到 C 端没有任何用处，
// 但它会出现在小程序的网络面板里——一个能拿去查我们商户配置的标识不该下发到客户端。
// 其余几个字段是后台的运营属性（草稿态、排序、创建时间），C 端也不该看见。
//
// 分成两个结构而不是给 PlanResponse 加 `json:"-"`：加 omitempty 那种做法改一次后台字段就会
// 悄悄影响 C 端，而这两个契约的**变更节奏完全不同**（后台跟着运营走，C 端跟着发版走）。
type MiniappPlanResponse struct {
	ID          string   `json:"id"`
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Benefits    []string `json:"benefits"`
	PriceCents  int64    `json:"priceCents"`
	Period      string   `json:"period"`
	PeriodCount int32    `json:"periodCount"`
	// AutoRenew 是给 C 端看的「这是连续包月，会到期自动续费」——用户点购买之前要知道这件事。
	AutoRenew bool `json:"autoRenew"`
	// MemberPriceMode 决定购买页上那句权益说明怎么说：「买完自动享会员价」还是
	// 「每期送 N 张会员价券」。两种说法对应两种完全不同的用户预期，不能在客户端猜。
	MemberPriceMode             string `json:"memberPriceMode"`
	MemberPriceCouponsPerPeriod int32  `json:"memberPriceCouponsPerPeriod"`
}
