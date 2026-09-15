package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// 这一层管的是**请求的形状**：范围与行自不自洽、理由有没有、图片是不是 URL、审核人从哪来。
// 金额、状态、互斥（同一行有没有别的售后单在跑）都由仓储在锁内判，所以那些用例在
// repository 的集成测试里，不在这里——在没有数据库的地方假装它们已经验过了，比不测更糟。
//
// 内嵌 Repository 是为了只实现本文件真正走到的方法：走到别的会 nil panic，那是故意的，
// 它说明用例越界了，不该悄悄返回个零值把事情糊过去。

type fakeAfterSaleRepo struct {
	Repository

	applyParams *repository.ApplyAfterSaleParams
	applyRow    *repository.AfterSaleRow
	applyReplay bool
	applyErr    error

	reviewParams *repository.ReviewAfterSaleParams
	reviewRow    *repository.AfterSaleRow
	reviewErr    error

	cancelParams *repository.CancelAfterSaleParams
	cancelRow    *repository.AfterSaleRow
	cancelErr    error
}

func (f *fakeAfterSaleRepo) ApplyAfterSale(_ context.Context, p repository.ApplyAfterSaleParams) (*repository.AfterSaleRow, bool, error) {
	f.applyParams = &p
	if f.applyErr != nil {
		return nil, false, f.applyErr
	}
	return f.applyRow, f.applyReplay, nil
}

func (f *fakeAfterSaleRepo) ReviewAfterSale(_ context.Context, p repository.ReviewAfterSaleParams) (*repository.AfterSaleRow, error) {
	f.reviewParams = &p
	if f.reviewErr != nil {
		return nil, f.reviewErr
	}
	return f.reviewRow, nil
}

func (f *fakeAfterSaleRepo) CancelAfterSale(_ context.Context, p repository.CancelAfterSaleParams) (*repository.AfterSaleRow, error) {
	f.cancelParams = &p
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return f.cancelRow, nil
}

func newAfterSaleService(repo *fakeAfterSaleRepo) *OrderService {
	return New(repo, nil, nil, Options{})
}

// validApply 是一份能过形状校验的申请，用例各自改它关心的那一栏。
func validApply() ApplyAfterSaleInput {
	return ApplyAfterSaleInput{
		OrderID:        "00000000-0000-0000-0000-000000000001",
		UserID:         "00000000-0000-0000-0000-000000000002",
		IdempotencyKey: "key-1",
		Scope:          model.AfterSaleScopeAll,
		Reason:         "买错了",
	}
}

// applyRow 是仓储申请成功后返回的那一行。只要单号有值就够用例断言「服务层原样透传」——
// 其余的列由仓储的集成测试负责，在这个没有数据库的地方假装它们有内容反而会骗人。
func applyRow(afterSaleNo string) *repository.AfterSaleRow {
	return &repository.AfterSaleRow{AfterSale: &model.OrderAfterSale{AfterSaleNo: afterSaleNo}}
}

func TestApplyAfterSaleShapeValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ApplyAfterSaleInput)
		want   error
		// notShape：这条不是「字段填错了」而是「查不到这张单」，controller 回 404 而不是
		// 400（同 CancelOrder 对空订单 id 的处理）。
		notShape bool
	}{
		{"没有订单 id", func(in *ApplyAfterSaleInput) { in.OrderID = "  " }, ErrOrderNotFound, true},
		{"没有幂等键", func(in *ApplyAfterSaleInput) { in.IdempotencyKey = "" }, ErrIdempotencyKeyRequired, false},
		{"没有用户", func(in *ApplyAfterSaleInput) { in.UserID = "" }, ErrUserRequired, false},
		{"范围不认识", func(in *ApplyAfterSaleInput) { in.Scope = "everything" }, ErrAfterSaleScopeInvalid, false},
		{"范围是会员", func(in *ApplyAfterSaleInput) { in.Scope = model.AfterSaleScopeMembership }, ErrAfterSaleMembershipUnsupported, false},
		{"没有理由", func(in *ApplyAfterSaleInput) { in.Reason = "   " }, ErrAfterSaleReasonRequired, false},
		{"整单退带了行", func(in *ApplyAfterSaleInput) {
			line := "00000000-0000-0000-0000-000000000003"
			in.OrderLineID = &line
		}, ErrAfterSaleLineNotAllowed, false},
		{"按行退没带行", func(in *ApplyAfterSaleInput) { in.Scope = model.AfterSaleScopeDrink }, ErrAfterSaleLineRequired, false},
		{"按行退带了空行", func(in *ApplyAfterSaleInput) {
			in.Scope = model.AfterSaleScopeDrink
			blank := "  "
			in.OrderLineID = &blank
		}, ErrAfterSaleLineRequired, false},
		{"图片不是 URL", func(in *ApplyAfterSaleInput) { in.Images = []string{"javascript:alert(1)"} }, ErrAfterSaleImagesInvalid, false},
		{"图片缺 host", func(in *ApplyAfterSaleInput) { in.Images = []string{"http://"} }, ErrAfterSaleImagesInvalid, false},
		{"图片太多", func(in *ApplyAfterSaleInput) {
			for i := 0; i <= maxAfterSaleImages; i++ {
				in.Images = append(in.Images, "https://example.com/a.jpg")
			}
		}, ErrAfterSaleImagesInvalid, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{}
			in := validApply()
			tc.mutate(&in)
			_, _, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ApplyAfterSale() = %v, want %v", err, tc.want)
			}
			if repo.applyParams != nil {
				// 校验没过就不该走到仓储：走到那一步意味着请求已经落库过一次了。
				t.Fatalf("形状校验失败却调用了仓储: %+v", repo.applyParams)
			}
			if tc.notShape {
				return
			}
			if !IsValidationError(err) {
				t.Fatalf("校验错误没登记进 ValidationErrors，controller 会回 500: %v", err)
			}
		})
	}
}

