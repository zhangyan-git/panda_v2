package repository

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// ErrSettlementAmountsDoNotBalance：分账计划的金额对不上（基数 ≠ 平台自留 + Σ 接收方）。
//
// 这条恒等式**跨表，CHECK 表达不了**（008 文件头写着），所以它只能由应用在同一事务里保证。
// 写在这里、以 error 的形式炸出来，是因为它是「钱算错了」的最后一处可见的地方——放过去就是
// 一张平台少拿或多拿的账，而对账要等结算时才发现。
//
// 判它的数**必须是写完之后从库里读回来的**，不能拿算计划时手上那几个变量再算一遍：平台金额
// 是差额倒挤出来的（platform = base − Σreceivers，见 service.computeSettlement），拿同一批
// 数字重算一遍是恒等式两边逐字相同的同义反复，永远为真——曾经就是那样写的。真正的判据在
// readSettlementBalance 里（库里那三行事实），见它的注释。
var ErrSettlementAmountsDoNotBalance = errors.New("settlement amounts do not balance")

// SettlementRuleQuery 是命中一条分账规则要给的维度。
type SettlementRuleQuery struct {
	// BizType 是业务分类，必填，取 settlement_rules.biz_type 的词表。
	BizType string
	// Provider 是这一笔走的渠道**名字**。接收账户必须挂在这个渠道上：分账是渠道能力，一个挂在微信
	// 上的子商户号，银联商务的分账接口不认。
	Provider string
	// 范围的值引用，**空串表示这一档给不出来**（不是「匹配空值」）。device 最具体、global 最宽泛。
	//
	// brand / product 今天恒为空：订单库里没有品牌（brands/stores 属商户域），而一笔支付可能
	// 含多个商品、008 又是「一笔支付一条任务」，没有单一商品可指。命中逻辑收全五档、调用方
	// 只给得出两个——写全是为了等值来源出现时不用改这里。
	DeviceRef  string
	StoreRef   string
	BrandRef   string
	ProductRef string
}

// SettlementRule 是一条命中的规则，连同它的项。
type SettlementRule struct {
	ID             string
	BizType        string
	ScopeType      string
	ScopeRef       string
	AllocationMode string
	Items          []SettlementRuleItem
}

// SettlementRuleItem 是规则里的一项：钱分给谁、分多少。
type SettlementRuleItem struct {
	PartyType string
	// CalcType 取 model.SettlementCalc* 三个值之一。
	CalcType string
	// RatioScaled 是 ratio × 1000000 的**整数**（NUMERIC(20,6) 精确换算，0.45 → 450000）。
	//
	// 用整数而不是 float64 传下去：比例是钱的乘数，float 的尾差会一路走到金额上，而 008 定的
	// 口径是「金额一律 BIGINT 分」。换算放在 SQL 里做（numeric 运算是精确的），Go 这边只做
	// 整数乘除。
	RatioScaled int64
	FixedAmount int64
	SortOrder   int
	// Account 为 nil 表示这一项**没有可用账户**：它指着的那一行被停用了，或者挂在别的渠道上。
	// 平台项永远是 nil（008 的 CHECK：平台项没有 account_id），两者靠 CalcType 分得开。
	Account *SettlementAccount
}

// SettlementAccount 是接收方在渠道侧的那个号。
//
// **不带主体引用**（关联门店/品牌/商户）：那三列在 016 里删了。它们从来没有读者——命中规则挑
// 账户只看 id / status / provider，账户挂在哪条渠道上才是要紧的事。
type SettlementAccount struct {
	ID           string
	PartyName    string
	ReceiverType string
	ReceiverID   string
}

