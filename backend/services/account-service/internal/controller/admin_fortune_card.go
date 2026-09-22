package controller

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
)

// AdminFortuneCardController 是后台的福卡账户接口。只读——本轮没有人工调整余额的入口，
// 所以这里没有任何写方法（见方案「明确不做」）。
type AdminFortuneCardController struct{ accounts *service.AccountService }

const adminFortuneCardPath = "/v1/admin/fortune-cards"

func NewAdminFortuneCardController(accounts *service.AccountService) *AdminFortuneCardController {
	return &AdminFortuneCardController{accounts: accounts}
}

// Cards 分发 /v1/admin/fortune-cards 这一棵路径。
func (c *AdminFortuneCardController) Cards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminFortuneCardPath), "/")
	switch {
	case rest == "entries":
		c.entries(w, r)
	case rest == "freezes":
		c.freezes(w, r)
	case rest != "" && !strings.Contains(rest, "/"):
		c.account(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

// entries 是流水查询：userId / orderNo / entryType / 时间闭开区间 + 分页。
//
// 订单详情那个「福卡」页签按 orderNo 用它（一单的承诺与到账放在一起看）。
func (c *AdminFortuneCardController) entries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := dto.EntryQuery{
		UserID:    strings.TrimSpace(query.Get("userId")),
		OrderNo:   strings.TrimSpace(query.Get("orderNo")),
		EntryType: strings.TrimSpace(query.Get("entryType")),
	}
	if q.UserID != "" {
		if _, err := uuid.Parse(q.UserID); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "userId must be a UUID")
			return
		}
	}
	if q.EntryType != "" && !isEntryType(q.EntryType) {
		// 枚举值在这里就挡掉：放过它只会得到一页空数据，看起来像「这个人没有流水」。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "entryType must be grant, draw or reverse")
		return
	}
	from, err := parseTimeParam(query.Get("from"))
	if err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "from must be RFC3339")
		return
	}
	to, err := parseTimeParam(query.Get("to"))
	if err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "to must be RFC3339")
		return
	}
	q.From, q.To = from, to

	page, size, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	q.Page, q.PageSize = page, size

	entries, total, err := c.accounts.Entries(r.Context(), q)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list fortune card entries")
		return
	}
	// Total 在 api.PageResponse 上是 int64，在 service 上是 int（仓储的 COUNT 走 int）：
	// 分页封套是全仓统一的那一个，不该为这一个接口改它的类型。
	api.Success(w, api.PageResponse{Items: entryResponses(entries), Total: int64(total), Page: q.Page, PageSize: q.PageSize})
}

// freezes 是退款冻结的核对：orderNo / userId / status + 分页。
//
// 订单详情那个「福卡」页签按 orderNo 用它（这一单的卡有没有被退款申请锁住），
// 客服按 userId 用它回答「我的卡为什么不能用」。
func (c *AdminFortuneCardController) freezes(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := dto.FreezeQuery{
		UserID:  strings.TrimSpace(query.Get("userId")),
		OrderNo: strings.TrimSpace(query.Get("orderNo")),
		Status:  strings.TrimSpace(query.Get("status")),
	}
	if q.UserID != "" {
		if _, err := uuid.Parse(q.UserID); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "userId must be a UUID")
			return
		}
	}
	if q.Status != "" && !isFreezeStatus(q.Status) {
		// 与 entryType 同一条规矩：放过一个拼错的状态只会得到一页空数据，
		// 看起来像「这个人的卡没被冻过」。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "status must be frozen, released or recovered")
		return
	}

	page, size, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	q.Page, q.PageSize = page, size

	freezes, total, err := c.accounts.Freezes(r.Context(), q)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list fortune card freezes")
		return
	}
	api.Success(w, api.PageResponse{Items: freezeResponses(freezes), Total: int64(total), Page: q.Page, PageSize: q.PageSize})
}

// account 是「某人还有几张福卡」——客服最常问的那一句。
func (c *AdminFortuneCardController) account(w http.ResponseWriter, r *http.Request, userID string) {
	if _, err := uuid.Parse(userID); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "userId must be a UUID")
		return
	}
	account, err := c.accounts.Account(r.Context(), userID)
	if err != nil {
		writeFortuneCardError(w, err, "failed to read fortune card account")
		return
	}
	api.Success(w, account)
}

func isEntryType(value string) bool {
	switch value {
	case model.EntryTypeGrant, model.EntryTypeDraw, model.EntryTypeReverse:
		return true
	default:
		return false
	}
}

func isFreezeStatus(value string) bool {
	switch value {
	case model.FreezeStatusFrozen, model.FreezeStatusReleased, model.FreezeStatusRecovered:
		return true
	default:
		return false
	}
}

// parseTimeParam 解析时间筛选参数，空串表示不筛。
//
// 只收 RFC3339（带时区的），不收 "2006-01-02"：日期字符串没有时区，而「今天」是业务
// 时区的今天。前端把「今天」换算成带时区的起止时刻再传过来，是唯一不会在跨时区部署时
// 悄悄错一天的做法（与 order-service 的 parseTimeParam 同一条规矩）。
func parseTimeParam(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// writeFortuneCardError 把 service/repository 的错误翻成状态码。
//
// 这一层只认得「请求不合法」与「查不到」两种；余额不足那条路走的是 gRPC 面，HTTP 这一侧
// 没有写操作，所以不会遇到。
func writeFortuneCardError(w http.ResponseWriter, err error, message string) {
	switch {
	case errors.Is(err, service.ErrInvalidUserID):
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, repository.ErrEntryNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "fortune card account not found")
	default:
		api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", message)
	}
}
