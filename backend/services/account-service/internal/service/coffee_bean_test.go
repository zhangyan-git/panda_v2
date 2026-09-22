package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// 这一层要验的是本服务**拥有**的规则：形状校验、幂等键怎么拼、负数在哪一边被允许、分页兜底。
// 与 SQL 有关的幂等与余额不变式在 repository 的集成测试里验（见 coffee_bean_integration_test.go）。

// stubBeanAdmin 是后台调整那条写路径的桩。它与 stubRepository 分开，因为服务层拿的是另一个
// 接口（BeanAdminRepository）——那正是「读路径不该为一个用不到的 recorder 多传参数」的形状。
type stubBeanAdmin struct {
	params []repository.BeanAdjustParams
	result repository.EntryResult
	err    error
}

func (s *stubBeanAdmin) AdjustBeans(_ context.Context, params repository.BeanAdjustParams) (repository.EntryResult, error) {
	s.params = append(s.params, params)
	return s.result, s.err
}

func TestBeanKeysAreDerivedFromTheirBusinessIDs(t *testing.T) {
	// 三把键都由「业务上是谁」拼出来，而不是调用方直接给——这一点是这三条路径能被重放的前提。
	// 拼错一个字符的后果不是报错，而是**换了一把键**：重放会变成第二笔账。
	orderID := uuid.NewString()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"consume is keyed by order id", model.BeanConsumeKey(orderID), "order:" + orderID},
		{"reverse is keyed by after sale no", model.BeanReverseKey("AS-2026-1"), "after_sale:AS-2026-1"},
		{"adjust is keyed by request id", model.BeanAdjustKey("req-1"), "adjust:req-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, tc.got)
			}
		})
	}

	// 扣减那把键取的是**订单 ID 而不是支付单号**：冲正要从 order.after_sale.refunded 反查
	// 这笔扣减，而那条事件只带 orderId。用支付单号做键的话，这里必须多一个参数，
	// 而那个参数只能来自调用方——于是「同一张订单被扣两次」只差调用方发一次新的支付单。
	if model.BeanConsumeKey(orderID) == model.BeanReverseKey(orderID) {
		t.Fatal("consume and reverse keys must not collide for the same id")
	}
}

func TestConsumeBeansRejectsBadRequests(t *testing.T) {
	userID := uuid.NewString()
	orderID := uuid.NewString()
	cases := []struct {
		name    string
		req     BeanConsumeRequest
		wantErr error
	}{
		{
			name:    "user id is not a uuid",
			req:     BeanConsumeRequest{UserID: "nope", OrderID: orderID, Amount: 1},
			wantErr: ErrInvalidUserID,
		},
		{
			// 订单 ID 不只是「哪张订单」：扣减的幂等键由它派生。收下一个空的或不是 UUID 的
			// 订单 ID，等于把这次扣减挂在一把别人也拼得出来的键上。
			name:    "order id is missing",
			req:     BeanConsumeRequest{UserID: userID, Amount: 1},
			wantErr: ErrInvalidOrderID,
		},
		{
			name:    "order id is not a uuid",
			req:     BeanConsumeRequest{UserID: userID, OrderID: "ORD-1", Amount: 1},
			wantErr: ErrInvalidOrderID,
		},
		{
			// 负的扣减会被仓储内部取负变成一次充值，而余额涨了不违反任何约束——这是最难发现的
			// 一类错，所以服务层先挡住。
			name:    "amount is not positive",
			req:     BeanConsumeRequest{UserID: userID, OrderID: orderID, Amount: 0},
			wantErr: ErrInvalidAmount,
		},
		{
			name:    "amount is negative",
			req:     BeanConsumeRequest{UserID: userID, OrderID: orderID, Amount: -100},
			wantErr: ErrInvalidAmount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubRepository{}
			if _, err := New(repo).ConsumeBeans(context.Background(), tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if len(repo.consumeBeanParams) != 0 {
				t.Fatalf("a rejected consume must not reach the repository, got %d calls", len(repo.consumeBeanParams))
			}
		})
	}
}

