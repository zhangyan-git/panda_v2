package controller

import (
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
)

// MiniappFortuneCardController 是小程序端的福卡接口。
//
// 只有一个读：余额 + 一页自己的流水。**没有**写入口——小程序这一侧今天没有任何动作会
// 改余额（抽奖扣减走 lottery-service 的 gRPC，不由客户端直连）。
type MiniappFortuneCardController struct{ accounts *service.AccountService }

const miniappFortuneCardPath = "/v1/miniapp/fortune-cards"

func NewMiniappFortuneCardController(accounts *service.AccountService) *MiniappFortuneCardController {
	return &MiniappFortuneCardController{accounts: accounts}
}

// Cards 处理 GET /v1/miniapp/fortune-cards。
//
// 查谁的余额由令牌决定，**不接受任何来自查询串的 user_id**：那等于把别人的流水接口
// 敞开。归属在这里就已经定死，service 再拿它去读。
func (c *MiniappFortuneCardController) Cards(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	page, size, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}

	// 读账户本身而不是只读余额：页面上要写的是「可用 1 张（1 张因退款申请冻结中）」，
	// 只给一个对不上账的余额正是客服被问住的那句话。
	account, err := c.accounts.Account(r.Context(), userID)
	if err != nil {
		writeFortuneCardError(w, err, "failed to read fortune card account")
		return
	}
	entries, total, err := c.accounts.Entries(r.Context(), dto.EntryQuery{
		UserID:   userID,
		Page:     page,
		PageSize: size,
	})
	if err != nil {
		writeFortuneCardError(w, err, "failed to list fortune card entries")
		return
	}
	api.Success(w, &dto.MiniappCardsResponse{
		Balance:          account.Balance,
		FrozenBalance:    account.FrozenBalance,
		AvailableBalance: account.AvailableBalance,
		Entries:          entryResponses(entries),
		Total:            total,
		Page:             page,
		PageSize:         size,
	})
}

// requireConsumer 是 C 端入口的闸门，返回调用者的用户 ID；失败时已经写好了响应。
//
// 判定必须落在 realm 上，不能只看 subject == user_id：平台管理员和商户账号的令牌同样
// 满足那两条，只查它们等于把福卡账户对管理端敞开——余额和流水是用户资产，不该由一条
// 管理端令牌从 C 端入口读走（管理端有自己的 /v1/admin/fortune-cards，那条路上有权限码）。
// 缺失和域不对分成 401 与 403：前者是没登录，后者是拿错了身份的令牌，客户端要做的处理
// 不同。这一段与 order-service / user-service 的 requireConsumer 是同一套判定，三处
// 必须保持一致，否则同一种令牌在三个服务上会得到不同的回答。
func requireConsumer(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" || identity.Subject != identity.UserID {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
		return "", false
	}
	if identity.Realm != auth.RealmConsumer {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "非小程序用户")
		return "", false
	}
	return identity.UserID, true
}
