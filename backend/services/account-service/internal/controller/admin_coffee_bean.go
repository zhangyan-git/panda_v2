package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
)

// AdminCoffeeBeanController 是后台的咖啡豆接口。
//
// 与福卡那个只读的控制器不同，这一棵树上有一个**写**：人工调整余额
// （POST /v1/admin/coffee-beans/{userId}/adjustments，权限码 account:manage）。它是全系统
// 唯一能凭空改动豆余额的入口——另一条写路径只能把钱扣走，冲正只能把扣过的还回来——所以
// 那条路上有审计，且审计与写入同生共死（见 repository.BeanAdminRepository）。
type AdminCoffeeBeanController struct {
	accounts *service.AccountService
	beans    *service.BeanAdminService
}

const adminCoffeeBeanPath = "/v1/admin/coffee-beans"

func NewAdminCoffeeBeanController(accounts *service.AccountService, beans *service.BeanAdminService) *AdminCoffeeBeanController {
	return &AdminCoffeeBeanController{accounts: accounts, beans: beans}
}

// Beans 分发 /v1/admin/coffee-beans 这一棵路径。
//
// 与福卡那边同一个形状（一个入口方法 + 路径分发），因为路由注册处同一条约束：gorilla/mux
// 取第一个匹配的路径，所以注册顺序是从长到短，分发这件事因此留在这里而不是路由表里。
func (c *AdminCoffeeBeanController) Beans(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminCoffeeBeanPath), "/")
	switch {
	case rest == "entries":
		c.entries(w, r)
	case strings.HasSuffix(rest, "/adjustments"):
		c.adjust(w, r, strings.TrimSuffix(rest, "/adjustments"))
	default:
		c.account(w, r, rest)
	}
}

// entries 是流水查询：userId / referenceNo / entryType / 时间闭开区间 + 分页。
//
// 过滤列是 referenceNo 而不是 orderNo：豆流水的 reference_no 有三种形状（调整的幂等号、
// 订单号、售后单号），订单号只是其中一种。
func (c *AdminCoffeeBeanController) entries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	q := dto.BeanEntryQuery{
		UserID:      strings.TrimSpace(query.Get("userId")),
		ReferenceNo: strings.TrimSpace(query.Get("referenceNo")),
		EntryType:   strings.TrimSpace(query.Get("entryType")),
	}
	if q.UserID != "" {
		if _, err := uuid.Parse(q.UserID); err != nil {
			api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "userId must be a UUID")
			return
		}
	}
	if q.EntryType != "" && !isBeanEntryType(q.EntryType) {
		// 枚举值在这里就挡掉：放过它只会得到一页空数据，看起来像「这个人没有流水」。
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "entryType must be adjust, consume or reverse")
		return
	}
	from, err := parseTimeParam(query.Get("from"))
	if err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "from must be RFC3339")
		return
	}
	to, err := parseTimeParam(query.Get("to"))
	if err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "to must be RFC3339")
		return
	}
	q.From, q.To = from, to

	page, size, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, message)
		return
	}
	q.Page, q.PageSize = page, size

	entries, total, err := c.accounts.BeanEntries(r.Context(), q)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to list coffee bean entries")
		return
	}
	api.Success(w, api.PageResponse{Items: beanEntryResponses(entries), Total: int64(total), Page: q.Page, PageSize: q.PageSize})
}

// account 是「某人还有多少豆」——客服最常问的那一句。
func (c *AdminCoffeeBeanController) account(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	// 路径为空或还带着斜杠（多了一段），都不是这一棵树上的资源。
	if userID == "" || strings.Contains(userID, "/") {
		http.NotFound(w, r)
		return
	}
	if _, err := uuid.Parse(userID); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "userId must be a UUID")
		return
	}
	account, err := c.accounts.BeanAccount(r.Context(), userID)
	if err != nil {
		writeCoffeeBeanError(w, err, "failed to read coffee bean account")
		return
	}
	api.Success(w, account)
}

// adjust 处理 POST /v1/admin/coffee-beans/{userId}/adjustments：人工调整余额。
//
// 金额**带符号**，单位分：充值为正、把充错的豆调回来为负。幂等号由后台每次打开弹窗时生成，
// 同一个号再来一次回 409 而不是回放原样——点它的是人，回一句「成功了」会让它再点一次。
//
// 操作人取自令牌（auth.IdentityFromRequest），不从请求体里读：那是一个可以由调用方随便填的
// 字段，而审计要回答的正是「谁动的」。
func (c *AdminCoffeeBeanController) adjust(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(userID, "/") {
		http.NotFound(w, r)
		return
	}
	if _, err := uuid.Parse(userID); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "userId must be a UUID")
		return
	}
	var body dto.BeanAdjustInput
	if err := decodeJSON(r, &body); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	var operator string
	if identity, ok := auth.IdentityFromRequest(r); ok {
		operator = identity.UserID
	}
	result, err := c.beans.AdjustBeans(r.Context(), service.BeanAdjustRequest{
		UserID:    userID,
		Amount:    body.Amount,
		RequestID: body.RequestID,
		Remark:    body.Remark,
		Operator:  operator,
	})
	if err != nil {
		writeCoffeeBeanError(w, err, "failed to adjust coffee bean balance")
		return
	}
	api.Success(w, &dto.BeanAdjustResponse{Balance: result.BalanceAfter, EntryID: result.EntryID})
}

func isBeanEntryType(value string) bool {
	switch value {
	case model.BeanEntryTypeAdjust, model.BeanEntryTypeConsume, model.BeanEntryTypeReverse:
		return true
	default:
		return false
	}
}

// decodeJSON 把请求体读进 v，请求体过大或格式不对时返回可读的错误。
//
// DisallowUnknownFields 是有意的（与 coffee-machine-service 同一条理由）：字段名拼错时静默
// 忽略，会变成一个「提示成功了但余额没变」的工单。
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20)) // 1 MiB
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	return nil
}

// writeCoffeeBeanError 把 service/repository 的错误翻成状态码。
//
// 与 writeFortuneCardError 分开：这一棵树上有写，所以多两类回答——「这个数会把余额扣成
// 负数」（400，是入参的问题）与「这个幂等号记过账了」（409，让调用方自己去查流水）。
func writeCoffeeBeanError(w http.ResponseWriter, err error, message string) {
	switch {
	case errors.Is(err, service.ErrInvalidUserID),
		errors.Is(err, service.ErrInvalidOrderID),
		errors.Is(err, service.ErrInvalidAmount),
		errors.Is(err, service.ErrInvalidRequestID),
		errors.Is(err, service.ErrInvalidAfterSaleNo):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case errors.Is(err, repository.ErrBeanAmountZero):
		// 调整 0 分不是一次调整，几乎总是手滑点了保存。
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "调整金额不能为 0")
	case errors.Is(err, repository.ErrInsufficientCoffeeBeans):
		// 走这条路的只有人工调整：扣减那条路在 gRPC 面，回的是 FailedPrecondition。
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "余额不足：这次调整会把余额扣成负数")
	case errors.Is(err, repository.ErrBeanDuplicateRequest), errors.Is(err, repository.ErrBeanEntryKeyConflict):
		// 同一个 requestId 又来了：上一次多半已经成功，前端只是没收到响应。回 409 而不是
		// 当作成功，让调用方自己去查流水，而不是在这里替它下结论。
		api.Error(w, http.StatusConflict, api.CodeConflict, "该 requestId 已经记过账")
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, repository.ErrBeanEntryNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "coffee bean account not found")
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, message)
	}
}
