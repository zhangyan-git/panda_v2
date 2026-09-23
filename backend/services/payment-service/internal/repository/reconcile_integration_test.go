package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 这一组用例守的是主动查单那条扫描（payments_pending_reconcile_idx 服务的就是它）：**一分钟一轮的补偿任务每轮问谁**。
//
// 只有真库能验它。三件事全写在那一条 WHERE 里——挑 pending、挑有渠道的、挑「发起之后一直没动过」
// 的——而写完它们之后，任何一层都补不回来：多挑一张，就会对着一个没有渠道可问的账户出资单去查
// 渠道的账；少挑一张，那笔用户真付了钱、回调却没进来的单就永远停在待支付，而这正是这条任务存在
// 的全部理由。金额对不上还能靠结算那道闸兜住，**漏扫没有任何东西兜**。
//
// 与 account_funding_integration_test.go 同一个约定：只用一个调用方给的库 URL，不建库、不删库、
// 不跑迁移，夹具一律用随机 uuid，跑完自己删干净。断言一律是「我这几张单在不在这批里」，不数总数
// ——这个库是共享的，里面本来就有别的 pending 行（历次验证留下的），数总数只会让用例随时间碎掉。

// stalePaymentFixture 记的是「怎么认出这张单」——断言全部按 payment_no 做成员判断，其余值一律
// 从库里读回来，不在这里存一份（存了就会有「夹具以为自己造了什么」与「库里真有什么」分岔的
// 那一天，而那种分岔只会让断言验的是夹具而不是 SQL）。
type stalePaymentFixture struct {
	paymentID string
	paymentNo string
}

