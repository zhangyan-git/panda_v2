package model

// 分账的受控词表，与 settlement_rules / settlement_rule_items / settlement_accounts 上的 CHECK 逐字一致。
//
// 三组词表放在一个文件里，因为它们是同一张表族的同一批取值：改词表 = 改那三张表的 CHECK + 改这里。
// 散在各处的话，「加了第五个 biz_type 却只改了一半」是必然会发生的事。

// 分账业务分类（settlement_rules.biz_type），也是发起支付时随请求过来的那个 biz_type。
//
// store_consume 是「到店消费」，老系统 store_pos 的存量口径：V2 没有产生它的来源，所以
// order-service 推不出这个值（见 order-service 的 model.SettlementBiz*）。它留在这里只为
// 存量数据与词表完整——规则表里可以配，今天命中不了。
const (
	SettlementBizCoffee       = "coffee"
	SettlementBizMembership   = "membership"
	SettlementBizStoreConsume = "store_consume"
	SettlementBizAddonProduct = "addon_product"
)

// IsSettlementBizType 判断一个值是不是词表里的业务分类。
//
// 校验它的理由**只有一个**：biz_type 是规则命中键的第一段，写错的表现是「安静地命不中规则、
// 整单归平台」——没有报错、没有异常，只是钱分错了地方。所以它是发起支付这条链路上唯一值得
// 在入口就拦下来的分账维度（store/device 落的是 TEXT 快照列，写错只影响命中，不会失真）。
func IsSettlementBizType(value string) bool {
	switch value {
	case SettlementBizCoffee, SettlementBizMembership, SettlementBizStoreConsume, SettlementBizAddonProduct:
		return true
	default:
		return false
	}
}

// 规则范围档位（settlement_rules.scope_type）。
//
// 声明顺序就是命中顺序：从具体到宽泛，先命中先返回（见 repository.FindSettlementRule）。
// 同一个 biz_type 下同档位只能有一条启用中的规则（settlement_rules_scope_uniq），所以这个顺序是确定的。
const (
	SettlementScopeDevice  = "device"
	SettlementScopeStore   = "store"
	SettlementScopeBrand   = "brand"
	SettlementScopeProduct = "product"
	SettlementScopeGlobal  = "global"
)

// IsSettlementScopeType 判断一个值是不是词表里的档位。
//
// 与 IsSettlementBizType 同一条道理，只是代价不同：档位写错不会分错钱（命不中就是整单归平台，
// 与 biz_type 写错同形），所以它值得拦的理由是**后台要能把「你填错了」和「这类业务今天没有
// 规则」分开说** —— 两者在列表里都是一片空白。
func IsSettlementScopeType(value string) bool {
	switch value {
	case SettlementScopeDevice, SettlementScopeStore, SettlementScopeBrand,
		SettlementScopeProduct, SettlementScopeGlobal:
		return true
	default:
		return false
	}
}

// 规则项的收款主体类型（settlement_rule_items.party_type 与 settlement_accounts.party_type
// 共用一套词表，两张表的 CHECK 是同一套取值）。
const (
	SettlementPartyPartner     = "partner"
	SettlementPartyCityCenter  = "city_center"
	SettlementPartyAgent       = "agent"
	SettlementPartyMemberStore = "member_store"
	SettlementPartyPlatform    = "platform"
)

// IsSettlementPartyType 判断一个值是不是词表里的主体类型。
func IsSettlementPartyType(value string) bool {
	switch value {
	case SettlementPartyPartner, SettlementPartyCityCenter, SettlementPartyAgent,
		SettlementPartyMemberStore, SettlementPartyPlatform:
		return true
	default:
		return false
	}
}

// IsSettlementCalcType / IsSettlementAllocationMode：写入口的白名单。
//
// 三组 bool 判据放在这一处而不是各自写在 service 里：它们是**词表的判据**，与常量在同一行
// 视野里，加第五个档位时能看见自己该改哪两行。
func IsSettlementCalcType(value string) bool {
	switch value {
	case SettlementCalcPercent, SettlementCalcFixed, SettlementCalcRemainder:
		return true
	default:
		return false
	}
}

// IsSettlementAllocationMode 判断一个值是不是词表里的分配模式。
func IsSettlementAllocationMode(value string) bool {
	switch value {
	case SettlementAllocationNormal, SettlementAllocationFixedThenRemaining:
		return true
	default:
		return false
	}
}

