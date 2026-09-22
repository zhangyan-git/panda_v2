package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// AdminQueryRepository 是后台只读页面要的那两个读方法。
//
// **它为什么不并进上面那个 Repository 接口**：那个接口是**写路径**的形状——发起支付、
// 落回调、结算、超时收单，service 的每个测试都要造一个满足它的 mock（见
// settleAccountPayment 那一串用例）。往里加四个只有后台页面用的读方法，等于让每一个写路径
// 的用例都多出四个「反正不会被调用」的实现；而 mock 一旦有了「反正不会被调用」的方法，
// 下一个往里加东西的人就不会再想「这条路径真的会调它吗」。
//
// 反过来也一样：**读**的这一半没有、也不该有任何写方法——「后台关单」要加时会先撞上这个
// 接口的名字。配置页那六个写方法走的是同级 admin_config.go 里的 AdminConfigRepository，
// 两个接口各自只声明自己那一半。
type AdminQueryRepository interface {
	ListPayments(ctx context.Context, q dto.PaymentQuery) ([]*repository.PaymentListRow, int, error)
	GetPaymentDetail(ctx context.Context, paymentNo string) (*repository.PaymentDetailRows, error)
}

// AdminQueryService 是后台只读页面的服务层。
//
// 这一刀里它**是纯透传**——四个方法各自一行，没有任何判断。留着这一层而不是让 controller
// 直接拿仓储，理由与 membership-service 里那几个同样薄的读方法相同：分层的形状一旦为「反正
// 这次没有逻辑」破一次例，下一次有逻辑时就会顺手写在 controller 里，而那正是这个仓库里
// 「用户看到的话在 controller 翻译、service 错误串留给日志」那条分工要防的事。
//
// 它与 PaymentService 是两个独立的结构体、两次构造：写路径那些依赖（providers / secrets /
// beans）后台读路径一个都不需要，硬凑在一起只会让 cmd/main.go 里多传三个用不上的参数，
// 也会让「后台只读那一半不碰任何写依赖」这件事从类型上就看不出。
//
// 它另外要一份**目录**：支付单上记的是渠道名与方式 code，而页面要显示的是中文名。名字属于
// 「这份代码认识哪些支付方式」，不属于「这笔钱发生了什么」——所以它不是数据库能回答的问题，
// 补名字这一步就落在这一层（见 decorate）。
type AdminQueryService struct {
	repository AdminQueryRepository
	catalog    *catalog.Catalog
}

func NewAdminQueryService(repository AdminQueryRepository, directory *catalog.Catalog) *AdminQueryService {
	return &AdminQueryService{repository: repository, catalog: directory}
}

// ListPayments 读一页支付单。找不到就是空列表，不是错误。
//
// 名字在读完**之后**补：筛选与分页全在 SQL 里（那些是数据库能回答的），而「ums_h5_wechat
// 叫什么」不是。所以先取到这一页的行，再照目录补上，顺序不会让分页结果依赖目录。
func (s *AdminQueryService) ListPayments(ctx context.Context, q dto.PaymentQuery) ([]*repository.PaymentListRow, int, error) {
	items, total, err := s.repository.ListPayments(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	for _, item := range items {
		s.decorate(item)
	}
	return items, total, nil
}

// GetPaymentDetail 按支付单号读详情页的六块。
//
// 单号不存在时返回 repository.ErrPaymentNotFound（经 mapPGError），由 controller 翻成 404。
// 不在这一层把「找不到」变成空详情：一张全是空字段的详情页看上去像一笔字段没落上的支付单，
// 而真相是「你打开了一个不存在的单号」。
func (s *AdminQueryService) GetPaymentDetail(ctx context.Context, paymentNo string) (*repository.PaymentDetailRows, error) {
	detail, err := s.repository.GetPaymentDetail(ctx, paymentNo)
	if err != nil {
		return nil, err
	}
	if detail.Payment != nil {
		s.decorate(detail.Payment)
	}
	return detail, nil
}

// decorate 把支付单上的两个 code 补成页面要显示的名字。
//
// 认不出来的 code **留空而不是报错**：它是这张单当初写下的事实，而目录是会变的（某个 code
// 今天不再存在）。为它让整张列表读不出来，等于用一次展示问题挡住一次对账。页面把空名字显示
// 成 `—`，而 code 本身照常在——那正是排查时要看的东西。
func (s *AdminQueryService) decorate(item *repository.PaymentListRow) {
	if item == nil {
		return
	}
	if channel, err := s.catalog.Channel(item.ChannelCode); err == nil && channel != nil {
		item.ChannelName = channel.Name
	}
	if method, err := s.catalog.Method(item.MethodCode); err == nil {
		item.MethodName = method.Name
	}
}
