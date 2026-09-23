package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
)

// 这一组守的是**后台写入口里唯一一条靠 SQL 才答得上来的判断**：改一个账户的渠道之前，
// 先看看有没有启用中的规则正引用着它（ErrSettlementAccountChannelInUse）。
//
// 为什么它必须连库：这条判断的判据是一句 `COUNT(*) ... JOIN settlement_rules`，而它要回答的
// 问题恰恰是「库里现在有几条启用中的规则挂着这个账户」。单元测试能验的只有「拿到那个数之后
// 怎么分支」，真正会错的是那句 SQL——JOIN 写反、把停用的规则也数进去、漏掉 account_id 那个
// 条件，三种错法都不报错，只会让一次**本该被拒的改渠道**静默通过，然后那条规则里的这一项在
// 执行期被 `a.provider = 这笔支付的渠道` 滤掉，钱按差额倒挤回平台。
//
// 与其它集成用例同一个约定：只用一个调用方给的库 URL，不建库、不删库、不跑迁移，跑完把自己的
// 行删干净。**message_outbox 不清理**（审计落在那里），残留行每次都是新的聚合 id。
//
// 夹具里的账户**不填任何主体引用**，与页面上配出来的账户一样。这曾经是个麻烦：那时
// settlement_accounts 的唯一键是「四个引用 + 渠道」，全空会退化成 (类型, '', '', '', 渠道)，
// 于是这条夹具会与 dev 库里任何一条同形的启用账户（比如 e2e 留下的那一条）撞上 23505，用例
// 就变成了在验夹具而不是在验代码——当时靠给 store_ref 塞一个随机 uuid 绕过去。
// 现在账户表上没有主体引用那几列，唯一键只认 (渠道, 接收方类型, 接收方号)（settlement_accounts_receiver_uniq，
// 且只对启用中的行生效），夹具因此可以照页面上的真实形状写。

// settlementAccountFixture 是一个账户加上一条引用它的规则。
type settlementAccountFixture struct {
	accountID string
	ruleID    string
	// values 是这个账户当前的完整内容。UpdateSettlementAccount 是整份覆盖，所以每次写入都要
	// 从它派生——少填一栏就是把那一栏清空，而那会让后面的断言看起来像通过了。
	values SettlementAccountWrite
}

// accountWrite 给出一份「除了渠道以外都不动」的写入。
func (f *settlementAccountFixture) accountWrite(provider string) SettlementAccountWrite {
	write := f.values
	write.Provider = provider
	return write
}

// newSettlementAccountFixture 造一个账户与一条引用它的规则。ruleStatus 决定那条规则启用还是
// 停用——这是这条用例唯一的自变量。
func newSettlementAccountFixture(t *testing.T, pool *pgxpool.Pool, ruleStatus string) *settlementAccountFixture {
	t.Helper()
	ctx := context.Background()

	fixture := &settlementAccountFixture{
		accountID: uuid.NewString(),
		ruleID:    uuid.NewString(),
		values: SettlementAccountWrite{
			PartyName:    "改渠道用例门店",
			PartyType:    "member_store",
			Provider:     "ums",
			ReceiverType: "MERCHANT_ID",
			ReceiverID:   "MID-" + uuid.NewString(),
			Status:       "enabled",
		},
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 顺序由外键决定：项挂着账户（RESTRICT）也挂着规则（CASCADE），所以先删项最省事。
		_, _ = pool.Exec(ctx, `DELETE FROM settlement_rule_items WHERE rule_id=$1`, fixture.ruleID)
		_, _ = pool.Exec(ctx, `DELETE FROM settlement_rules WHERE id=$1`, fixture.ruleID)
		_, _ = pool.Exec(ctx, `DELETE FROM settlement_accounts WHERE id=$1`, fixture.accountID)
	})

	if _, err := pool.Exec(ctx, `INSERT INTO settlement_accounts
		(id, party_name, party_type, provider, receiver_type, receiver_id, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		fixture.accountID, fixture.values.PartyName,
		fixture.values.PartyType, fixture.values.Provider,
		fixture.values.ReceiverType, fixture.values.ReceiverID, fixture.values.Status); err != nil {
		t.Fatalf("insert settlement account: %v", err)
	}
	// scope_ref 用随机 uuid：启用中的规则在同一档位上会被 settlement_rules_scope_uniq 拦，
	// 而这条用例要的只是「有一条启用中的规则引用着这个账户」。
	if _, err := pool.Exec(ctx, `INSERT INTO settlement_rules
		(id, name, biz_type, scope_type, scope_ref, allocation_mode, status)
		VALUES ($1,$2,'coffee','store',$3,'normal',$4)`,
		fixture.ruleID, "改渠道用例规则-"+fixture.ruleID[:8], uuid.NewString(), ruleStatus); err != nil {
		t.Fatalf("insert settlement rule: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settlement_rule_items
		(rule_id, party_type, calc_type, ratio, fixed_amount, account_id, sort_order)
		VALUES ($1,'member_store','percent',0.45,0,$2,0)`,
		fixture.ruleID, fixture.accountID); err != nil {
		t.Fatalf("insert settlement rule item: %v", err)
	}
	return fixture
}