// TestApplyAfterSalePassesTrimmedShape 盯的是「服务层替仓储收拾干净了什么」：单号在这里
// 生成、图片在这里序列化、范围与行的等价关系在这里定死。这些在返回值上看不出来，只能看
// 落库前那一份入参。
func TestApplyAfterSalePassesTrimmedShape(t *testing.T) {
	line := "00000000-0000-0000-0000-000000000003"
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), applyReplay: true}
	in := validApply()
	in.Scope = " " + model.AfterSaleScopeDrink + " "
	in.OrderLineID = &line
	in.Reason = " 少冰做成了正常冰 "
	in.Images = []string{" https://example.com/a.jpg "}

	row, replayed, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), in)
	if err != nil {
		t.Fatalf("ApplyAfterSale() error = %v", err)
	}
	if row.AfterSale.AfterSaleNo != "REF1" {
		t.Fatalf("结果没有原样返回仓储的返回: %+v", row)
	}
	// replayed 必须原样透出来：controller 靠它决定回 200 还是 201，服务层吞掉的话
	// 每一次重放都会被当成一次新的申请。
	if !replayed {
		t.Fatal("仓储说是重放，服务层却回 false：controller 会给重放回 201")
	}
	got := repo.applyParams
	if got.Scope != model.AfterSaleScopeDrink {
		t.Fatalf("scope = %q, want %q", got.Scope, model.AfterSaleScopeDrink)
	}
	if got.OrderLineID == nil || *got.OrderLineID != line {
		t.Fatalf("orderLineId = %v, want %s", got.OrderLineID, line)
	}
	if got.Reason != "少冰做成了正常冰" {
		t.Fatalf("reason = %q, 没有去空白", got.Reason)
	}
	if got.ActorType != model.ActorUser {
		t.Fatalf("actorType = %q, want %q", got.ActorType, model.ActorUser)
	}
	if !strings.HasPrefix(got.AfterSaleNo, "REF") {
		t.Fatalf("售后单号 %q 不以 REF 开头", got.AfterSaleNo)
	}
	// 图片以 JSON 数组落库（列类型是 JSONB），URL 的前后空白也要去掉。
	var images []string
	if err := json.Unmarshal(got.Images, &images); err != nil {
		t.Fatalf("images 不是合法 JSON 数组: %v (%s)", err, got.Images)
	}
	if len(images) != 1 || images[0] != "https://example.com/a.jpg" {
		t.Fatalf("images = %v", images)
	}
}

// TestApplyAfterSaleWithoutImages 确认「没传图」落成 []，不是 null：列的默认值就是 []，
// 两种空在库里长得一样，读出来才不用判两次。
func TestApplyAfterSaleWithoutImages(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1")}
	if _, _, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), validApply()); err != nil {
		t.Fatalf("ApplyAfterSale() error = %v", err)
	}
	if string(repo.applyParams.Images) != "[]" {
		t.Fatalf("images = %s, want []", repo.applyParams.Images)
	}
}