// SettlementPlan 是发起支付那一刻算好的分账计划，随支付单在**同一个事务里**落库。
//
// 它是 repository 的类型而不是 service 的：它整个地作为 BeginPaymentParams 的一个字段被消费
// （与 FundingLine 同一个处境），定义在调用点旁边比藏在另一层好找。
type SettlementPlan struct {
	// RuleID 为空是**常态**：门店没配规则时这条任务照样建（整单归平台），老系统也是这个兜底。
	RuleID string
	// 命中时的范围快照。RuleID 为空时 ScopeType 也留空（008 的 CHECK 允许空串），表示
	// 「这次没有规则命中」，而不是 global 那一档。
	ScopeType string
	ScopeRef  string
	// StoreRef / BrandRef 是**订单侧推过来的**值引用，不是规则的范围：门店没配规则、走的是
	// global 那条规则时，它们照样有值。结算按主体归集时要的正是它们。
	StoreRef    string
	BrandRef    string
	MerchantRef string
	// PlatformAmount 是平台自留，**差额倒挤**：base − Σ Receivers.Amount。
	PlatformAmount int64
	// Receivers 是本笔要发给各接收方的钱。算出来不足 1 分的**不在这里**（008：不建 0 元的行）。
	Receivers []SettlementReceiverLine
}

// SettlementReceiverLine 是 settlement_receivers 的一行，金额与主体快照都在里面。
type SettlementReceiverLine struct {
	AccountID    string
	PartyType    string
	MerchantRef  string
	BrandRef     string
	StoreRef     string
	PartyName    string
	ReceiverType string
	ReceiverID   string
	// RatioScaled 是当初生效的比例（× 1000000）。固定额项记 0，它的金额在 Amount 上——与 008 的
	// 列注释一致：规则改了不影响这一行。
	RatioScaled int64
	Amount      int64
}

