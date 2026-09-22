package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

type issueBatchMock struct {
	called bool
	result *IssueResult
	err    error
}

func (m *issueBatchMock) ReserveInventory(context.Context, string, int64) error { return nil }
func (m *issueBatchMock) IssueCoupons(context.Context, string, string, string, dto.IssueCouponsRequest) (*IssueResult, error) {
	m.called = true
	return m.result, m.err
}

type couponMock struct{}

func (couponMock) Redeem(context.Context, string, string, string) (*model.UserCoupon, error) {
	return nil, nil
}

type idemMock struct {
	key *model.IdempotencyKey
	err error
}

func (idemMock) Create(context.Context, *model.IdempotencyKey) (bool, error) { return false, nil }
func (m idemMock) Find(context.Context, string, string) (*model.IdempotencyKey, error) {
	return m.key, m.err
}

func TestIssueCouponsValidatesRequest(t *testing.T) {
	m := &issueBatchMock{}
	s := New(m, couponMock{}, idemMock{})
	_, err := s.IssueCoupons(context.Background(), "", "actor", dto.IssueCouponsRequest{TemplateID: "template", UserIDs: []string{"user"}, QuantityPerUser: 1})
	if !errors.Is(err, ErrInvalidIssueRequest) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if m.called {
		t.Fatal("repository called for invalid request")
	}
}

func TestIssueCouponsRejectsBlankUserIDWithoutDispatching(t *testing.T) {
	m := &issueBatchMock{}
	s := New(m, couponMock{}, idemMock{})
	_, err := s.IssueCoupons(context.Background(), "key", "actor", dto.IssueCouponsRequest{TemplateID: "template", UserIDs: []string{" "}, QuantityPerUser: 1})
	if !errors.Is(err, ErrInvalidIssueRequest) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if m.called {
		t.Fatal("repository called for invalid user ID")
	}
}

func TestIssueCouponsReturnsCachedIdempotentResponse(t *testing.T) {
	want := &IssueResult{BatchID: "batch", IssuedQuantity: 1, UserCouponIDs: []string{"coupon"}}
	response, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	req := dto.IssueCouponsRequest{TemplateID: "template", UserIDs: []string{"user"}, QuantityPerUser: 1}
	m := &issueBatchMock{result: &IssueResult{BatchID: "unexpected"}}
	// 用旧格式哈希喂进去：这条用例因此同时覆盖「重放走缓存」和「改名之前
	// 建好的 key 在新代码里仍然命中」，后者正是改 tag 最容易踩坏的地方。
	s := New(m, couponMock{}, idemMock{key: &model.IdempotencyKey{RequestHash: legacySnakeCaseIssueHash(req), Response: response}})
	got, err := s.IssueCoupons(context.Background(), "key", "actor", req)
	if err != nil {
		t.Fatalf("IssueCoupons() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IssueCoupons() = %#v, want %#v", got, want)
	}
	if m.called {
		t.Fatal("repository called for cached idempotent request")
	}
}

func TestIssueCouponsRejectsIdempotencyHashConflict(t *testing.T) {
	req := dto.IssueCouponsRequest{TemplateID: "template", UserIDs: []string{"user"}, QuantityPerUser: 1}
	s := New(&issueBatchMock{}, couponMock{}, idemMock{key: &model.IdempotencyKey{RequestHash: "different"}})
	_, err := s.IssueCoupons(context.Background(), "key", "actor", req)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("IssueCoupons() error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestIssueCouponsNormalizesUserIDsBeforeHashingAndDispatch(t *testing.T) {
	m := &issueBatchMock{}
	s := New(m, couponMock{}, idemMock{})
	_, err := s.IssueCoupons(context.Background(), "key", "actor", dto.IssueCouponsRequest{TemplateID: "template", UserIDs: []string{" user ", "user"}, QuantityPerUser: 1})
	if err != nil {
		t.Fatalf("IssueCoupons() error = %v", err)
	}
	if !m.called {
		t.Fatal("repository was not called")
	}
}

func TestIssueCouponsPropagatesIdempotencyLookupError(t *testing.T) {
	lookupErr := errors.New("lookup failed")
	s := New(&issueBatchMock{}, couponMock{}, idemMock{err: lookupErr})
	_, err := s.IssueCoupons(context.Background(), "key", "actor", dto.IssueCouponsRequest{TemplateID: "template", UserIDs: []string{"user"}, QuantityPerUser: 1})
	if !errors.Is(err, lookupErr) {
		t.Fatalf("IssueCoupons() error = %v, want lookup error", err)
	}
}

// legacySnakeCaseIssueHash 重算「改名之前」的幂等哈希：那时候生产代码直接
// json.Marshal dto.IssueCouponsRequest，而它的 tag 还是 snake_case。
//
// 这里刻意手写字面量、不复用生产的 issueRequestHash，否则断言就成了同义反复
// ——它要证明的恰恰是「改名之前建好的 key，在新代码里仍然命中」。
// 字段顺序也必须与 dto.IssueCouponsRequest 的声明顺序一致：json.Marshal 按声明
// 顺序输出，换成 map 会因键排序而产出不同字节，哈希就对不上了。
func legacySnakeCaseIssueHash(req dto.IssueCouponsRequest) string {
	raw, _ := json.Marshal(struct {
		TemplateID          string   `json:"template_id"`
		UserIDs             []string `json:"user_ids"`
		QuantityPerUser     int      `json:"quantity_per_user"`
		Reason              string   `json:"reason"`
		SkipLimitValidation bool     `json:"skip_limit_validation"`
	}{req.TemplateID, req.UserIDs, req.QuantityPerUser, req.Reason, req.SkipLimitValidation})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TestIssueRequestHashMatchesPreRenameDigest 把发券幂等哈希钉成一个字面量摘要。
// 该摘要由改名前的实现产出，能对上就说明 DTO 的 tag 从 snake_case 换成
// camelCase 没有动到哈希，存量 Idempotency-Key 重试仍会命中而不是 409。
// 这个常量一旦需要改动，等于宣布线上所有未过期的发券 key 开始报
// IDEMPOTENCY_CONFLICT——那是需要配套版本化方案的决定，不是随手改个数。
func TestIssueRequestHashMatchesPreRenameDigest(t *testing.T) {
	req := dto.IssueCouponsRequest{TemplateID: "tpl-0001", UserIDs: []string{"u1", "u2"}, QuantityPerUser: 3, Reason: "smoke", SkipLimitValidation: true}
	const want = "4db13d216a47fd55db78a7e69e82d22ec99f6afb4c99baf3eafb3bc72602e394"
	if got := issueRequestHash(req); got != want {
		t.Fatalf("issueRequestHash() = %s, want %s", got, want)
	}
	// 顺带校验测试里的旧格式重算确实复现了这个摘要，否则上面那条重放用例
	// 断言的就是一个假前提。
	if got := legacySnakeCaseIssueHash(req); got != want {
		t.Fatalf("legacySnakeCaseIssueHash() = %s, want %s", got, want)
	}
}