// TestApplyAfterSaleForwardsRepositoryError 确认仓储的判断原样传到调用方：这里的错误是
// 「这一行已经退过款了」这类锁内事实，服务层不该把它翻译成别的东西——controller 比的
// 是同一个值。
func TestApplyAfterSaleForwardsRepositoryError(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyErr: repository.ErrAfterSaleAlreadyRefunded}
	_, _, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), validApply())
	if !errors.Is(err, ErrAfterSaleAlreadyRefunded) {
		t.Fatalf("err = %v, want ErrAfterSaleAlreadyRefunded", err)
	}
}

func TestReviewAfterSaleShapeValidation(t *testing.T) {
	cases := []struct {
		name string
		in   ReviewAfterSaleInput
		want error
	}{
		{"没有单号", ReviewAfterSaleInput{Action: model.AfterSaleActionApprove, ReviewedBy: "u1"}, ErrAfterSaleNotFound},
		{"动作不认识", ReviewAfterSaleInput{AfterSaleNo: "REF1", Action: "maybe", ReviewedBy: "u1"}, ErrAfterSaleActionInvalid},
		{"驳回没写理由", ReviewAfterSaleInput{AfterSaleNo: "REF1", Action: model.AfterSaleActionReject, ReviewedBy: "u1"}, ErrAfterSaleRemarkRequired},
		{"没有审核人", ReviewAfterSaleInput{AfterSaleNo: "REF1", Action: model.AfterSaleActionApprove}, ErrForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{}
			_, err := newAfterSaleService(repo).ReviewAfterSale(t.Context(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReviewAfterSale() = %v, want %v", err, tc.want)
			}
			if repo.reviewParams != nil {
				t.Fatalf("校验失败却调用了仓储: %+v", repo.reviewParams)
			}
		})
	}
}

// TestReviewAfterSaleCarriesFortuneCardConfirmation 盯的是福卡规则的那个开关有没有被
// 原样传下去：它决定这笔退款能不能放行，中途丢掉等于给「订单送过福卡的退款」开后门。
func TestReviewAfterSaleCarriesFortuneCardConfirmation(t *testing.T) {
	repo := &fakeAfterSaleRepo{reviewRow: &repository.AfterSaleRow{}}
	_, err := newAfterSaleService(repo).ReviewAfterSale(t.Context(), ReviewAfterSaleInput{
		AfterSaleNo:                " REF1 ",
		Action:                     model.AfterSaleActionApprove,
		Remark:                     " 客服已核 ",
		FortuneCardUnusedConfirmed: true,
		ReviewedBy:                 "admin-1",
		TraceID:                    "trace-1",
	})
	if err != nil {
		t.Fatalf("ReviewAfterSale() error = %v", err)
	}
	got := repo.reviewParams
	if got.AfterSaleNo != "REF1" || got.Remark != "客服已核" {
		t.Fatalf("单号/备注没有去空白: %+v", got)
	}
	if !got.FortuneCardUnusedConfirmed {
		t.Fatal("福卡确认没有传到仓储：这笔单会被当成「没确认」拒掉")
	}
	if got.Action != model.AfterSaleActionApprove || got.ReviewedBy != "admin-1" || got.TraceID != "trace-1" {
		t.Fatalf("入参被改动: %+v", got)
	}
}

// TestReviewAfterSaleForwardsNotPending 是并发审核那一步的现场：后到的那次看到的是
// approved，仓储回 ErrAfterSaleNotPending，服务层原样传出去（controller 回 409）。
func TestReviewAfterSaleForwardsNotPending(t *testing.T) {
	repo := &fakeAfterSaleRepo{reviewErr: repository.ErrAfterSaleNotPending}
	_, err := newAfterSaleService(repo).ReviewAfterSale(t.Context(), ReviewAfterSaleInput{
		AfterSaleNo: "REF1", Action: model.AfterSaleActionApprove, ReviewedBy: "u1",
	})
	if !errors.Is(err, ErrAfterSaleNotPending) {
		t.Fatalf("err = %v, want ErrAfterSaleNotPending", err)
	}
}