// FindSettlementRule 按范围精度命中一条启用中的规则，连同它的项与账户一次取回。
//
// 命中顺序 device → store → brand → product → global，**第一条命中的就是它**。同档位不会撞车：
// 008 的 settlement_rules_scope_uniq 保证 (biz_type, scope_type, scope_ref) 上只有一条启用中的。
//
// 没命中返回 (nil, nil)——这不是错误，是「这个门店没配规则」，调用方按「整单归平台」落任务。
// **查失败（error）与没命中必须分得开**：前者是配置库读不动，后者是一个正常的业务事实。
// 老系统把两者一起当成「没规则、全归平台」，于是配置故障被静默变成一次错误的分账。
func (r *PostgresRepository) FindSettlementRule(ctx context.Context, q SettlementRuleQuery) (*SettlementRule, error) {
	// 五个档位写成一条 OR，顺序由 CASE 定。空的范围引用**天然命中不了非 global 的规则**：
	// 008 的 CHECK 钉住了「非 global ⇒ scope_ref <> ''」，所以 store_ref 为空的规则不存在。
	rule := &SettlementRule{}
	err := r.pool.QueryRow(ctx, `SELECT id::text, biz_type, scope_type, scope_ref, allocation_mode
		FROM settlement_rules
		WHERE status = 'enabled' AND biz_type = $1
			AND ( (scope_type = 'device'  AND scope_ref = $2)
			   OR (scope_type = 'store'   AND scope_ref = $3)
			   OR (scope_type = 'brand'   AND scope_ref = $4)
			   OR (scope_type = 'product' AND scope_ref = $5)
			   OR (scope_type = 'global') )
		ORDER BY CASE scope_type
			WHEN 'device' THEN 0 WHEN 'store' THEN 1 WHEN 'brand' THEN 2
			WHEN 'product' THEN 3 ELSE 4 END
		LIMIT 1`,
		q.BizType, q.DeviceRef, q.StoreRef, q.BrandRef, q.ProductRef).
		Scan(&rule.ID, &rule.BizType, &rule.ScopeType, &rule.ScopeRef, &rule.AllocationMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	items, err := r.settlementRuleItems(ctx, rule.ID, q.Provider)
	if err != nil {
		return nil, err
	}
	rule.Items = items
	return rule, nil
}

// settlementRuleItems 读一条规则的项，并把每一项的账户解析出来。
//
// 账户用 LEFT JOIN 而不是 JOIN：**项还在、账户不可用**是一种必须看得见的状态（它的钱要归平台，
// 而且要记一条 warn）。INNER JOIN 会把那一项整个吞掉，于是「配置里有三项、分出去两项」这件事
// 在数据上完全看不出来——而它正是运营改配置时最需要看到的那条线索。
//
// 可用的判据两条：账户是启用中的（停用了就不再参与新分账，008 的列注释），以及账户挂在本笔
// 支付走的那个渠道上。
//
// 渠道那一列今天是 TEXT（渠道名），不再需要 NULLIF(...)::uuid：空串本身就能比，
// 而「没有渠道」的账户（要是将来真有）也就能挂在空串上。
func (r *PostgresRepository) settlementRuleItems(ctx context.Context, ruleID, provider string) ([]SettlementRuleItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT i.party_type, i.calc_type, (i.ratio * 1000000)::bigint,
			i.fixed_amount, i.sort_order,
			a.id::text, COALESCE(a.party_name,''), COALESCE(a.receiver_type,''),
			COALESCE(a.receiver_id,'')
		FROM settlement_rule_items i
		LEFT JOIN settlement_accounts a
			ON a.id = i.account_id AND a.status = 'enabled' AND a.provider = $2
		WHERE i.rule_id = $1::uuid
		ORDER BY i.sort_order, i.id`, ruleID, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []SettlementRuleItem
	for rows.Next() {
		var (
			item    SettlementRuleItem
			account *SettlementAccount
			id      *string
		)
		var partyName, receiverType, receiverID string
		if err := rows.Scan(&item.PartyType, &item.CalcType, &item.RatioScaled, &item.FixedAmount,
			&item.SortOrder, &id, &partyName, &receiverType, &receiverID); err != nil {
			return nil, err
		}
		if id != nil {
			account = &SettlementAccount{
				ID: *id, PartyName: partyName, ReceiverType: receiverType, ReceiverID: receiverID,
			}
		}
		item.Account = account
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// insertSettlementTask 把分账计划写成一条任务与它的接收方明细。
//
// 它**只是 BeginPayment 那一个事务里多出来的一段**，不是独立的事务：任务与支付单必须同生共死。
// 分开了会怎样——建了支付单没建任务，这一笔就永远不会有分账，而且没有任何报错（正是缺口 A 那种
// 「安静地建不出来」）；建了任务没建支付单更糟，一张没有钱对应着的分账任务。
func insertSettlementTask(ctx context.Context, tx pgx.Tx, payment *model.Payment, plan *SettlementPlan) error {
	if plan == nil {
		// nil 表示这次不发分账（账户出资：渠道分账分的是渠道里的钱，豆支付没有渠道资金可动）。
		return nil
	}

	var taskID string
	if err := tx.QueryRow(ctx, `INSERT INTO settlement_tasks
		(task_no, payment_id, payment_no, base_amount, platform_amount, rule_id,
		 scope_type, scope_ref, store_ref, brand_ref, merchant_ref, status)
		VALUES ($1,$2::uuid,$3,$4,$5,NULLIF($6,'')::uuid,$7,$8,$9,$10,$11,$12)
		RETURNING id::text`,
		newSettlementTaskNo(time.Now()), payment.ID, payment.PaymentNo, payment.Amount,
		plan.PlatformAmount, plan.RuleID, plan.ScopeType, plan.ScopeRef,
		plan.StoreRef, plan.BrandRef, plan.MerchantRef,
		model.SettlementStatusPending).Scan(&taskID); err != nil {
		return mapPGError(err)
	}

	for _, receiver := range plan.Receivers {
		if _, err := tx.Exec(ctx, `INSERT INTO settlement_receivers
			(task_id, account_id, party_type, merchant_ref, brand_ref, store_ref, party_name,
			 receiver_type, receiver_id, ratio, amount, status)
			VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,($10::bigint)::numeric / 1000000,$11,$12)`,
			taskID, receiver.AccountID, receiver.PartyType, receiver.MerchantRef, receiver.BrandRef,
			receiver.StoreRef, receiver.PartyName, receiver.ReceiverType, receiver.ReceiverID,
			receiver.RatioScaled, receiver.Amount, model.SettlementStatusPending); err != nil {
			return mapPGError(err)
		}
	}

	// 任务与接收方都写完了，再从库里把这三行事实读回来核一次恒等式（见 readSettlementBalance
	// 与 checkSettlementIdentity）。放在所有 INSERT **之后**：这一处要回答的是「库里现在是什么
	// 样」，写之前没有任何东西可读。它失败会把整个事务（连同支付单）一起回滚掉——这正是一张
	// 算不平的分账账该有的下场：宁可这一次付不成，也不要一张平台少拿了钱的单悄悄进来
	// （与 buildSettlementPlan 那句「一次分账错误比一次付不成更贵」同一条判断）。
	balance, err := readSettlementBalance(ctx, tx, taskID)
	if err != nil {
		return err
	}
	return checkSettlementIdentity(balance)
}

// settlementBalance 是一条分账任务写进库之后，从库里读回来的三个数。
//
// 它是个纯数据包（不是从计划里抄来的），存在的意义就是让 checkSettlementIdentity 可以在没有
// 数据库的情况下被测：把三个数摆成不平的样子，断言那条错误真的会被返回——这件事在改动之前
// 做不到，因为当时那个「检查」拿的是同一批变量、永远为真。
type settlementBalance struct {
	TaskNo string
	// Base / Platform 是 settlement_tasks 行上的值。
	Base     int64
	Platform int64
	// Receivers 是库对 settlement_receivers 行自己求的和；ReceiverRows 是行数，带在错误信息里
	// 是为了让「和不对」与「行没写全」在同一句话里分得清。
	Receivers    int64
	ReceiverRows int
}

// checkSettlementIdentity 核 008 文件头写的那条恒等式：base = platform + Σreceivers。
//
// 它是个**纯函数**，不碰数据库：判据（三个数从哪来）与比较（三个数平不平）分开，后者才需要
// 被穷举测。唯一的写法要点是比较**读回来的**三个数——见 ErrSettlementAmountsDoNotBalance
// 的注释：用算计划时手上的变量重算一遍是永远为真的同义反复。
func checkSettlementIdentity(b settlementBalance) error {
	if b.Platform+b.Receivers != b.Base {
		return fmt.Errorf("%w: task %s base %d, platform %d, receivers %d (%d rows)",
			ErrSettlementAmountsDoNotBalance, b.TaskNo, b.Base, b.Platform, b.Receivers, b.ReceiverRows)
	}
	return nil
}

// readSettlementBalance 把一条刚写完的分账任务从库里读回来。
//
// 三个数来自**库自己**：任务行上的两列，以及数据库对接收方行做的聚合（SUM/COUNT）。这与
// 「拿计划里的 platform 与计划里的 receivers 相减」是两回事——后者是同一个表达式的两边，
// 前者能看见写的过程中丢掉的东西：漏写一行（将来某次改动把 INSERT 换成 ON CONFLICT DO NOTHING
// 就会静默丢行）、金额被列的精度截断、触发器或将来某个 BEFORE INSERT 规则动过那两个数。
// 这些都是「算的时候没错、落库之后不对」的情形，只有在写完之后回读才看得见。
//
// 一笔接收方都没有是合法的（整单归平台，计划里 Receivers 为空），所以 LEFT JOIN + COALESCE：
// INNER JOIN 会让那条任务一行都读不出来，把「归平台」错报成「任务不存在」。
func readSettlementBalance(ctx context.Context, tx pgx.Tx, taskID string) (settlementBalance, error) {
	var balance settlementBalance
	if err := tx.QueryRow(ctx, `SELECT t.task_no, t.base_amount, t.platform_amount,
			COALESCE(SUM(r.amount), 0)::bigint, COUNT(r.id)::int
		FROM settlement_tasks t
		LEFT JOIN settlement_receivers r ON r.task_id = t.id
		WHERE t.id = $1::uuid
		GROUP BY t.task_no, t.base_amount, t.platform_amount`, taskID).
		Scan(&balance.TaskNo, &balance.Base, &balance.Platform, &balance.Receivers, &balance.ReceiverRows); err != nil {
		return settlementBalance{}, mapPGError(err)
	}
	return balance, nil
}

// cancelPendingSettlementInTx 把这张支付单上**还没发往渠道**的分账任务与接收方一起作废。
//
// 支付单进 failed / expired 时调它。作废只会发生在 pending 上：那时还没向渠道发起过，渠道侧
// 没有任何东西要收回。已经 submitted 的任务不动——钱可能已经分出去了，那种只能走
// settlement_reversals 回退（008 的状态说明）。
//
// **接收方先改、任务后改**，顺序是有用的：第一句要在任务还是 pending 的时候读它，反过来就一条
// 也匹配不到了。
func cancelPendingSettlementInTx(ctx context.Context, tx pgx.Tx, paymentID string) error {
	if _, err := tx.Exec(ctx, `UPDATE settlement_receivers r
		SET status=$2, updated_at=NOW()
		FROM settlement_tasks t
		WHERE r.task_id = t.id AND t.payment_id = $1::uuid
			AND t.status = $3 AND r.status = $3`,
		paymentID, model.SettlementStatusCancelled, model.SettlementStatusPending); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE settlement_tasks
		SET status=$2, updated_at=NOW()
		WHERE payment_id = $1::uuid AND status = $3`,
		paymentID, model.SettlementStatusCancelled, model.SettlementStatusPending)
	return err
}

