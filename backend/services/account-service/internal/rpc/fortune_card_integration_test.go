package rpc

import (
	"context"
	"net"
	"os"
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
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
)

// 这一组用例打的是真的 gRPC 面：真的监听、真的走 auth 拦截器、真的落库。三个 RPC 今天
// 都还没有调用方，所以这是它们唯一被执行到的地方——等 lottery 落地再验就太晚了，
// 「没有调用方」不是「不用验」。
//
// 用真 TCP 而不是 bufconn：这条路径上有一半要验的东西（服务令牌过 metadata 进来、
// 拦截器在链条上的位置）与传输无关，但真监听能顺带证明装配是对的——kgrpc.Listener
// 与 GRPCServerOption 在真服务里就是这么拼的。
const (
	testServiceToken = "account-service-integration-test-token-0123456789"
	testJWTSecret    = "account-service-integration-test-jwt-secret-0123456789"
)

func accountIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("ACCOUNT_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set ACCOUNT_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("account test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("account test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newFortuneCardClient 起一个真的 gRPC 服务端，返回一个连着它的客户端。
//
// 服务端装的是与 cmd/main.go 完全一样的两样东西：同一个 rpc.FortuneCardService 实现、
// 同一个 auth.GRPCServerOption。所以「令牌不对会被拒」在这里验到的就是线上的那一条。
func newFortuneCardClient(t *testing.T) accountv1.FortuneCardServiceClient {
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
	accountv1.RegisterFortuneCardServiceServer(srv, NewFortuneCardService(accounts))

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
	return accountv1.NewFortuneCardServiceClient(conn)
}

// serviceCtx 是「代表基础设施」的调用上下文：带共享服务令牌，不带任何用户身份。
func serviceCtx() context.Context {
	return auth.WithServiceToken(context.Background(), testServiceToken)
}

// seedGrant 直接写一笔发放，省掉「先造一单」的前置——订单完成那条路在 service 的用例里
// 已经验过，这里要的是账户里先有几张可扣。
func seedGrant(t *testing.T, pool *pgxpool.Pool, userID string, amount int64) repository.EntryResult {
	t.Helper()
	orderID := uuid.NewString()
	results, err := repository.NewPostgresRepository(pool).GrantOrderFortune(context.Background(), repository.GrantParams{
		UserID:     userID,
		OrderID:    orderID,
		OrderNo:    "CO-RPC-" + orderID[:8],
		OccurredAt: time.Now().UTC(),
		Lines: []repository.GrantLine{{
			Title: "订单完成赠送", Amount: amount, EntryKey: model.BaseGrantKey(orderID),
		}},
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	return results[0]
}

func TestFortuneCardServiceRequiresTheServiceToken(t *testing.T) {
	client := newFortuneCardClient(t)

	// 不带任何凭据：拦截器给 Unauthenticated。
	if _, err := client.GetFortuneCardBalance(context.Background(), &accountv1.GetFortuneCardBalanceRequest{
		UserId: uuid.NewString(),
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated without credentials, got %v", err)
	}

	// 带一个错的服务令牌，更要拒：这是内部面，猜对的成本必须高到不可能是运气。
	if _, err := client.GetFortuneCardBalance(auth.WithServiceToken(context.Background(), "wrong-token"), &accountv1.GetFortuneCardBalanceRequest{
		UserId: uuid.NewString(),
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated with a wrong token, got %v", err)
	}
}

func TestGetFortuneCardBalanceAnswersZeroBeforeTheFirstGrant(t *testing.T) {
	client := newFortuneCardClient(t)

	// 从没收到过福卡的人问余额：答案是 0 张这件事本身，不是 NotFound。抽奖在扣减之前
	// 先看一眼，拿到 0 才好说那句「福卡余额不足」。
	response, err := client.GetFortuneCardBalance(serviceCtx(), &accountv1.GetFortuneCardBalanceRequest{
		UserId: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if response.GetBalance() != 0 {
		t.Fatalf("want balance 0, got %d", response.GetBalance())
	}
}

func TestDeductFortuneCardsOverGRPC(t *testing.T) {
	client := newFortuneCardClient(t)
	pool := accountIntegrationPool(t)
	userID := uuid.NewString()
	seedGrant(t, pool, userID, 2)

	request := &accountv1.DeductFortuneCardsRequest{
		UserId:    userID,
		Amount:    1,
		RequestId: "participation-" + uuid.NewString(),
		Title:     "参与抽奖",
	}
	first, err := client.DeductFortuneCards(serviceCtx(), request)
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}
	if first.GetBalanceAfter() != 1 || first.GetEntryId() == "" || first.GetReplayed() {
		t.Fatalf("unexpected deduct response: %+v", first)
	}

	// 同一个 request_id 重放：拿回同一笔，replayed 为真，余额不再动。调用方超时重试
	// 走的就是这一条，它决定了「一次网络抖动会不会白扣用户一张福卡」。
	replay, err := client.DeductFortuneCards(serviceCtx(), request)
	if err != nil {
		t.Fatalf("replay deduct: %v", err)
	}
	if !replay.GetReplayed() || replay.GetEntryId() != first.GetEntryId() || replay.GetBalanceAfter() != first.GetBalanceAfter() {
		t.Fatalf("a replay must return the first response: %+v vs %+v", replay, first)
	}
	balance, err := client.GetFortuneCardBalance(serviceCtx(), &accountv1.GetFortuneCardBalanceRequest{UserId: userID})
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if balance.GetBalance() != 1 {
		t.Fatalf("a replayed deduct must not deduct twice, got balance %d", balance.GetBalance())
	}

	// 余额不足：FailedPrecondition，不是 Internal。「用户没钱」与「服务坏了」混成一个码，
	// 前者就会变成一条告警，而调用方也没法把它翻成给用户看的那句话。
	if _, err := client.DeductFortuneCards(serviceCtx(), &accountv1.DeductFortuneCardsRequest{
		UserId:    userID,
		Amount:    5,
		RequestId: "participation-" + uuid.NewString(),
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for an overdraft, got %v", err)
	}

	// 参数不合法：InvalidArgument。没有 request_id 就没有重放这回事，不能收下。
	if _, err := client.DeductFortuneCards(serviceCtx(), &accountv1.DeductFortuneCardsRequest{
		UserId: userID,
		Amount: 1,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument without a request id, got %v", err)
	}
}

func TestReverseFortuneCardEntryOverGRPC(t *testing.T) {
	client := newFortuneCardClient(t)
	pool := accountIntegrationPool(t)
	userID := uuid.NewString()
	granted := seedGrant(t, pool, userID, 1)

	first, err := client.ReverseFortuneCardEntry(serviceCtx(), &accountv1.ReverseFortuneCardEntryRequest{
		EntryId: granted.EntryID,
		Title:   "退款追回",
	})
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if first.GetBalanceAfter() != 0 || first.GetReplayed() {
		t.Fatalf("unexpected reverse response: %+v", first)
	}

	// 重放同一笔冲正：回放已有那笔，余额不再动。
	replay, err := client.ReverseFortuneCardEntry(serviceCtx(), &accountv1.ReverseFortuneCardEntryRequest{
		EntryId: granted.EntryID,
	})
	if err != nil {
		t.Fatalf("replay reverse: %v", err)
	}
	if !replay.GetReplayed() || replay.GetEntryId() != first.GetEntryId() {
		t.Fatalf("a replayed reverse must return the first response: %+v vs %+v", replay, first)
	}

	// 冲一笔不存在的流水：NotFound。
	if _, err := client.ReverseFortuneCardEntry(serviceCtx(), &accountv1.ReverseFortuneCardEntryRequest{
		EntryId: uuid.NewString(),
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for an unknown entry, got %v", err)
	}

	// 已经抽过奖的福卡追不回来：FailedPrecondition，而且要把话说清楚——调用方（退款单）
	// 需要知道这是一条业务规则，而不是一次可以重试的故障。
	spentUser := uuid.NewString()
	spent := seedGrant(t, pool, spentUser, 1)
	if _, err := client.DeductFortuneCards(serviceCtx(), &accountv1.DeductFortuneCardsRequest{
		UserId: spentUser, Amount: 1, RequestId: "participation-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("deduct the seeded card: %v", err)
	}
	_, err = client.ReverseFortuneCardEntry(serviceCtx(), &accountv1.ReverseFortuneCardEntryRequest{
		EntryId: spent.EntryID,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for a spent card, got %v", err)
	}
	// 光有状态码不够：调用方要把这句话转述给用户，所以拒绝必须带上一句人话。
	// 断言「有一句话」而不是具体措辞——措辞会改，这条要求不会。
	if status.Convert(err).Message() == "" {
		t.Fatalf("a refused reversal must carry an explanation, got %v", err)
	}
}

// TestPreviewFortuneCardFreezeOverGRPC 走一整条真的：订单域的客户端（client.NewFortuneCardQuoter）
// 读的就是这两个字段，所以「答的两个数怎么过线」必须在真的 gRPC 面上验一次——字段名写错、
// 少注册一个方法、响应体是空的，都会让订单域那边退化成 503（它的客户端把空响应判成
// 「没问到」，见 client.FortuneCardQuoter.FreezeQuote）。这一刀之前没有调用方，正是这类
// 装配错误最容易活下来的地方。
//
// 三档与仓储那一组同构，这里只验「原样过线」与「不认识的人不是错误」。
func TestPreviewFortuneCardFreezeOverGRPC(t *testing.T) {
	client := newFortuneCardClient(t)
	pool := accountIntegrationPool(t)
	userID := uuid.NewString()
	orderID := uuid.NewString()
	baseKey := model.BaseGrantKey(orderID)

	// 从没收到过福卡的人：0/0，不是 NotFound。订单域拿这两个数去判「发放还没落库」，
	// 收到一个错误就变成一次 503——那会把「先申请退款、再完成订单」这条正常路径堵死。
	stranger, err := client.PreviewFortuneCardFreeze(serviceCtx(), &accountv1.PreviewFortuneCardFreezeRequest{
		UserId:    uuid.NewString(),
		EntryKeys: []string{baseKey},
	})
	if err != nil {
		t.Fatalf("preview for a user without an account: %v", err)
	}
	if stranger.GetGranted() != 0 || stranger.GetFreezable() != 0 {
		t.Fatalf("want 0/0 for a stranger, got %d/%d", stranger.GetGranted(), stranger.GetFreezable())
	}

	if _, err := repository.NewPostgresRepository(pool).GrantOrderFortune(context.Background(), repository.GrantParams{
		UserID:     userID,
		OrderID:    orderID,
		OrderNo:    "CO-RPC-PREVIEW-" + orderID[:8],
		OccurredAt: time.Now().UTC(),
		Lines:      []repository.GrantLine{{Title: "订单完成赠送", Amount: 2, EntryKey: baseKey}},
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	full, err := client.PreviewFortuneCardFreeze(serviceCtx(), &accountv1.PreviewFortuneCardFreezeRequest{
		UserId:    userID,
		EntryKeys: []string{baseKey},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if full.GetGranted() != 2 || full.GetFreezable() != 2 {
		t.Fatalf("want 2/2 before any draw, got %d/%d", full.GetGranted(), full.GetFreezable())
	}

	// 抽掉一张：两个数开始分家。只回一个数的实现会在这里露馅。
	if _, err := client.DeductFortuneCards(serviceCtx(), &accountv1.DeductFortuneCardsRequest{
		UserId: userID, Amount: 1, RequestId: "participation-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("deduct: %v", err)
	}
	spent, err := client.PreviewFortuneCardFreeze(serviceCtx(), &accountv1.PreviewFortuneCardFreezeRequest{
		UserId:    userID,
		EntryKeys: []string{baseKey},
	})
	if err != nil {
		t.Fatalf("preview after a draw: %v", err)
	}
	if spent.GetGranted() != 2 || spent.GetFreezable() != 1 {
		t.Fatalf("want 2/1 after spending one card, got %d/%d", spent.GetGranted(), spent.GetFreezable())
	}
}