// newStalePaymentFixture 造一张单。
//
// status、provider、updatedAt 全都留着参数：这条查询的三条判据（是不是 pending、有没有渠道、
// 多久没动过）各要一个反例，而反例只有在其余条件都**与正例相同**时才算数。
func newStalePaymentFixture(t *testing.T, pool *pgxpool.Pool, status, provider string, updatedAt time.Time) *stalePaymentFixture {
	t.Helper()
	fixture := &stalePaymentFixture{
		paymentID: uuid.NewString(),
		paymentNo: "PAY-REC-" + uuid.NewString(),
	}
	// payment_method 一并给上：这条查询自己不看它，但选出来的行会被服务层拿去解渠道
	// （见 service.reconcilePendingPayment），形状与真行一样才验得动下一段。
	//
	// expires_at 推到一天后：夹具不是「到点该作废」的单，别让并排跑着的关单扫描把它收走。
	_, err := pool.Exec(context.Background(), `INSERT INTO payments
		(id, payment_no, order_no, user_id, amount, status,
		 provider, payment_method, request_id, expires_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW() + INTERVAL '1 day',$10)`,
		fixture.paymentID, fixture.paymentNo, "ORD-REC-"+uuid.NewString(), uuid.NewString(),
		1980, status, provider, catalog.CodeUMSH5Alipay, "req-"+fixture.paymentID, updatedAt)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 状态流水不删（append-only 触发器直接拒），夹具没写过它。
		_, _ = pool.Exec(ctx, `DELETE FROM payment_fundings WHERE payment_id=$1`, fixture.paymentID)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_transactions WHERE payment_no=$1`, fixture.paymentNo)
		_, _ = pool.Exec(ctx, `DELETE FROM payments WHERE id=$1`, fixture.paymentID)
	})
	return fixture
}

// selected 在那批里找这几张单，返回「找到了哪几张」。
//
// 按 payment_no 找而不是按下标：这批里还有别人的行。
func selected(payments []model.Payment, fixtures ...*stalePaymentFixture) map[string]bool {
	found := make(map[string]bool, len(fixtures))
	for i := range fixtures {
		found[fixtures[i].paymentNo] = false
	}
	for i := range payments {
		if _, ok := found[payments[i].PaymentNo]; ok {
			found[payments[i].PaymentNo] = true
		}
	}
	return found
}

// TestPostgresListStalePendingPaymentsSelectsOnlyChannelPaymentsThatHaveStoppedMoving 是这一组的
// 主干，一次摆四张单，三条判据各验正反两面。
//
// 摆法（窗口取「一分钟前」，即 updatedAt <= NOW()-1min 的才算数）：
//
//	滞后的、有渠道的    ← 这一张是这条查询存在的全部理由，必须在
//	刚发起的、有渠道的  ← 还在等回调，不该被问
//	滞后的、没渠道的    ← 账户出资，没有渠道可问（它的补偿是另一条查询）
//	滞后的、已是终态的  ← 早就结完了，问它是白花一次出网
//
// 四张摆在一起是刻意的：只有反例与正例**除了被验的那一格以外全都相同**，断言才说明得了
// 「这一格是判据」，否则它验的只是「这个函数返回了非空」。
func TestPostgresListStalePendingPaymentsSelectsOnlyChannelPaymentsThatHaveStoppedMoving(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())

	now := time.Now().UTC()
	stale := newStalePaymentFixture(t, pool, model.PaymentPending, catalog.ChannelCodeUMS, now.Add(-time.Hour))
	fresh := newStalePaymentFixture(t, pool, model.PaymentPending, catalog.ChannelCodeUMS, now)
	unroutable := newStalePaymentFixture(t, pool, model.PaymentPending, "", now.Add(-time.Hour))
	settled := newStalePaymentFixture(t, pool, model.PaymentSucceeded, catalog.ChannelCodeUMS, now.Add(-time.Hour))

	payments, err := repo.ListStalePendingPayments(context.Background(), now.Add(-time.Minute), 100)
	if err != nil {
		t.Fatalf("ListStalePendingPayments: %v", err)
	}

	found := selected(payments, stale, fresh, unroutable, settled)
	if !found[stale.paymentNo] {
		t.Fatalf("发起满一小时、回调没来、停在 pending 的单没被选中——这条任务就是为它存在的")
	}
	if found[fresh.paymentNo] {
		t.Errorf("刚发起不到一分钟的单被选中了（%s）：它可能只是用户还在收银页上", fresh.paymentNo)
	}
	if found[unroutable.paymentNo] {
		t.Errorf("没有渠道的账户出资单被选中了（%s）：它没有渠道可问，补偿走另一条路", unroutable.paymentNo)
	}
	if found[settled.paymentNo] {
		t.Errorf("已经是终态的单被选中了（%s）：问它白花一次出网，而且结算那一步必然被拒", settled.paymentNo)
	}

	// 顺序也是这条查询的一部分：最久没动的排最前。积压时 limit 会把排在后面的截掉，而
	// 「先问谁」直接决定了最老的那些单要等多久——排错了，老单会永远排在尾巴上取不到。
	//
	// 只在这两条相邻的自家夹具上比：别人的行夹在中间不影响这个断言（它是按位置比，不是按值）。
	for i := 1; i < len(payments); i++ {
		if payments[i].UpdatedAt.Before(payments[i-1].UpdatedAt) {
			t.Fatalf("第 %d 行（%s）比前一行还旧：结果没有按 updated_at 升序——最久没动的那些会被 limit 挡在门外",
				i, payments[i].PaymentNo)
		}
	}

	// 选出来的行要带着能继续往下走的东西：服务层拿 payment_method 解渠道、拿 payment_no 与
	// order_no 拼查单报文、拿 request_id 做幂等。少一格，服务层那一轮就是空转。
	var picked *model.Payment
	for i := range payments {
		if payments[i].PaymentNo == stale.paymentNo {
			picked = &payments[i]
		}
	}
	if picked == nil {
		t.Fatal("选中的那一张没在结果里")
	}
	if picked.PaymentMethod != catalog.CodeUMSH5Alipay {
		t.Errorf("选中的行 payment_method = %q，want %q", picked.PaymentMethod, catalog.CodeUMSH5Alipay)
	}
	if picked.Amount != 1980 {
		t.Errorf("选中的行 amount = %d，want 1980", picked.Amount)
	}
	if picked.Provider != catalog.ChannelCodeUMS {
		t.Errorf("选中的行 provider = %q，want %q", picked.Provider, catalog.ChannelCodeUMS)
	}
}

// TestPostgresListStalePendingPaymentsRespectsTheLimit 批量必须是真的批量。
//
// 这个数字管的是**每一轮向下游发的请求数**：查单要出网，一轮放开就等于把积压一次全打给渠道，
// 而渠道那边看到的是瞬时并发。写错的表现是平时看不出来、积压那天才炸。
//
// 与上面那条一样，不断言「恰好 N 张」：这个库里本来就有别的 pending 行，够 limit 用。
func TestPostgresListStalePendingPaymentsRespectsTheLimit(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())

	now := time.Now().UTC()
	// 三张，都远超窗口，保证候选不少于三张。
	newStalePaymentFixture(t, pool, model.PaymentPending, catalog.ChannelCodeUMS, now.Add(-3*time.Hour))
	newStalePaymentFixture(t, pool, model.PaymentPending, catalog.ChannelCodeUMS, now.Add(-2*time.Hour))
	newStalePaymentFixture(t, pool, model.PaymentPending, catalog.ChannelCodeUMS, now.Add(-time.Hour))

	payments, err := repo.ListStalePendingPayments(context.Background(), now.Add(-time.Minute), 2)
	if err != nil {
		t.Fatalf("ListStalePendingPayments: %v", err)
	}
	if len(payments) > 2 {
		t.Fatalf("limit=2 却取回了 %d 行——一轮会把积压一次全打给渠道", len(payments))
	}
}

// TestPostgresSettlePaymentWithoutANotification 钉住这一整条改动的**承重墙**：查单那条路上
// 的结算是复用了回调那条路的 SettlePayment，而它手上**没有一条回调记录**可以标。
//
// 承重的意思是：查单问到「钱收到了」之后，落到账上的东西必须与回调收到时一模一样——同一个
// 事务、同一套锁与金额校验、同一行资金流水、同一条 outbox 事件。如果做不到，就只能另写一条
// 结算路径，而两条路径一旦分岔，对账时会看到同一件事有两种记法。
//
// 它能做到的依据是 markNotificationInTx 在 id 为空时直接返回（见那段注释），而**那个依据
// 只有真库验得动**：假仓储上「传了空 id」是一个断言，真库上「空 id 不炸、而且整套写入照常
// 提交」才是事实。传给它的每一个检查（锁、渠道一致性、金额、出资行）都在这一条 SQL 里。
func TestPostgresSettlePaymentWithoutANotification(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	now := time.Now().UTC()
	fixture := newStalePaymentFixture(t, pool, model.PaymentPending, catalog.ChannelCodeUMS, now)
	// 建单那一步必写的预占出资行。真行长这样，夹具就得长这样：少了它，下面那条资金流水会走
	// 「没有出资行」的兜底分支，而被验的就不是正常路径了。
	//
	// line_type 取上面那张单的 payment_method（出资渠道那套词表已经退场，两处今天存的是同一个
	// 值）：随手写个字面量会让夹具比真行多出一种状态。
	if _, err := pool.Exec(ctx, `INSERT INTO payment_fundings
		(payment_id, line_no, line_type, amount, status) VALUES ($1,1,$2,1980,'reserved')`,
		fixture.paymentID, catalog.CodeUMSH5Alipay); err != nil {
		t.Fatalf("insert funding: %v", err)
	}

	// 与 service.settleReconciledPayment 传的逐字一致：NotificationID 空。
	settlement, err := repo.SettlePayment(ctx, SettleNotificationParams{
		PaymentNo:             fixture.paymentNo,
		Provider:              catalog.ChannelCodeUMS,
		Succeeded:             true,
		Amount:                1980,
		ProviderTransactionID: "TXN-REC-1",
	})
	if err != nil {
		t.Fatalf("没有回调记录的结算必须能落库，却报错：%v——查单那条路就整条哑了", err)
	}
	if settlement.AlreadySettled {
		t.Fatal("刚从 pending 结出来的单被系统认成「早就结过了」")
	}
	if settlement.Payment.Status != model.PaymentSucceeded {
		t.Fatalf("结算后状态 = %q, want %q", settlement.Payment.Status, model.PaymentSucceeded)
	}

	// 落在那张单上的东西。渠道交易号与成交时间缺一个，退款与对账都反查不回来。
	var status, txnID string
	var paidAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, provider_transaction_id, paid_at
		FROM payments WHERE id=$1`, fixture.paymentID).Scan(&status, &txnID, &paidAt); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if status != model.PaymentSucceeded || txnID != "TXN-REC-1" || paidAt == nil {
		t.Fatalf("落库结果 = status %q / txn %q / paid_at %v，want succeeded / TXN-REC-1 / 非空",
			status, txnID, paidAt)
	}

	// 资金流水是不可变的对账基准：查单收下的钱与回调收下的钱在账上必须长得一样。
	var kind, direction, provider string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT kind, direction, amount, provider
		FROM payment_transactions WHERE payment_no=$1`, fixture.paymentNo).
		Scan(&kind, &direction, &amount, &provider); err != nil {
		t.Fatalf("查单结出来的支付没有资金流水：%v", err)
	}
	if kind != "payment" || direction != "in" || amount != 1980 || provider != catalog.ChannelCodeUMS {
		t.Fatalf("资金流水 = %s/%s/%d/%s，want payment/in/1980/%s",
			kind, direction, amount, provider, catalog.ChannelCodeUMS)
	}

	// outbox 事件也在同一个事务里。少了它，支付单成了而订单永远停在待支付——这正是 outbox
	// 而不是「改完再发」的理由，查单这条路上同样成立。
	// payload 是 bytea（见 messaging 那两张表的列定义），先转文本再当 jsonb 读。
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_outbox
		WHERE event_type='payment.succeeded'
			AND convert_from(payload,'UTF8')::jsonb->>'paymentNo'=$1`,
		fixture.paymentNo).Scan(&events); err != nil {
		t.Fatalf("查 outbox: %v", err)
	}
	if events != 1 {
		t.Fatalf("payment.succeeded 事件 = %d 条，want 1（查单收的钱也要把订单推走）", events)
	}

	// 再来一次：这一笔已经是 succeeded 了，同一个结论落在同一个状态上。空 id 那条路在**这个
	// 分支**里也要是空操作，否则每一轮扫到的重复结论都会在这里炸一次。
	again, err := repo.SettlePayment(ctx, SettleNotificationParams{
		PaymentNo: fixture.paymentNo, Provider: catalog.ChannelCodeUMS,
		Succeeded: true, Amount: 1980, ProviderTransactionID: "TXN-REC-1",
	})
	if err != nil {
		t.Fatalf("同一结论来第二次报错：%v", err)
	}
	if !again.AlreadySettled {
		t.Fatal("第二次结算没有被认成「早就结过了」")
	}
}