// succeedSettlementInTx 把这张支付单上的分账任务与接收方一起置为成功。
//
// 支付成功回调里调它，紧跟着 payments 置 succeeded 那一句、同一事务。**这里不向渠道发任何东西**：
// 分账指令是随下单报文一起下去的（provider.DivisionInstruction），渠道给我们回「支付成功」时，
// 那次下发里带的子单就已经按同一份报文生效了——银联商务没有单独的「发起分账/查分账」两步，
// 老系统也是这么认的（docs/unionpay-h5-pay.md）。
//
// 代价写在明处：渠道若实际分账失败，本地会显示成功。要收口只能靠对账，不是靠再发一次请求。
//
// 没命中规则的单**也有**任务（整单归平台、接收方为空），照样要置成功——否则它会永远停在
// pending 里，被「未完成的任务」这类视图一直捞出来。
//
// **接收方先改、任务后改**，与 cancelPendingSettlementInTx 同理：第一句要在任务还是 pending
// 的时候读它，反过来就一条也匹配不到了。
func succeedSettlementInTx(ctx context.Context, tx pgx.Tx, paymentID, providerTransactionID string, finishedAt time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE settlement_receivers r
		SET status=$2, updated_at=NOW()
		FROM settlement_tasks t
		WHERE r.task_id = t.id AND t.payment_id = $1::uuid
			AND t.status = $3 AND r.status = $3`,
		paymentID, model.SettlementStatusSucceeded, model.SettlementStatusPending); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE settlement_tasks
		SET status=$2,
		    provider_transaction_id=COALESCE(NULLIF($3,''), provider_transaction_id),
		    finished_at=$4, attempts=attempts+1, last_error='', updated_at=NOW()
		WHERE payment_id = $1::uuid AND status = $5`,
		paymentID, model.SettlementStatusSucceeded, providerTransactionID, finishedAt,
		model.SettlementStatusPending)
	return err
}

// newSettlementTaskNo 生成分账任务号：SET + YmdHis + 6 位随机。
//
// 与支付单号（service 的 paymentNo）同一个形状，只是前缀不同。范围小写不带渠道约束，按可读性取：
// 它就是给人对着渠道账单找单用的。撞号由 settlement_tasks_task_no_key 挡下来。
func newSettlementTaskNo(now time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		// crypto/rand 在 darwin/linux 上不会失败；真失败了也不该退回固定值——那会让同一秒内
		// 的任务号完全由时间戳决定，撞号从概率问题变成必然问题。
		panic(fmt.Sprintf("payment-service: read random for settlement task number: %v", err))
	}
	tail := uint32(random[0])<<16 | uint32(random[1])<<8 | uint32(random[2])
	return fmt.Sprintf("SET%s%06d", now.Format("20060102150405"), tail%1000000)
}