// accountProvider 读回账户当前的渠道，用来验「被拒的那一次没有留下半次写入」。
func accountProvider(t *testing.T, pool *pgxpool.Pool, accountID string) string {
	t.Helper()
	var provider string
	if err := pool.QueryRow(context.Background(),
		`SELECT provider FROM settlement_accounts WHERE id=$1`, accountID).Scan(&provider); err != nil {
		t.Fatalf("read settlement account: %v", err)
	}
	return provider
}

// TestPostgresUpdateAccountProviderGuardedByRules 是这条守卫的三段：改别的不拦、改渠道被拦、
// 把规则停用之后放行。
//
// 三段必须一起测：只测中间那一段的话，一个「任何更新都拒」的写法（比如把判断写成无条件）会
// 全绿，而那种写法会让这个页面上的每一次保存都失败。
func TestPostgresUpdateAccountProviderGuardedByRules(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	fixture := newSettlementAccountFixture(t, pool, "enabled")

	// ① 渠道没变：引用它的规则再多也不该拦——这一页上最常见的一次保存就是改个名字。
	renamed := fixture.accountWrite("ums")
	renamed.PartyName = "改过名的门店"
	if _, err := repo.UpdateSettlementAccount(ctx, fixture.accountID, renamed); err != nil {
		t.Fatalf("只改名字被拦了：%v", err)
	}

	// ② 改渠道：被启用中的规则引用着，必须拒。
	if _, err := repo.UpdateSettlementAccount(ctx, fixture.accountID, fixture.accountWrite("wechat_pay")); !errors.Is(err, ErrSettlementAccountChannelInUse) {
		t.Fatalf("改渠道的错误 = %v，期望 ErrSettlementAccountChannelInUse", err)
	}
	// 拒了就得一栏都不动：这条判断排在 UPDATE 之前，而它一旦排到后面，用户会拿到错误却
	// 发现渠道已经变了——那正是这条守卫要防的局面，只是换了个方式发生。
	if got := accountProvider(t, pool, fixture.accountID); got != "ums" {
		t.Fatalf("被拒之后 provider = %q，期望还是 ums", got)
	}

	// ③ 停用那条规则：同一次改渠道就该放行——这是报错里写的那个出口。
	if _, err := pool.Exec(ctx, `UPDATE settlement_rules SET status='disabled' WHERE id=$1`, fixture.ruleID); err != nil {
		t.Fatalf("disable settlement rule: %v", err)
	}
	updated, err := repo.UpdateSettlementAccount(ctx, fixture.accountID, fixture.accountWrite("wechat_pay"))
	if err != nil {
		t.Fatalf("停用规则之后改渠道仍然被拦：%v", err)
	}
	if updated.Provider != "wechat_pay" {
		t.Fatalf("provider = %q，期望 wechat_pay", updated.Provider)
	}
}

// TestPostgresUpdateAccountProviderIgnoresDisabledRules 是上一条的反面：只有**启用中的**规则
// 才该拦住改渠道。
//
// 单拎出来是因为「把所有引用都数进去」是个很容易写出来的版本（那句 JOIN 少一个 WHERE 就是
// 它），而它的表现是：一条早就停用的规则会把一个账户永久钉死在原来的渠道上，页面上只给一句
// 「请先改掉引用它的规则」——而那条规则在分账规则页上根本看不出和这个账户有什么关系。
func TestPostgresUpdateAccountProviderIgnoresDisabledRules(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	fixture := newSettlementAccountFixture(t, pool, "disabled")

	updated, err := repo.UpdateSettlementAccount(ctx, fixture.accountID, fixture.accountWrite("wechat_pay"))
	if err != nil {
		t.Fatalf("被一条停用的规则拦住了：%v", err)
	}
	if updated.Provider != "wechat_pay" {
		t.Fatalf("provider = %q，期望 wechat_pay", updated.Provider)
	}
}
