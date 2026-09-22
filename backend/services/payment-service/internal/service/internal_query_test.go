package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 订阅详情那两个只读投影的用例面（见 internal_query.go）。
//
// # 为什么这份用例值得写
//
// 这两条服务方法体内一行判定都没有——它们只做两件事：把空值挡掉，以及**决定「协议不存在」与
// 「这个协议还没有任何期次」是两个不同的结论**。第二件事正是这一份要钉的：两种输入在真仓储上
// 只差一行数据，在调用方那一侧却是「报警」与「正常的开局」之别。把后者也回成 ErrAgreementNotFound
// （或者反过来把前者回成空列表），编译期不红、仓储单测不红，而前端要么一片空白的正常页面被当成
// 故障、要么一行坏数据永远装作一切正常。
//
// 假仓储用的是嵌在 fakeRepository 上的一层薄壳：这一族读只有两个方法，为了它们把整个
// Repository 再实现一遍不值得，而嵌进去之后**其余方法的默认行为与别的用例完全一致**。

// internalQueryRepo 是 fakeRepository 加两个可编排的只读格子。
type internalQueryRepo struct {
	*fakeRepository
	charges []*model.PaymentAgreementCharge
	// chargeErr 编排列表读本身的故障（真仓储那次查询会失败的那些原因：连接断了、列对不上）。
	chargeErr error
	// chargeQueries 记下每一次列表读的实参，用来断言「协议不存在时压根没往下走」。
	chargeQueries []string
}

func (r *internalQueryRepo) ListChargesByAgreementNo(_ context.Context, agreementNo string) ([]*model.PaymentAgreementCharge, error) {
	r.chargeQueries = append(r.chargeQueries, agreementNo)
	if r.chargeErr != nil {
		return nil, r.chargeErr
	}
	if r.charges == nil {
		return []*model.PaymentAgreementCharge{}, nil
	}
	return r.charges, nil
}

// newInternalQueryService 起一个只用来调这两条读的服务。
//
// 不复用 newServiceWith：那个 helper 的入参是具体的 *fakeRepository，塞不进这层壳。这一条不需要
// 目录与适配器（纯读不碰它们），只给一个空的注册表，好让「读居然走到了渠道那一步」当场炸出来。
func newInternalQueryService(repo Repository) *PaymentService {
	return New(repo, testCatalog(), provider.NewRegistry(), nil, nil, Options{})
}

// agreementFixture 造一份能被 FindAgreementByNo 找到的协议。
func agreementFixture(agreementNo string) *fakeRepository {
	return &fakeRepository{agreement: &model.PaymentAgreement{
		ID: "agreement-1", AgreementNo: agreementNo, Status: model.AgreementStatusActive,
	}}
}

// TestListAgreementChargesReadsTheAgreementsOwnPeriods 钉住正常那一格：协议查得到就照原样把
// 期次给出去，中间不做任何裁剪或排序——顺序与内容是仓储的契约（真库那一条由
// TestIntegrationListChargesByAgreementNo 钉），服务层再排一次只会让两处的规则各自漂移。
func TestListAgreementChargesReadsTheAgreementsOwnPeriods(t *testing.T) {
	const agreementNo = "AGR-1"
	repo := &internalQueryRepo{
		fakeRepository: agreementFixture(agreementNo),
		charges: []*model.PaymentAgreementCharge{
			{ID: "c1", AgreementNo: agreementNo, BizPeriod: "20261001", Status: model.ChargeStatusSucceeded},
			{ID: "c2", AgreementNo: agreementNo, BizPeriod: "20261101", Status: model.ChargeStatusCharging},
		},
	}

	charges, err := newInternalQueryService(repo).ListAgreementCharges(context.Background(), agreementNo)
	if err != nil {
		t.Fatalf("读一份存在的协议的期次失败：%v", err)
	}
	if len(charges) != 2 || charges[0].BizPeriod != "20261001" || charges[1].BizPeriod != "20261101" {
		t.Fatalf("期次被改动或丢失了：%+v", charges)
	}
	if len(repo.chargeQueries) != 1 || repo.chargeQueries[0] != agreementNo {
		t.Errorf("喂给仓储的协议号是 %v，想要 [%q]", repo.chargeQueries, agreementNo)
	}
}

// TestListAgreementChargesDistinguishesMissingFromEmpty 是这一份用例的核心：同一份空回答的两种
// 来源必须分成两个结论。
//
//	协议查不到   → ErrAgreementNotFound，**且不再去查期次**（那一次查询没有任何意义）
//	协议在、没期次 → 空列表，没有错误
//
// 「不再去查期次」这一条不是洁癖：反过来的实现（先查列表、空才去核协议）在协议真的不存在时也
// 会回 ErrAgreementNotFound，用例只看错误的话完全一样——而它在**每一次正常的空回答上**多打一次
// 库（订阅刚签、一期还没扣的页面天天走这条）。
func TestListAgreementChargesDistinguishesMissingFromEmpty(t *testing.T) {
	// 协议查不到。
	missing := &internalQueryRepo{fakeRepository: &fakeRepository{}}
	_, err := newInternalQueryService(missing).ListAgreementCharges(context.Background(), "AGR-NOT-HERE")
	if !errors.Is(err, ErrAgreementNotFound) {
		t.Errorf("协议不存在时回的是 %v，想要 ErrAgreementNotFound", err)
	}
	if len(missing.chargeQueries) != 0 {
		t.Errorf("协议都不存在，还是去查了期次：%v", missing.chargeQueries)
	}

	// 协议在，一期都还没扣过。
	const agreementNo = "AGR-2"
	empty := &internalQueryRepo{fakeRepository: agreementFixture(agreementNo)}
	charges, err := newInternalQueryService(empty).ListAgreementCharges(context.Background(), agreementNo)
	if err != nil {
		t.Fatalf("一份还没有任何期次的协议读失败了：%v（它是个正常的开局，不是错误）", err)
	}
	if charges == nil {
		t.Error("回的是 nil，调用方拿到的是 null 而不是空数组")
	}
	if len(charges) != 0 {
		t.Errorf("回了几期凭空造出来的数据：%+v", charges)
	}
}

