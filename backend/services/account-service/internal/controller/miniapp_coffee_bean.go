package controller

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
)

// MiniappCoffeeBeanController 是小程序端的咖啡豆接口。
//
// 与福卡那边同形，也只有一个读：余额 + 一页自己的流水。**没有**写入口——豆的来路只有后台
// 人工调整，花豆走 payment-service 的账户出资（服务端直连，不由客户端调），小程序这一侧
// 今天没有任何动作会改余额。
type MiniappCoffeeBeanController struct{ accounts *service.AccountService }

const miniappCoffeeBeanPath = "/v1/miniapp/coffee-beans"

func NewMiniappCoffeeBeanController(accounts *service.AccountService) *MiniappCoffeeBeanController {
	return &MiniappCoffeeBeanController{accounts: accounts}
}

// Beans 处理 GET /v1/miniapp/coffee-beans。
//
// 归属由令牌定死，理由与福卡那条逐字相同：查谁的余额不接受任何来自查询串的 user_id，
// 那等于把别人的流水敞开。
//
// 没有账户时不报 404：账号是懒创建的，一个没充过豆的用户问自己的余额，答案是「0 分」，
// 不是「查不到这个人」——那会变成一句让用户以为账号有问题的提示。
func (c *MiniappCoffeeBeanController) Beans(w http.ResponseWriter, r *http.Request) {
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
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, message)
		return
	}

	account, err := c.accounts.BeanAccount(r.Context(), userID)
	if err != nil {
		writeCoffeeBeanError(w, err, "failed to read coffee bean account")
		return
	}
	entries, total, err := c.accounts.BeanEntries(r.Context(), dto.BeanEntryQuery{
		UserID:   userID,
		Page:     page,
		PageSize: size,
	})
	if err != nil {
		writeCoffeeBeanError(w, err, "failed to list coffee bean entries")
		return
	}
	api.Success(w, &dto.MiniappBeansResponse{
		Balance:  account.Balance,
		Entries:  beanEntryResponses(entries),
		Total:    total,
		Page:     page,
		PageSize: size,
	})
}
