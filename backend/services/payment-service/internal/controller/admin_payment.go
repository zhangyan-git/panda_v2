package controller

import (
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// 后台支付域的只读接口。三条路径，四个 handler（支付单那条路径上分列表与详情两支）。
//
// # 这一层在整个服务里的位置
//
// 它是 payment-service 里**第一个用 platform/api 的地方**：在此之前这个服务的 controller
// 只有渠道回调，而回调按渠道的约定答（HTTP 状态 + 渠道自己认的报文），一个字都不走我们的
// 错误信封。见 doc.go 与 payment_response.go 各自的说明。
//
// # 为什么这一半里一条写路径都没有
//
// 支付单是**钱的既成事实**：一笔 succeeded 的单子后面挂着出资行、记账流水、渠道调用、回调
// 通知四张只增表。开任何一个写入口（关单、改状态、重放回调）都要先有退款与对账那两件事的
// 语义，而它们本轮没做（见 docs/architecture.md 支付那一段）。所以这里注册的全是 GET——
// 写方法落在 routeFor 的 http.NotFound 上，不是 405，更不是 200。
//
// 早先这里还有一篇隔壁的 admin_config.go：支付方式与渠道的写接口，改的是「将来怎么收钱」，
// 所以能动。那一片连同 payment:manage 一起删了（见 routes/admin.go）——今天后台这一棵树上
// 一个写入口都没有。
//
// adminPaymentsPath 是这条路径上裁路径参数用的前缀。它与 routes/admin.go 里注册那两条路由时
// 写的字面量**必须一起改**：这里多一点少一点，裁出来的 rest 就不是支付单号。
// （早先还有两条没有路径参数的路径，它们的前缀只写在 routes 那一侧。那两条删了。）
const adminPaymentsPath = "/v1/admin/payments"

// AdminPaymentController 是后台支付域的只读控制器。
//
// 它自己拿一份目录，只为一件事：把 `methodCode` 这个筛选值在**进 SQL 之前**挡下来（见
// listPayments）。这里不是「controller 顺手查了目录」——service 层那份目录是用来把 code
// 翻成中文名的，与校验请求参数是两件事，共用一份实例只是因为它本来就只构造一次。
type AdminPaymentController struct {
	payments *service.AdminQueryService
	catalog  *catalog.Catalog
}

func NewAdminPaymentController(payments *service.AdminQueryService, directory *catalog.Catalog) *AdminPaymentController {
	return &AdminPaymentController{payments: payments, catalog: directory}
}

// Payments 分发 /v1/admin/payments 与 /v1/admin/payments/{paymentNo}。
//
// 路径参数是**手写裁剪**出来的，与回调那边取渠道码同款（见 payment_response.go 的
// callbackChannelCode）：路径模板写在 routes 里只是为了注册与指标里的标签好看，真正被解析的
// 是 r.URL.Path。
//
// 单号里不会出现斜杠，所以多一段（/payments/a/b）一律 404——不把多出来的那一段当成单号的一部分
// 去查库，那只会让「有人拼错了 URL」看起来像「这个单号查无此单」。
func (c *AdminPaymentController) Payments(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := restOf(r.URL.Path, adminPaymentsPath)
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listPayments(w, r)
	case rest != "" && r.Method == http.MethodGet && !strings.Contains(rest, "/"):
		c.getPaymentDetail(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminPaymentController) listPayments(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	createdFrom, createdTo, ok := timeRange(query.Get("createdFrom"), query.Get("createdTo"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidDate)
		return
	}
	userID, ok := parseOptionalUUID(query.Get("userId"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidUserID)
		return
	}
	// 枚举筛选先过白名单再进 SQL。
	//
	// 不校验的后果不是报错，而是**安静地返回空列表**：`status=suceeded`（打错一个字母）与
	// 「这段时间真的一张成功的单都没有」在响应里长得一模一样。前者是该被指出来的输入错误，
	// 后者是运营拿去盘账的结论——把它俩混起来比报错糟得多。白名单挡下来，用户看到的是一句
	// 「筛选条件不合法」，他知道自己该改哪里。
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !model.IsPaymentStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}
	methodCode := strings.TrimSpace(query.Get("methodCode"))
	if methodCode != "" {
		if _, err := c.catalog.Method(methodCode); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
			return
		}
	}

	items, total, err := c.payments.ListPayments(r.Context(), dto.PaymentQuery{
		PaymentNo:   strings.TrimSpace(query.Get("paymentNo")),
		OrderNo:     strings.TrimSpace(query.Get("orderNo")),
		UserID:      userID,
		Status:      status,
		MethodCode:  methodCode,
		CreatedFrom: createdFrom,
		CreatedTo:   createdTo,
		Page:        page,
		PageSize:    pageSize,
	})
	if err != nil {
		writeAdminPaymentError(w, r, err, "failed to list payments")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    paymentItems(items),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// getPaymentDetail 一次给出详情页的六块。
//
// 页面**只发这一次请求**：六个页签各自去取一次会让打开一次详情发六个请求，而且六个响应之间
// 天然不一致（第三次回来时第二块可能已经变了）。数据在同一个只读快照里读出来，见
// repository.GetPaymentDetail。
func (c *AdminPaymentController) getPaymentDetail(w http.ResponseWriter, r *http.Request, paymentNo string) {
	detail, err := c.payments.GetPaymentDetail(r.Context(), paymentNo)
	if err != nil {
		writeAdminPaymentError(w, r, err, "failed to read payment detail")
		return
	}
	api.Success(w, dto.PaymentDetail{
		Payment:       paymentItem(detail.Payment),
		Fundings:      fundingItems(detail.Fundings),
		Transactions:  transactionItems(detail.Transactions),
		Transitions:   transitionItems(detail.Transitions),
		ProviderCalls: providerCallItems(detail.ProviderCalls),
		Notifications: notificationItems(detail.Notifications),
	})
}

// 空值一律走 derefString / 原样透传指针，**不在这里把 nil 换成 0 或空串再让前端猜**：
// closedAt 的 null 与「发生在零时刻」必须分得开，前端那个 `—` 是照着 null 显示的。

func paymentItem(row *repository.PaymentListRow) dto.PaymentItem {
	if row == nil || row.Payment == nil {
		return dto.PaymentItem{}
	}
	p := row.Payment
	return dto.PaymentItem{
		ID:        p.ID,
		PaymentNo: p.PaymentNo,
		LegacyID:  p.LegacyID,
		OrderNo:   p.OrderNo,
		UserID:    p.UserID,
		Amount:    p.Amount,
		Status:    p.Status,
		Subject:   p.Subject,
		Attach:    p.Attach,

		ProviderTransactionID: p.ProviderTransactionID,
		FailureCode:           p.FailureCode,
		FailureMessage:        p.FailureMessage,
		RequestID:             p.RequestID,

		ChannelCode: row.ChannelCode,
		ChannelName: row.ChannelName,

		MethodCode: row.MethodCode,
		MethodName: row.MethodName,

		AccountEntryID:  p.AccountEntryID,
		AccountFundedAt: p.AccountFundedAt,
		ExpiresAt:       p.ExpiresAt,
		PaidAt:          p.PaidAt,
		ClosedAt:        p.ClosedAt,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
}

func paymentItems(rows []*repository.PaymentListRow) []dto.PaymentItem {
	items := make([]dto.PaymentItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, paymentItem(row))
	}
	return items
}

func fundingItems(fundings []*model.PaymentFunding) []dto.PaymentFundingItem {
	items := make([]dto.PaymentFundingItem, 0, len(fundings))
	for _, f := range fundings {
		items = append(items, dto.PaymentFundingItem{
			ID:                    f.ID,
			LineNo:                f.LineNo,
			LineType:              f.LineType,
			Amount:                f.Amount,
			Status:                f.Status,
			ProviderTransactionID: f.ProviderTransactionID,
			FailureCode:           f.FailureCode,
			AccountEntryID:        f.AccountEntryID,
			SucceededAt:           f.SucceededAt,
			ReversedAt:            f.ReversedAt,
			CreatedAt:             f.CreatedAt,
			UpdatedAt:             f.UpdatedAt,
		})
	}
	return items
}

func transactionItems(transactions []*model.PaymentTransaction) []dto.PaymentTransactionItem {
	items := make([]dto.PaymentTransactionItem, 0, len(transactions))
	for _, t := range transactions {
		items = append(items, dto.PaymentTransactionItem{
			ID:                    t.ID,
			LegacyID:              t.LegacyID,
			Kind:                  t.Kind,
			PaymentNo:             t.PaymentNo,
			RefundNo:              t.RefundNo,
			FundingLineNo:         t.FundingLineNo,
			LineType:              t.LineType,
			Direction:             t.Direction,
			Amount:                t.Amount,
			Provider:              t.Provider,
			ProviderTransactionID: t.ProviderTransactionID,
			AccountEntryID:        t.AccountEntryID,
			OccurredAt:            t.OccurredAt,
			CreatedAt:             t.CreatedAt,
		})
	}
	return items
}

// transitionItems 原样带上 reason 与 request_id。
//
// reason 是中英混着的诊断串（`payment created` / `provider reported payment failed: 用户取消`
// / `咖啡豆余额不足，需要 3400 分`），页面上也原样显示。**不在这里把它映射成中文码表**：
// 那种映射一定映射错，而它承载的恰恰是「当时到底发生了什么」这条唯一的一手信息。
func transitionItems(transitions []*model.PaymentStateTransition) []dto.PaymentTransitionItem {
	items := make([]dto.PaymentTransitionItem, 0, len(transitions))
	for _, t := range transitions {
		items = append(items, dto.PaymentTransitionItem{
			ID:            t.ID,
			AggregateType: t.AggregateType,
			AggregateID:   t.AggregateID,
			FromStatus:    t.FromStatus,
			ToStatus:      t.ToStatus,
			Reason:        t.Reason,
			RequestID:     t.RequestID,
			ActorType:     t.ActorType,
			ActorID:       t.ActorID,
			Metadata:      t.Metadata,
			CreatedAt:     t.CreatedAt,
		})
	}
	return items
}

func providerCallItems(calls []*model.PaymentProviderCall) []dto.PaymentProviderCallItem {
	items := make([]dto.PaymentProviderCallItem, 0, len(calls))
	for _, call := range calls {
		items = append(items, dto.PaymentProviderCallItem{
			ID:              call.ID,
			Provider:        call.Provider,
			Operation:       call.Operation,
			PaymentNo:       call.PaymentNo,
			RefundNo:        call.RefundNo,
			AgreementNo:     call.AgreementNo,
			RequestID:       call.RequestID,
			TraceID:         call.TraceID,
			AttemptNo:       call.AttemptNo,
			RequestSummary:  call.RequestSummary,
			ResponseSummary: call.ResponseSummary,
			HTTPStatus:      call.HTTPStatus,
			ProviderCode:    call.ProviderCode,
			ProviderMessage: call.ProviderMessage,
			Result:          call.Result,
			DurationMS:      call.DurationMS,
			CreatedAt:       call.CreatedAt,
		})
	}
	return items
}

// notificationItems 把 bytea 的 body 显式转成 string。
//
// **不转的话 JSON 出的是 base64**（Go 对 []byte 的默认编码），前端 `JSON.stringify` 之后
// 看到的就是一串谁都不认识的字母——而这一列本来的用途恰恰是「渠道到底回了什么」。
// 转成 string 之后，非 UTF-8 的字节会被替换成 U+FFFD，页面上注明过这件事。
func notificationItems(notifications []*model.PaymentNotification) []dto.PaymentNotificationItem {
	items := make([]dto.PaymentNotificationItem, 0, len(notifications))
	for _, n := range notifications {
		items = append(items, dto.PaymentNotificationItem{
			ID:                n.ID,
			Provider:          n.Provider,
			NotificationID:    n.NotificationID,
			EventType:         n.EventType,
			PaymentNo:         n.PaymentNo,
			RefundNo:          n.RefundNo,
			Body:              string(n.Body),
			BodySHA256:        n.BodySHA256,
			Headers:           n.Headers,
			SignatureVerified: n.SignatureVerified,
			Status:            n.Status,
			FailureReason:     n.FailureReason,
			ReceivedAt:        n.ReceivedAt,
			ProcessedAt:       n.ProcessedAt,
		})
	}
	return items
}