// 规则与账户共用的启用状态（settlement_rules.status / settlement_accounts.status，两张表的
// CHECK 是同一对取值）。
//
// 它与下面那组任务状态是**两件事**，只是名字撞在一起：这两个回答「这条配置参不参与命中」，
// 那三个回答「这一笔钱分到哪一步了」。
const (
	SettlementRecordEnabled  = "enabled"
	SettlementRecordDisabled = "disabled"
)

// IsSettlementRecordStatus 判断一个值是不是启用/停用之一。
func IsSettlementRecordStatus(value string) bool {
	switch value {
	case SettlementRecordEnabled, SettlementRecordDisabled:
		return true
	default:
		return false
	}
}

// 账户上的渠道侧接收方类型（settlement_accounts.receiver_type）。
//
// MERCHANT_ID 是默认、也是今天唯一用得到的那个：银联商务按子商户号直接分，取不到「个人 openid」
// 这个概念。PERSONAL_OPENID 是微信服务商分账要的那一种，跟着词表留着。
const (
	SettlementReceiverMerchantID     = "MERCHANT_ID"
	SettlementReceiverPersonalOpenID = "PERSONAL_OPENID"
)

// IsSettlementReceiverType 判断一个值是不是词表里的接收方类型。
func IsSettlementReceiverType(value string) bool {
	switch value {
	case SettlementReceiverMerchantID, SettlementReceiverPersonalOpenID:
		return true
	default:
		return false
	}
}

// 规则项的算法（settlement_rule_items.calc_type）。
const (
	// SettlementCalcPercent 按基数乘比例（ratio，0~1）。
	SettlementCalcPercent = "percent"
	// SettlementCalcFixed 拿固定额（fixed_amount，单位分）。
	SettlementCalcFixed = "fixed"
	// SettlementCalcRemainder 平台自留，拿的是**差额**（基数 − 其他接收方）。只有平台项能用，
	// settlement_rule_items 的 CHECK 把这一条钉死了：别的项用 remainder 等于让某个门店去当那个「剩下的」，
	// 既算不清也说不通。
	SettlementCalcRemainder = "remainder"
)

// 分配模式（settlement_rules.allocation_mode）。
const (
	// SettlementAllocationNormal 各接收方按各自的比例/固定额拿，剩下的归平台。
	SettlementAllocationNormal = "normal"
	// SettlementAllocationFixedThenRemaining 先扣掉所有固定额，剩余部分再按比例分。
	SettlementAllocationFixedThenRemaining = "fixed_then_remaining"
)

// 任务与接收方的状态。这里只列本服务**写**的那三个。
//
// 没写的：submitted（微信那种「先发起、等回执」的四步走法才需要，我们走的是一次下发）、
// failed 与 returned（渠道明确拒绝、以及退回，两条路都还没有实现）。settlement_tasks.status 已经把落点摆好，
// 实现时按它的状态说明写，别在这里补一个没人写的常量。
const (
	// SettlementStatusPending 已建任务、还没向渠道发起。**发起支付时就写它**：任务提前建，
	// 而渠道侧的分账一律要等支付成功——钱先到账，才谈得上分。
	SettlementStatusPending = "pending"
	// SettlementStatusSucceeded 分账成功。
	//
	// 写它的是**支付成功回调**（见 repository.succeedSettlementInTx），不是某个分账回执：
	// 银联商务的分账指令随下单一次下发，发出去就是终局，成功应答本身就是分账成功的凭据
	// （老系统同款口径，见 docs/unionpay-h5-pay.md）。将来做查单时它也能写这个状态。
	SettlementStatusSucceeded = "succeeded"
	// SettlementStatusCancelled 这笔支付没成，任务随之作废。只能从 pending 进：那时还没发往
	// 渠道，没有要收回的钱；已经发出去的任务只能走 settlement_reversals 回退。
	SettlementStatusCancelled = "cancelled"
)

// IsSettlementTaskStatus 判断一个值是不是 settlement_tasks.status 那一列上的六个取值
// 之一。
//
// **它认的是列的全集，不是本服务写的那三个**（上面那组常量）：后台的筛选框要能筛出库里真有的
// 每一行，而 submitted / failed / returned 是留给微信四步那条路与将来的——今天写不进去，
// 但「筛不出来」会在那些值真出现的那一天变成一个新的谜。拦在这里的是**拼错的值**
// （`succeded`），那种错误与「这段时间真的一笔都没有」在列表上长得一模一样。
func IsSettlementTaskStatus(value string) bool {
	switch value {
	case SettlementStatusPending, SettlementStatusSucceeded, SettlementStatusCancelled,
		"submitted", "failed", "returned":
		return true
	default:
		return false
	}
}
