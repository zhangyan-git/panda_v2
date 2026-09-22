package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// FindPaymentByNo 是**渠道流水号唯一的出口**：membership-service 拿订单上的支付单号换它，渲染
// 后台「首月支付信息」里那一格微信流水（见 service.GetPayment 与 GetPayment 那条 RPC）。
//
// 它自己是一条老读路径（回调、幂等回放、退款都在用），但此前**没有任何一条用例直接钉过它**——
// 那些调用方各自验的是它们拿这张单去做什么。这一条只问一件事：库里那一行的每一格，读出来还是
// 不是原来的那一格。中间的 `paymentColumns` 清单是唯一的失败点：SELECT 的列与 scanPayment 的
// 扫描顺序错位时，编译期不红、上层也看不出，读出来的是**隔壁列的值**——支付方式那一格会开始
// 显示金额，而渠道流水号会显示一个失败原因。
//
// 与其它集成用例同一个约定：只用一个调用方给的库 URL，夹具用随机 uuid，跑完删掉自己那份。
func TestIntegrationFindPaymentByNoReadsBackTheRow(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, nil)
	ctx := context.Background()

	paymentID := uuid.NewString()
	paymentNo := "PAY-QRY-" + uuid.NewString()
	orderNo := "ORD-QRY-" + uuid.NewString()
	transactionID := "WXTXN-QRY-" + paymentID
	paidAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)

	// 一行「成功且有渠道流水」的支付单。**不用上面那个 fundedPaymentFixture**：那一份是给账户
	// 出资（纯豆）造的，它的 provider 与 provider_transaction_id 都是空的——正是这一条用例要读的
	// 那两格。它还要 account_entry_id 指向一条真账变，与这里无关。
	_, err := pool.Exec(ctx, `INSERT INTO payments
		(id, payment_no, order_no, user_id, amount, provider, payment_method, status,
		 subject, provider_transaction_id, request_id, paid_at)
		VALUES ($1,$2,$3,$4,990,'ums','ums_h5_alipay','succeeded',
			'会员首月',$5,$6,$7)`,
		paymentID, paymentNo, orderNo, uuid.NewString(), transactionID, "req-"+paymentID, paidAt)
	if err != nil {
		t.Fatalf("插一张支付单失败：%v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM payments WHERE id=$1`, paymentID)
	})

	payment, err := repo.FindPaymentByNo(ctx, paymentNo)
	if err != nil {
		t.Fatalf("按支付单号读回失败：%v", err)
	}
	if payment.PaymentNo != paymentNo || payment.OrderNo != orderNo {
		t.Errorf("读回了别的单：payment_no=%q order_no=%q", payment.PaymentNo, payment.OrderNo)
	}
	// 这三格就是 GetPayment 那条 RPC 的全部理由。
	if payment.ProviderTransactionID != transactionID {
		t.Errorf("渠道流水号读成了 %q——它是这一整条链上唯一的对账凭据", payment.ProviderTransactionID)
	}
	if payment.Status != model.PaymentSucceeded {
		t.Errorf("状态读成了 %q，想要 %q", payment.Status, model.PaymentSucceeded)
	}
	if payment.Amount != 990 {
		t.Errorf("金额读成了 %d，想要 990", payment.Amount)
	}
	if payment.PaymentMethod != "ums_h5_alipay" {
		t.Errorf("支付方式读成了 %q，想要 ums_h5_alipay", payment.PaymentMethod)
	}
	if payment.PaidAt == nil || !payment.PaidAt.Equal(paidAt) {
		t.Errorf("支付时间读成了 %v，想要 %v", payment.PaidAt, paidAt)
	}

	// 查无此单回 ErrPaymentNotFound，回 nil 单。这条分档决定上层把它读成 NotFound 还是别的
	// （见 rpc.serviceError）——回一个零值单会让调用方拿着一张空单继续往下走。
	missing, err := repo.FindPaymentByNo(ctx, "PAY-NOT-"+uuid.NewString())
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("读一张不存在的支付单回的是 %v，想要 ErrPaymentNotFound", err)
	}
	if missing != nil {
		t.Errorf("查无此单却回了一张单：%+v", missing)
	}

	// 未支付成功的那几格是 NULL / 空串，读出来不能变成零值时间。续费单这条路上没有支付单，
	// 而首月那一段今天恒为空——正是「读一张还没成功的单」这个形状。
	pendingID := uuid.NewString()
	pendingNo := "PAY-QRY-PENDING-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO payments
		(id, payment_no, order_no, user_id, amount, payment_method, status, request_id)
		VALUES ($1,$2,$3,$4,990,'ums_h5_alipay','pending',$5)`,
		pendingID, pendingNo, "ORD-QRY-"+uuid.NewString(), uuid.NewString(), "req-"+pendingID); err != nil {
		t.Fatalf("插一张未支付单失败：%v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM payments WHERE id=$1`, pendingID)
	})

	pending, err := repo.FindPaymentByNo(ctx, pendingNo)
	if err != nil {
		t.Fatalf("读一张未支付单失败：%v", err)
	}
	if pending.PaidAt != nil {
		t.Errorf("未支付单却带着支付时间：%v", pending.PaidAt)
	}
	if pending.ProviderTransactionID != "" {
		t.Errorf("未支付单却带着渠道流水号：%q", pending.ProviderTransactionID)
	}
}