func TestCancelAfterSaleShape(t *testing.T) {
	t.Run("没有用户", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{}
		_, err := newAfterSaleService(repo).CancelAfterSale(t.Context(), CancelAfterSaleInput{AfterSaleNo: "REF1"})
		if !errors.Is(err, ErrUserRequired) {
			t.Fatalf("err = %v, want ErrUserRequired", err)
		}
		if repo.cancelParams != nil {
			t.Fatal("校验失败却调用了仓储")
		}
	})

	t.Run("没有理由时补一句默认的", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{cancelRow: &repository.AfterSaleRow{}}
		if _, err := newAfterSaleService(repo).CancelAfterSale(t.Context(), CancelAfterSaleInput{
			AfterSaleNo: "REF1", UserID: "u1",
		}); err != nil {
			t.Fatalf("CancelAfterSale() error = %v", err)
		}
		// 状态流水的 reason 空着就查不出「这张单为什么走到 cancelled」，所以补一句。
		if repo.cancelParams.Reason == "" {
			t.Fatal("撤销没有补默认理由：状态流水里会留下一条空 reason 的记录")
		}
	})

	t.Run("归属由仓储判，服务层原样传 userID", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{cancelErr: repository.ErrAfterSaleNotFound}
		_, err := newAfterSaleService(repo).CancelAfterSale(t.Context(), CancelAfterSaleInput{
			AfterSaleNo: "REF1", UserID: "someone-else",
		})
		if !errors.Is(err, ErrAfterSaleNotFound) {
			t.Fatalf("err = %v, want ErrAfterSaleNotFound", err)
		}
		if repo.cancelParams.UserID != "someone-else" {
			t.Fatalf("userID = %q, 没有传下去", repo.cancelParams.UserID)
		}
	})
}

// TestAfterSaleValidationSentinelRegistered 是「新增一条校验就要在 controller 里同步加一个
// case」这句话的自动版：writeOrderError 的兜底是 500，忘了登记的校验错误会以 500 的形式
// 回给前端，前端拿 500 提示不出「你这个字段填错了」。
func TestAfterSaleValidationSentinelRegistered(t *testing.T) {
	sentinels := []error{
		ErrAfterSaleScopeInvalid, ErrAfterSaleMembershipUnsupported, ErrAfterSaleLineRequired,
		ErrAfterSaleLineNotAllowed, ErrAfterSaleReasonRequired, ErrAfterSaleImagesInvalid,
		ErrAfterSaleRemarkRequired, ErrAfterSaleActionInvalid,
	}
	for _, sentinel := range sentinels {
		if !IsValidationError(sentinel) {
			t.Errorf("%v 不在 ValidationErrors 里，controller 会回 500", sentinel)
		}
	}
}

// TestGenerateAfterSaleNo 只盯格式：前缀与长度是客服肉眼分单的依据（REF 开头一眼看得出
// 不是订单号）。唯一性靠唯一索引兜，不在这里赌随机数。
func TestGenerateAfterSaleNo(t *testing.T) {
	pattern := regexp.MustCompile(`^REF\d{14}\d{3}\d{6}$`)
	for i := 0; i < 5; i++ {
		if no := generateAfterSaleNo(time.Now()); !pattern.MatchString(no) {
			t.Fatalf("售后单号 %q 不符合 REF+YmdHis+9 位数字 的格式", no)
		}
	}
}

// TestAfterSaleStateMachine 把这张表的两条不变量钉住：这一版只能从 pending 出发，而
// 终态没有出边。表被改坏（比如给 rejected 加了出边）时，这里会先响。
func TestAfterSaleStateMachine(t *testing.T) {
	reachable := []struct{ from, to string }{
		{model.AfterSaleStatusPending, model.AfterSaleStatusApproved},
		{model.AfterSaleStatusPending, model.AfterSaleStatusRejected},
		{model.AfterSaleStatusPending, model.AfterSaleStatusCancelled},
	}
	for _, tc := range reachable {
		if !CanTransitionAfterSale(tc.from, tc.to) {
			t.Errorf("CanTransitionAfterSale(%s, %s) = false, want true", tc.from, tc.to)
		}
	}

	blocked := []struct{ from, to string }{
		// 终态没有出边：被驳回、已撤销、退款失败都意味着钱没出去，用户要退就重新申请一张。
		{model.AfterSaleStatusRejected, model.AfterSaleStatusPending},
		{model.AfterSaleStatusCancelled, model.AfterSaleStatusPending},
		{model.AfterSaleStatusRefunded, model.AfterSaleStatusApproved},
		{model.AfterSaleStatusFailed, model.AfterSaleStatusApproved},
		// 审核通过只能往下走去建退款单，不能回到 pending，也不能再驳回一次。
		{model.AfterSaleStatusApproved, model.AfterSaleStatusRejected},
		{model.AfterSaleStatusApproved, model.AfterSaleStatusPending},
		// 没听说过的状态一律拒绝：宁可挡住一次操作让人来查。
		{"whatever", model.AfterSaleStatusPending},
		{model.AfterSaleStatusPending, "whatever"},
	}
	for _, tc := range blocked {
		if CanTransitionAfterSale(tc.from, tc.to) {
			t.Errorf("CanTransitionAfterSale(%s, %s) = true, want false", tc.from, tc.to)
		}
	}
}