// TestListAgreementChargesRejectsABlankAgreementNo 钉住「空串不是一次查询」：它在查协议之前
// 就被挡掉，否则回的是 ErrAgreementNotFound——那会把调用方的一个 bug（协议号没传下来）说成
// 「这份协议不见了」，让人去错的地方找。
func TestListAgreementChargesRejectsABlankAgreementNo(t *testing.T) {
	repo := &internalQueryRepo{fakeRepository: agreementFixture("AGR-3")}
	svc := newInternalQueryService(repo)

	for _, blank := range []string{"", "   ", "\t\n"} {
		_, err := svc.ListAgreementCharges(context.Background(), blank)
		if !errors.Is(err, ErrAgreementNoRequired) {
			t.Errorf("协议号 %q 回的是 %v，想要 ErrAgreementNoRequired", blank, err)
		}
	}
	if len(repo.chargeQueries) != 0 {
		t.Errorf("空协议号还是打到了仓储：%v", repo.chargeQueries)
	}
}

// TestListAgreementChargesTrimsTheAgreementNo 钉住两侧空白会被去掉再往下走。
//
// 协议号是后台那一行抄过来又经过一次 JSON 与一次表单的，末尾带一个空格是最常见的一种；不 trim
// 就会查出一份「不存在的协议」，而人眼看那个号是正确的。
func TestListAgreementChargesTrimsTheAgreementNo(t *testing.T) {
	const agreementNo = "AGR-4"
	repo := &internalQueryRepo{fakeRepository: agreementFixture(agreementNo)}

	if _, err := newInternalQueryService(repo).ListAgreementCharges(context.Background(), "  "+agreementNo+"\n"); err != nil {
		t.Fatalf("带空白的协议号读失败：%v", err)
	}
	if len(repo.chargeQueries) != 1 || repo.chargeQueries[0] != agreementNo {
		t.Errorf("喂给仓储的是 %v，想要 [%q]（空白没去掉）", repo.chargeQueries, agreementNo)
	}
}

// TestListAgreementChargesPassesTheRepositoryErrorThrough 钉住「仓储的故障不被翻成别的结论」。
//
// 一次连接超时绝不能被读成「这份协议没有期次」：那样后台会平静地显示「暂无续费记录」，而
// 真相是这一页根本没读到数据——用户以为没扣款，其实可能扣了。
func TestListAgreementChargesPassesTheRepositoryErrorThrough(t *testing.T) {
	const agreementNo = "AGR-5"
	boom := errors.New("connection reset by peer")
	repo := &internalQueryRepo{fakeRepository: agreementFixture(agreementNo), chargeErr: boom}

	_, err := newInternalQueryService(repo).ListAgreementCharges(context.Background(), agreementNo)
	if !errors.Is(err, boom) {
		t.Errorf("仓储的故障被换成了 %v，想要原样出去", err)
	}
}

// TestGetPaymentReadsOnePaymentSummary 钉住正常那一格：支付单号原样送下去，读回来的那张单
// 原样出去（幂等回放与退款那几条路都靠这一个方法，它的行为不能被这里改掉）。
func TestGetPaymentReadsOnePaymentSummary(t *testing.T) {
	const paymentNo = "PAY-1"
	repo := &fakeRepository{paymentByNo: &model.Payment{
		ID: "p1", PaymentNo: paymentNo, Status: model.PaymentSucceeded, Amount: 990,
		PaymentMethod: "ums_h5_alipay", ProviderTransactionID: "WXTXN-1",
	}}

	payment, err := newInternalQueryService(repo).GetPayment(context.Background(), "  "+paymentNo+" ")
	if err != nil {
		t.Fatalf("读一张存在的支付单失败：%v", err)
	}
	if payment.PaymentNo != paymentNo || payment.ProviderTransactionID != "WXTXN-1" {
		t.Errorf("读回来的不是那张单：%+v", payment)
	}
}

// TestGetPaymentRejectsABlankPaymentNo 钉住空串回的是「请求不合法」而不是「查无此单」。
//
// 这条分档在这一刀里是有具体来历的：会员续费单与设备单的订单上 payment_no **就是空的**（那两条
// 路上没有支付单）。调用方把那个空值原样拿来问时，回 NotFound 会让「这条路没有支付单」看起来
// 像一次正常的数据缺失，而回 InvalidArgument 能让那个 bug 当场显形（见 internal_query.go）。
func TestGetPaymentRejectsABlankPaymentNo(t *testing.T) {
	repo := &fakeRepository{}
	svc := newInternalQueryService(repo)

	for _, blank := range []string{"", "   "} {
		_, err := svc.GetPayment(context.Background(), blank)
		if !errors.Is(err, ErrPaymentNoRequired) {
			t.Errorf("支付单号 %q 回的是 %v，想要 ErrPaymentNoRequired", blank, err)
		}
	}
}

// TestGetPaymentPassesNotFoundThrough 钉住查无此单照旧是 ErrPaymentNotFound——不在这里被翻成
// 别的结论，也不回一张零值的单让调用方继续往下走。
func TestGetPaymentPassesNotFoundThrough(t *testing.T) {
	_, err := newInternalQueryService(&fakeRepository{}).GetPayment(context.Background(), "PAY-NOT-HERE")
	if !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("查无此单回的是 %v，想要 ErrPaymentNotFound", err)
	}
}