func TestConsumeBeansPassesTrimmedIDsThrough(t *testing.T) {
	repo := &stubRepository{}
	userID := uuid.NewString()
	orderID := uuid.NewString()

	if _, err := New(repo).ConsumeBeans(context.Background(), BeanConsumeRequest{
		UserID:  " " + userID + " ",
		OrderID: " " + orderID + " ",
		OrderNo: " ORD-1 ",
		Amount:  3400,
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	params := repo.consumeBeanParams[0]
	// 客户端的空格会一路进到 SQL 与幂等键里：`order: <id>` 与 `order:<id>` 是两把键，
	// 重放因此会变成第二笔扣款。在这里 trim 掉，别让一个空格换一把键。
	if params.UserID != userID || params.OrderID != orderID || params.OrderNo != "ORD-1" {
		t.Fatalf("ids must be trimmed, got user=%q order=%q no=%q", params.UserID, params.OrderID, params.OrderNo)
	}
	// 服务交出去的是「这单用掉多少豆」，仍是正数：流水上记负数、以及键怎么拼，都归仓储。
	if params.Amount != 3400 {
		t.Fatalf("want amount 3400, got %d", params.Amount)
	}
	// 时间由服务层给（s.now），仓储不自己取：测试里那个时刻才是「发生了什么」的记录。
	if params.OccurredAt.IsZero() {
		t.Fatal("occurred at must be set by the service")
	}
}

func TestConsumeBeansPassesTheLedgerCopyThrough(t *testing.T) {
	// 标题与备注是**写给人看的**那一行：客服按支付单号反查一笔豆扣减时，能对上的就是它。
	// 它们不参与幂等键、也不影响金额，所以漏传的表现是完全静默的——扣减成功、余额正确、
	// 流水也在，只是那句解释永远不在上面。这条用例钉的就是这条传递链的服务层一段。
	repo := &stubRepository{}

	if _, err := New(repo).ConsumeBeans(context.Background(), BeanConsumeRequest{
		UserID:  uuid.NewString(),
		OrderID: uuid.NewString(),
		OrderNo: "ORD-1",
		Amount:  3400,
		Title:   " 咖啡豆支付 ",
		Remark:  " 支付单 PAY-1 ",
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	params := repo.consumeBeanParams[0]
	// 与 ID 同样 trim：这两个字段来自 gRPC 请求、由另一个服务拼出来，多一个空格既不报错
	// 也不影响业务，只会让后台那行文案带着空白显示。
	if params.Title != "咖啡豆支付" || params.Remark != "支付单 PAY-1" {
		t.Fatalf("the ledger copy must be passed through trimmed, got title=%q remark=%q", params.Title, params.Remark)
	}

	// 两个都可以留空：留空的语义由仓储兜（标题兜成「咖啡豆支付」），服务层不替它编一句，
	// 否则「调用方没给」与「调用方给了这一句」就再也分不开了。
	repo = &stubRepository{}
	if _, err := New(repo).ConsumeBeans(context.Background(), BeanConsumeRequest{
		UserID: uuid.NewString(), OrderID: uuid.NewString(), Amount: 100,
	}); err != nil {
		t.Fatalf("consume without copy: %v", err)
	}
	if got := repo.consumeBeanParams[0]; got.Title != "" || got.Remark != "" {
		t.Fatalf("a consume without copy must not invent one, got title=%q remark=%q", got.Title, got.Remark)
	}
}

func TestReverseBeansRejectsBadRequests(t *testing.T) {
	orderID := uuid.NewString()
	cases := []struct {
		name    string
		req     BeanReverseRequest
		wantErr error
	}{
		{
			name:    "order id is missing",
			req:     BeanReverseRequest{AfterSaleNo: "AS-1", Amount: 100},
			wantErr: ErrInvalidOrderID,
		},
		{
			// 售后单号是冲正的幂等键（after_sale:{no}）：没有它就没有「一条售后只冲一次」。
			name:    "after sale no is missing",
			req:     BeanReverseRequest{OrderID: orderID, Amount: 100},
			wantErr: ErrInvalidAfterSaleNo,
		},
		{
			name:    "amount is not positive",
			req:     BeanReverseRequest{OrderID: orderID, AfterSaleNo: "AS-1", Amount: 0},
			wantErr: ErrInvalidAmount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubRepository{}
			if _, err := New(repo).ReverseBeans(context.Background(), tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if len(repo.beanReverseParams) != 0 {
				t.Fatalf("a rejected reverse must not reach the repository, got %d calls", len(repo.beanReverseParams))
			}
		})
	}
}

func TestReverseBeansReportsWhetherSomethingWasReversed(t *testing.T) {
	orderID := uuid.NewString()

	// 渠道支付的订单走到这里：仓储找不到那笔扣减，什么都没做。这个 false 不是错误，
	// 事件侧靠它区分「退过了」与「这单根本没用豆付过」。
	repo := &stubRepository{beanReverseResult: false}
	reversed, err := New(repo).ReverseBeans(context.Background(), BeanReverseRequest{
		OrderID: orderID, AfterSaleID: uuid.NewString(), AfterSaleNo: "AS-1", Amount: 900,
	})
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if reversed {
		t.Fatal("no-op reverse must report false")
	}

	// 真的冲了一笔。部分退款的金额由调用方给（这里 900 小于整单），服务不整单照抄——
	// 上限由仓储按「这笔扣减还没冲回的部分」卡。
	repo = &stubRepository{beanReverseResult: true}
	reversed, err = New(repo).ReverseBeans(context.Background(), BeanReverseRequest{
		OrderID: orderID, AfterSaleID: uuid.NewString(), AfterSaleNo: "AS-2", Amount: 900,
	})
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if !reversed {
		t.Fatal("a real reversal must report true")
	}
	if got := repo.beanReverseParams[0].Amount; got != 900 {
		t.Fatalf("want amount 900, got %d", got)
	}
}

func TestBeanAccountIsZeroNotAnErrorWhenThereIsNoRow(t *testing.T) {
	userID := uuid.NewString()
	repo := &stubRepository{} // 零值 = 没有账户行

	account, err := New(repo).BeanAccount(context.Background(), userID)
	if err != nil {
		t.Fatalf("a user without an account row is not an error: %v", err)
	}
	if account.Balance != 0 || account.HasAccount {
		t.Fatalf("want balance 0 and hasAccount false, got %+v", account)
	}
	if account.UserID != userID {
		t.Fatalf("want the requested user id back, got %q", account.UserID)
	}

	// 有账户行时 hasAccount 才是 true。两者的余额都可能显示 0，界面靠这个区分
	// 「花光了」与「从来没有过」。
	repo = &stubRepository{beanAccount: &model.CoffeeBeanAccount{UserID: userID, Balance: 1600}}
	account, err = New(repo).BeanAccount(context.Background(), userID)
	if err != nil {
		t.Fatalf("bean account: %v", err)
	}
	if account.Balance != 1600 || !account.HasAccount {
		t.Fatalf("want balance 1600 and hasAccount true, got %+v", account)
	}

	if _, err := New(&stubRepository{}).BeanAccount(context.Background(), "nope"); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("want %v, got %v", ErrInvalidUserID, err)
	}
}

func TestBeanEntriesAppliesPagingDefaults(t *testing.T) {
	repo := &stubRepository{}
	svc := New(repo)

	// 0 的 pageSize 会让 SQL 的 LIMIT 变成 0：页面永远空着，而没有任何一处报错。
	if _, _, err := svc.BeanEntries(context.Background(), dto.BeanEntryQuery{}); err != nil {
		t.Fatalf("bean entries: %v", err)
	}
	if repo.beanEntriesQuery.Page != 1 || repo.beanEntriesQuery.PageSize != defaultPageSize {
		t.Fatalf("want page 1 size %d, got page %d size %d",
			defaultPageSize, repo.beanEntriesQuery.Page, repo.beanEntriesQuery.PageSize)
	}

	// 超过上限的 pageSize 会把整库拉出来。
	if _, _, err := svc.BeanEntries(context.Background(), dto.BeanEntryQuery{
		Page: 2, PageSize: dto.MaxPageSize + 1,
	}); err != nil {
		t.Fatalf("bean entries: %v", err)
	}
	if repo.beanEntriesQuery.PageSize != dto.MaxPageSize {
		t.Fatalf("want page size capped at %d, got %d", dto.MaxPageSize, repo.beanEntriesQuery.PageSize)
	}
	// 过滤器原样透传：它们是 SQL 的 WHERE 条件，服务层不解释。
	if _, _, err := svc.BeanEntries(context.Background(), dto.BeanEntryQuery{
		UserID: uuid.NewString(), ReferenceNo: "ORD-1", EntryType: model.BeanEntryTypeConsume,
	}); err != nil {
		t.Fatalf("bean entries: %v", err)
	}
	if repo.beanEntriesQuery.ReferenceNo != "ORD-1" ||
		repo.beanEntriesQuery.EntryType != model.BeanEntryTypeConsume {
		t.Fatalf("filters must pass through, got %+v", repo.beanEntriesQuery)
	}
}

func TestAdjustBeansRejectsBadRequests(t *testing.T) {
	userID := uuid.NewString()
	cases := []struct {
		name    string
		req     BeanAdjustRequest
		wantErr error
	}{
		{
			name:    "user id is not a uuid",
			req:     BeanAdjustRequest{UserID: "nope", Amount: 100, RequestID: "r-1"},
			wantErr: ErrInvalidUserID,
		},
		{
			// 幂等号必填这一条，在「这是人点的」这条路径上比别处更要紧：没有它，管理员的一次
			// 点击在超时重发之后就变成两次加钱。
			name:    "request id is missing",
			req:     BeanAdjustRequest{UserID: userID, Amount: 100},
			wantErr: ErrInvalidRequestID,
		},
		{
			name:    "request id is blank",
			req:     BeanAdjustRequest{UserID: userID, Amount: 100, RequestID: "   "},
			wantErr: ErrInvalidRequestID,
		},
		{
			// 调整 0 分不是一次调整，几乎总是手滑点了保存。注意**只有 0 被挡**：负数在这条
			// 路径上是合法的（把充错的豆调回来），见下一个用例。
			name:    "amount is zero",
			req:     BeanAdjustRequest{UserID: userID, Amount: 0, RequestID: "r-1"},
			wantErr: repository.ErrBeanAmountZero,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubBeanAdmin{}
			if _, err := NewBeanAdmin(repo).AdjustBeans(context.Background(), tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if len(repo.params) != 0 {
				t.Fatalf("a rejected adjustment must not reach the repository, got %d calls", len(repo.params))
			}
		})
	}
}

func TestAdjustBeansAllowsNegativeAmounts(t *testing.T) {
	repo := &stubBeanAdmin{}
	userID := uuid.NewString()

	// 与扣减那条路**相反**：这里是唯一允许负数的写路径——把充错的豆调回来只能靠它。
	// 挡掉负数的后果不是一个错，而是「充错了没法改」。
	if _, err := NewBeanAdmin(repo).AdjustBeans(context.Background(), BeanAdjustRequest{
		UserID:    userID,
		Amount:    -6000,
		RequestID: " req-1 ",
		Remark:    " 充错了 ",
		Operator:  " " + userID + " ",
	}); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	params := repo.params[0]
	if params.Amount != -6000 {
		t.Fatalf("want the signed amount -6000, got %d", params.Amount)
	}
	if params.RequestID != "req-1" {
		t.Fatalf("request id must be trimmed, got %q", params.RequestID)
	}
	// 操作人来自令牌（controller 从 auth.IdentityFromRequest 取），它同时落进流水的
	// operator_id 与审计的 actor_id，两处因此说的是同一个人。
	if params.Operator != userID {
		t.Fatalf("operator must be trimmed, got %q", params.Operator)
	}
	if params.OccurredAt.IsZero() {
		t.Fatal("occurred at must be set by the service")
	}
	if params.OccurredAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("occurred at looks wrong: %v", params.OccurredAt)
	}
}
