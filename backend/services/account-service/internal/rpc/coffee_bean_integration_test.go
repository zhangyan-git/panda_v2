package rpc

import (
	"context"
	"net"
	"testing"
	"time"

	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
)

// 与 fortune_card_integration_test.go 同一套做法（真监听、真拦截器、真落库），验的是豆这一面
// 独有的那条错误码界线：「用户没钱」与「服务坏了」必须是两个码。
//
// 名字与福卡那份不共用，两个 client 各起一个服务端：同一台服务端上同时注册两棵树当然可以，
// 但那样一条用例失败时得先想「是装配错了还是实现错了」。分开起，装配各自独立成证。

// newCoffeeBeanClient 起一个真的 gRPC 服务端，返回一个连着它的客户端。
//
// 装的是与 cmd/main.go 一样的两样东西：同一个 rpc.CoffeeBeanService 实现、同一个
// auth.GRPCServerOption。所以「令牌不对会被拒」在这里验到的就是线上的那一条。
func newCoffeeBeanClient(t *testing.T) (accountv1.CoffeeBeanServiceClient, *pgxpool.Pool) {
	t.Helper()
	pool := accountIntegrationPool(t)
	accounts := service.New(repository.NewPostgresRepository(pool))

	jwtService, err := auth.NewService([]byte(testJWTSecret), "panda-account-test", time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("init jwt: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := kgrpc.NewServer(
		kgrpc.Listener(lis),
		auth.GRPCServerOption(jwtService, testServiceToken),
	)
	accountv1.RegisterCoffeeBeanServiceServer(srv, NewCoffeeBeanService(accounts))

	serverCtx, stopServer := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Start(serverCtx)
	}()
	t.Cleanup(func() {
		stopServer()
		_ = srv.Stop(context.Background())
		<-done
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return accountv1.NewCoffeeBeanServiceClient(conn), pool
}

// seedBeans 直接写一笔后台调整，省掉「先造一单再支付」的前置——那条路在 service 与
// repository 的用例里已经验过，这里要的是账户里先有豆。
//
// 审计传 nil（等价于关掉）：这一组验的是 gRPC 面，审计链路不在这条断言里，而没有 outbox
// 的库也写不出审计行。
func seedBeans(t *testing.T, pool *pgxpool.Pool, userID string, amount int64) {
	t.Helper()
	if _, err := repository.NewBeanAdminRepository(repository.NewPostgresRepository(pool), nil).
		AdjustBeans(context.Background(), repository.BeanAdjustParams{
			UserID:     userID,
			Amount:     amount,
			RequestID:  "seed-" + uuid.NewString(),
			Remark:     "rpc 用例的前置",
			Operator:   uuid.NewString(),
			OccurredAt: time.Now().UTC(),
		}); err != nil {
		t.Fatalf("seed beans: %v", err)
	}
}

func TestCoffeeBeanServiceRequiresTheServiceToken(t *testing.T) {
	client, _ := newCoffeeBeanClient(t)

	// 不带任何凭据：拦截器给 Unauthenticated。
	if _, err := client.GetCoffeeBeanBalance(context.Background(), &accountv1.GetCoffeeBeanBalanceRequest{
		UserId: uuid.NewString(),
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated without credentials, got %v", err)
	}

	// 三个方法都要挡。只验一个的话，剩下两个可以漏装拦截器而没人发现——它们都是
	// 「能凭空改余额」的口子，一个都不能漏。
	if _, err := client.DeductCoffeeBeans(context.Background(), &accountv1.DeductCoffeeBeansRequest{
		UserId: uuid.NewString(), OrderId: uuid.NewString(), Amount: 100,
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated on deduct without credentials, got %v", err)
	}

	// 带一个错的服务令牌，更要拒：这是内部面，猜对的成本必须高到不可能是运气。
	if _, err := client.ReverseCoffeeBeanEntry(auth.WithServiceToken(context.Background(), "wrong-token"), &accountv1.ReverseCoffeeBeanEntryRequest{
		OrderId: uuid.NewString(), AfterSaleNo: "AS-1", Amount: 100,
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated with a wrong token, got %v", err)
	}
}

func TestGetCoffeeBeanBalanceAnswersZeroBeforeTheFirstEntry(t *testing.T) {
	client, pool := newCoffeeBeanClient(t)

	// 从没有过账户行的人问余额：答案是 0 分这件事本身，不是 NotFound。payment-service 在
	// 扣减之前先看一眼，拿到 0 才好说那句「咖啡豆余额不足」——一个 NotFound 在那里只能被
	// 翻成一次 5xx。
	userID := uuid.NewString()
	response, err := client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{
		UserId: userID,
	})
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if response.GetBalance() != 0 {
		t.Fatalf("want balance 0, got %d", response.GetBalance())
	}

	// 有豆时读到的就是那个数（单位分，不换算）。
	seedBeans(t, pool, userID, 5000)
	response, err = client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{
		UserId: userID,
	})
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if response.GetBalance() != 5000 {
		t.Fatalf("want balance 5000, got %d", response.GetBalance())
	}

	// 不是 UUID 的用户 ID：InvalidArgument。让它走到 SQL 的话，回的是一个约束/类型错误，
	// 而真正的原因是调用方把一个非用户 ID 当成用户 ID 传了。
	if _, err := client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{
		UserId: "nope",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a bad user id, got %v", err)
	}
}

func TestDeductCoffeeBeansOverGRPC(t *testing.T) {
	client, pool := newCoffeeBeanClient(t)
	userID := uuid.NewString()
	orderID := uuid.NewString()
	seedBeans(t, pool, userID, 5000)

	request := &accountv1.DeductCoffeeBeansRequest{
		UserId:  userID,
		OrderId: orderID,
		OrderNo: "CO-RPC-" + orderID[:8],
		Amount:  3400,
		// 文案字段：payment-service 就是这么填的（备注写支付单号）。它不参与幂等也不影响
		// 金额，所以漏在链路的哪一段都不会报错——只会在后台那行流水上少一句话。
		Title:  "咖啡豆支付",
		Remark: "支付单 PAY-RPC-1",
	}
	first, err := client.DeductCoffeeBeans(serviceCtx(), request)
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}
	if first.GetBalanceAfter() != 1600 || first.GetEntryId() == "" || first.GetReplayed() {
		t.Fatalf("unexpected deduct response: %+v", first)
	}

	// 文案真的落到了流水上。从 gRPC 请求一路读到库里的那一列：这条断言跨过的正是
	// 「proto 上有、某一层不读」最容易断掉的那几跳，只验到服务层是抓不到的。
	var title, remark string
	if err := pool.QueryRow(context.Background(),
		`SELECT title, remark FROM coffee_bean_entries WHERE id = $1`, first.GetEntryId()).Scan(&title, &remark); err != nil {
		t.Fatalf("read the entry back: %v", err)
	}
	if title != "咖啡豆支付" || remark != "支付单 PAY-RPC-1" {
		t.Fatalf("the ledger copy must survive the whole chain, got title=%q remark=%q", title, remark)
	}

	// 同一个订单重放：拿回同一笔，replayed 为真，余额不再动。payment-service 在
	// 「扣了但事务没落」之后拿同一个订单重试走的就是这一条，它决定了会不会扣第二次。
	replay, err := client.DeductCoffeeBeans(serviceCtx(), request)
	if err != nil {
		t.Fatalf("replay deduct: %v", err)
	}
	if !replay.GetReplayed() || replay.GetEntryId() != first.GetEntryId() || replay.GetBalanceAfter() != first.GetBalanceAfter() {
		t.Fatalf("a replay must return the first response: %+v vs %+v", replay, first)
	}
	balance, err := client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{UserId: userID})
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if balance.GetBalance() != 1600 {
		t.Fatalf("a replayed deduct must not deduct twice, got balance %d", balance.GetBalance())
	}

	// 余额不足：FailedPrecondition 而不是 Internal，而且**带一句人话**。payment-service
	// 要把这句话转成 failureMessage 落进支付单，光有码它只能自己编一句。
	_, err = client.DeductCoffeeBeans(serviceCtx(), &accountv1.DeductCoffeeBeansRequest{
		UserId: userID, OrderId: uuid.NewString(), Amount: 9999,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for insufficient beans, got %v", err)
	}
	if status.Convert(err).Message() == "" {
		t.Fatal("a refused deduction must carry an explanation for the user")
	}

	// 没有订单 ID：InvalidArgument。订单 ID 是幂等键的来源，收下一个空的等于把这次扣减
	// 挂在别人也拼得出来的键上。
	if _, err := client.DeductCoffeeBeans(serviceCtx(), &accountv1.DeductCoffeeBeansRequest{
		UserId: userID, Amount: 100,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument without an order id, got %v", err)
	}

	// 0 分：一样是 InvalidArgument（服务层的形状校验先挡），不是「扣了个寂寞」。
	if _, err := client.DeductCoffeeBeans(serviceCtx(), &accountv1.DeductCoffeeBeansRequest{
		UserId: userID, OrderId: uuid.NewString(), Amount: 0,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a zero amount, got %v", err)
	}
}

func TestReverseCoffeeBeanEntryOverGRPC(t *testing.T) {
	client, pool := newCoffeeBeanClient(t)
	userID := uuid.NewString()
	orderID := uuid.NewString()
	seedBeans(t, pool, userID, 5000)

	if _, err := client.DeductCoffeeBeans(serviceCtx(), &accountv1.DeductCoffeeBeansRequest{
		UserId: userID, OrderId: orderID, OrderNo: "CO-RPC-" + orderID[:8], Amount: 3400,
	}); err != nil {
		t.Fatalf("deduct: %v", err)
	}

	// 先退加购行：部分冲正由调用方给金额，服务不整单照抄。
	afterSaleNo := "AS-" + uuid.NewString()[:8]
	first, err := client.ReverseCoffeeBeanEntry(serviceCtx(), &accountv1.ReverseCoffeeBeanEntryRequest{
		OrderId: orderID, AfterSaleId: uuid.NewString(), AfterSaleNo: afterSaleNo, Amount: 900,
	})
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if !first.GetReversed() {
		t.Fatal("a partial reversal must report true")
	}

	// 重投同一条售后：reversed=false 且**不是错误**。这两种情况（退过了 / 这单没用豆付过）
	// 调用方要做的都是同一件事——什么都不做；做成错误会让一条正常的事件推进重试链。
	replay, err := client.ReverseCoffeeBeanEntry(serviceCtx(), &accountv1.ReverseCoffeeBeanEntryRequest{
		OrderId: orderID, AfterSaleId: uuid.NewString(), AfterSaleNo: afterSaleNo, Amount: 900,
	})
	if err != nil {
		t.Fatalf("a replayed after sale must not be an error: %v", err)
	}
	if replay.GetReversed() {
		t.Fatal("a replayed after sale must report false")
	}
	balance, err := client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{UserId: userID})
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if balance.GetBalance() != 2500 {
		t.Fatalf("want balance 2500 after a 900 refund, got %d", balance.GetBalance())
	}

	// 渠道支付的订单走到这里：那笔订单根本没有豆扣减。同样是「什么都不做」，同样不报错
	// ——这条路径天天会被走到，报错会把正常事件一路重试到死信。
	noop, err := client.ReverseCoffeeBeanEntry(serviceCtx(), &accountv1.ReverseCoffeeBeanEntryRequest{
		OrderId: uuid.NewString(), AfterSaleId: uuid.NewString(),
		AfterSaleNo: "AS-" + uuid.NewString()[:8], Amount: 900,
	})
	if err != nil {
		t.Fatalf("a reverse without a deduction must not be an error: %v", err)
	}
	if noop.GetReversed() {
		t.Fatal("a reverse without a deduction must report false")
	}
	// 而且不能凭空造豆：这个用户的余额必须还是冲正后的那个数（冲正不带 userID，回谁的钱
	// 只由那笔扣减记录说了算，所以「找不到扣减就什么都不发生」等于不动任何人的余额）。
	if got, err := client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{
		UserId: userID,
	}); err != nil || got.GetBalance() != 2500 {
		t.Fatalf("a no-op reverse must not move the balance: %v %+v", err, got)
	}

	// 剩余可冲额是 3400-900=2500，要 3000 是超退：FailedPrecondition 而不是静默钳制。
	// 订单域已经按「实付 - 已退 - 在途」钳过一次，真撞到说明有一处算错了。
	_, err = client.ReverseCoffeeBeanEntry(serviceCtx(), &accountv1.ReverseCoffeeBeanEntryRequest{
		OrderId: orderID, AfterSaleId: uuid.NewString(),
		AfterSaleNo: "AS-" + uuid.NewString()[:8], Amount: 3000,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for an over-refund, got %v", err)
	}
	if status.Convert(err).Message() == "" {
		t.Fatal("a refused reversal must carry an explanation")
	}
	if got, err := client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{
		UserId: userID,
	}); err != nil || got.GetBalance() != 2500 {
		t.Fatalf("a refused reversal must not move the balance: %v %+v", err, got)
	}

	// 把剩下的 2500 冲干净，然后再重投**这一条**：冲干净之后剩余可冲是 0，若幂等查排在
	// 上限判定之后，这次重投会得到 FailedPrecondition——而那正是 relay 重投一条正常事件时
	// 最不该发生的事（它会把这条事件一路重试到死信）。所以这里断言的是 no-op 而不是报错。
	finalSale := "AS-" + uuid.NewString()[:8]
	if _, err := client.ReverseCoffeeBeanEntry(serviceCtx(), &accountv1.ReverseCoffeeBeanEntryRequest{
		OrderId: orderID, AfterSaleId: uuid.NewString(), AfterSaleNo: finalSale, Amount: 2500,
	}); err != nil {
		t.Fatalf("reverse the rest: %v", err)
	}
	replayFull, err := client.ReverseCoffeeBeanEntry(serviceCtx(), &accountv1.ReverseCoffeeBeanEntryRequest{
		OrderId: orderID, AfterSaleId: uuid.NewString(), AfterSaleNo: finalSale, Amount: 2500,
	})
	if err != nil {
		t.Fatalf("a replay of a fully reversed after sale must not be an error: %v", err)
	}
	if replayFull.GetReversed() {
		t.Fatal("a replay of a fully reversed after sale must report false")
	}
	if got, err := client.GetCoffeeBeanBalance(serviceCtx(), &accountv1.GetCoffeeBeanBalanceRequest{
		UserId: userID,
	}); err != nil || got.GetBalance() != 5000 {
		t.Fatalf("a replay must not move the balance: %v %+v", err, got)
	}
}
