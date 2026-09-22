package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 本文件是**给别的服务**的两个只读投影：一份协议下的代扣期次，以及一张支付单的摘要。
//
// 它们的调用方今天只有一个——membership-service，用来渲染后台「包月订阅」详情抽屉里的
// 「续费明细」与「首月支付」两块（见 contracts/proto/payment/v1 的 ListAgreementCharges 与
// GetPayment）。
//
// # 为什么不与 admin_query.go 那两条合在一起
//
// admin_query.go 是**后台页面**的读：它补中文名、按页面的形状拼行，而且它自己的接口
// （AdminQueryRepository）刻意与写路径的 Repository 分开。这两条不同：
//
//   - 它们是**别的服务**在读，不是我们自己的页面在读，所以它们不该长成某个页面的形状
//     （中文名、拼好的展示串都不是这里的责任）；
//   - 它们要的两份事实（这一期扣成没有、这笔钱的渠道流水号）本来就是本域自己那两个聚合上的，
//     换一个消费者不改变这一点。
//
// # 它们都是纯读
//
// 没有锁、没有事务、不碰任何写路径。调用方拿到的是某一刻的快照，而这是够的：这两块的用途
// 是给人看「这几期发生了什么」，不是拿来做判定（真正的判定——能不能扣、扣没扣成——都在
// 各自那条写路径的锁里做）。

// ListAgreementCharges 读一份协议下的全部期次，按期次升序（最早的一期在前）。
//
// # 协议不存在与「这个协议还没有任何期次」是两件事
//
// 前者返回 ErrAgreementNotFound，后者返回一个空列表。分开是必要的：调用方（membership-service）
// 手里那份协议号抄自订阅行，对不上任何一份协议时说明**那行数据坏了**（或者指着别的服务的
// 库），而它对「这个订阅一期都没扣过」的处置完全不同——前者要报警，后者是正常的开局。
// 把前者也回成空列表，那行坏数据会一直装作一切正常。
//
// 所以要查一次协议。它是 agreement_no 上的一次唯一索引查找，与后面那次列表读同量级。
func (s *PaymentService) ListAgreementCharges(ctx context.Context, agreementNo string) ([]*model.PaymentAgreementCharge, error) {
	agreementNo = strings.TrimSpace(agreementNo)
	if agreementNo == "" {
		return nil, ErrAgreementNoRequired
	}
	// 先确认这份协议真的在我们这儿。**不**把查到的协议回给调用方：它要的是期次，
	// 而协议本身的状态属于签约那条路（QueryAgreement）。
	if _, err := s.repository.FindAgreementByNo(ctx, agreementNo); err != nil {
		return nil, err
	}
	return s.repository.ListChargesByAgreementNo(ctx, agreementNo)
}

// GetPayment 读一张支付单的摘要（渠道流水号 / 支付方式 / 实付 / 支付时间 / 订单号）。
//
// # 它是「渠道流水号」唯一的出口
//
// 渠道流水号（provider_transaction_id）是对账凭据，全仓只有本服务对外给得出它——
// order-service 的 GetOrder 只给 payment_no（见那边的说明）。调用方拿着订单上的支付单号来换
// 这一格，这是既定分工。
//
// # 支付单号为空是 InvalidArgument，不是 NotFound
//
// 空串在这里不是「没有这一单」。设备单与会员续费单的订单上 payment_no 就是空的（那两条路上
// 没有支付单），调用方拿着空值过来问，说明它把「这条路没有支付单」误当成了「来查一下」。
// 回 NotFound 会让那个 bug 看起来像一次正常的数据缺失；回 InvalidArgument 能让它显形。
func (s *PaymentService) GetPayment(ctx context.Context, paymentNo string) (*model.Payment, error) {
	paymentNo = strings.TrimSpace(paymentNo)
	if paymentNo == "" {
		return nil, ErrPaymentNoRequired
	}
	return s.repository.FindPaymentByNo(ctx, paymentNo)
}
