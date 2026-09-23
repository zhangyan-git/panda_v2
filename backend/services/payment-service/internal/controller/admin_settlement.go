package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// 分账后台的 HTTP 入口：规则、账户、任务、渠道四条路径，十二个方法。
//
// # 与 admin_payment.go 的分工
//
// 那个文件是**只读**那一半的形状，它整篇的道理都建立在「支付单没有写入口」上。这里是另一半：
// 规则与账户**能改**。两半共用同一套管道（requireAdmin / pageParams / restOf / api.Success），
// 但错误那一半不共用——读路径上服务层只会产生一个错误值，一句 switch 就写完了；写路径有
// 三十来条，它们各自要说清楚是哪一栏填错了，所以这里有一张人话表（见下）。
//
// # 四条路径，两个入口函数
//
// 规则与账户各自一个 handler 管「集合」与「单条」两段（靠 restOf 裁出来的 rest 分岔），与
// Payments 那条同一个形状。任务只有一个入口（两段都只读）；渠道没有路径参数，它甚至不查库
// ——能分账的渠道今天写在代码里（catalog.Channel.Settlement）。

const (
	// 三个前缀与 routes/admin.go 里注册时写的字面量**必须一起改**：这里多一点少一点，
	// 裁出来的 rest 就不是 id。渠道那条路径没有参数，所以没有对应的前缀常量。
	adminSettlementRulesPath    = "/v1/admin/settlement/rules"
	adminSettlementAccountsPath = "/v1/admin/settlement/accounts"
	adminSettlementTasksPath    = "/v1/admin/settlement/tasks"
)

// 给用户看的中文句子。与 admin_response.go 里那几条并列，只是这批多得多。
//
// 它们分三类，与下面的错误分组一一对应：**填错了**（400，改一个输入框）、**不合适**
// （409，当前这个局面不接受它，出口往往是停用而不是重来）、**找不到**（404）。
//
// 不写成 err.Error() 的中译：service 的错误串是给日志看的（英文、带 `%w` 链），把它翻一遍
// 弹给用户的做法会让「哪句话是给谁看的」这件事再也分不出来。
const (
	// msgSettlementRuleNotFound / msgSettlementAccountNotFound / msgSettlementTaskNotFound：
	// 三个「查无此物」。**与「这个东西字段是空的」严格分开**——一张全是空字段的详情页看上去
	// 像一条没配好的规则，而真相是打开了一个不存在的 id。
	msgSettlementRuleNotFound    = "分账规则不存在"
	msgSettlementAccountNotFound = "分账账户不存在"
	msgSettlementTaskNotFound    = "分账任务不存在"
	// msgInvalidID：路径里的 id 不是合法 uuid。在进 SQL 之前挡下来，理由见 admin_response.go
	// 的 msgInvalidUserID——不是 uuid 的串会在 PostgreSQL 解析参数时报错，然后以 500 的面目
	// 弹在一个只是拼错了的 URL 上。
	msgInvalidID = "ID 不是合法的 UUID"
	// msgScopeConflict：同一业务分类 + 同一档位 + 同一范围上已经有一条启用中的规则。
	//
	// 这一句必须把**出口**写出来（停用那条旧的，或者换一个档位）：用户看到 409 的第一反应是
	// 「那我再想想」，而真实情况是他只要把旧的那条停用就能配上——这条规则存在的意义就是让
	// 「命中哪一条」是确定的，不是拦着他配。
	msgScopeConflict = "这个业务分类下、同一档位同一范围已经有一条启用中的规则，请先停用那一条（停用后可以再配一条同档位的）"
	// msgRuleInUse / msgAccountInUse：被引用过，删不掉。
	//
	// 两句话都把「改为停用」写出来：这是**常规出口**而不是退路——历史任务上存着当初命中那条
	// 规则的名字与金额，账户的号被当初那笔分账指着，删掉它们会让历史明细指向一个不存在的东西。
	msgRuleInUse    = "这条规则已经被分账任务引用过，删不掉，请改为停用"
	msgAccountInUse = "这个账户已经被规则或历史分账明细引用过，删不掉，请改为停用"
	// msgAccountChannelInUse：改渠道会让引用它的规则**静默少分一笔**（详见仓储的
	// ErrSettlementAccountChannelInUse）。所以这一句必须写出**出口**：用户看到「改不了」的
	// 第一反应是「那就算了」，而真实情况是他只要先改掉那几项就行。
	msgAccountChannelInUse = "这个账户正被启用中的规则引用，改渠道会让那些规则分不到钱，请先改掉引用它的规则（或停用那些规则）再改渠道"
	// msgAccountConflict：同一渠道下这个接收方号已被别的启用账户登记。
	msgAccountConflict = "同一渠道下这个子商户号已经有一条启用的记录了，只能登记一次"
	// msgRuleItemConflict：同一账户在同一条规则里出现了两次。
	msgRuleItemConflict = "同一条规则里同一个账户只能出现一次"
	// msgInvalidBody：请求体不是一个合法的 JSON。
	msgInvalidBody = "请求体格式不正确"
)

// AdminSettlementController 是分账后台的控制器。
//
// 它**不直接拿目录**（与 AdminPaymentController 不同）：那个类自己校验 methodCode 是因为筛选
// 白名单要用到目录，而这里的渠道下拉本身就是一个接口（SettlementChannels），由服务层从目录
// 里取。controller 多拿一份目录只会多一处「两个入口各自读了一次同一份配置」。
type AdminSettlementController struct {
	settlement *service.AdminSettlementService
}

func NewAdminSettlementController(settlement *service.AdminSettlementService) *AdminSettlementController {
	return &AdminSettlementController{settlement: settlement}
}

// ============================================================
// 规则
// ============================================================

// Rules 分发 /v1/admin/settlement/rules 与 .../rules/{id}。
//
// 方法不许的那几种落进 default 的 http.NotFound（与 routes 那边同一条取舍：这套路由表里没有
// 声明 Allow 的地方，回 405 还得自己拼头）。
//
// 多一段的路径（/rules/a/b）也一律 404，不把多出来的那一段当成 id 的一部分去查库——那只会让
// 「有人拼错了 URL」看起来像「这条规则查无此规则」。
func (c *AdminSettlementController) Rules(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := restOf(r.URL.Path, adminSettlementRulesPath)
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listRules(w, r)
	case rest == "" && r.Method == http.MethodPost:
		c.createRule(w, r)
	case rest != "" && !strings.Contains(rest, "/"):
		switch r.Method {
		case http.MethodGet:
			c.getRule(w, r, rest)
		case http.MethodPut:
			c.updateRule(w, r, rest)
		case http.MethodDelete:
			c.deleteRule(w, r, rest)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminSettlementController) listRules(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	// 三个枚举筛选先过白名单再进 SQL，理由见 msgInvalidFilter。
	bizType := strings.TrimSpace(query.Get("bizType"))
	if bizType != "" && !model.IsSettlementBizType(bizType) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}
	scopeType := strings.TrimSpace(query.Get("scopeType"))
	if scopeType != "" && !model.IsSettlementScopeType(scopeType) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !model.IsSettlementRecordStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}

	items, total, err := c.settlement.ListRules(r.Context(), dto.SettlementRuleQuery{
		Name:      strings.TrimSpace(query.Get("name")),
		BizType:   bizType,
		ScopeType: scopeType,
		Status:    status,
		Page:      page,
		PageSize:  pageSize,
	})
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to list settlement rules")
		return
	}
	api.Success(w, api.PageResponse{
		Items: items, Total: int64(total), Page: page, PageSize: pageSize,
	})
}

func (c *AdminSettlementController) getRule(w http.ResponseWriter, r *http.Request, id string) {
	ruleID, ok := parsePathUUID(w, id)
	if !ok {
		return
	}
	rule, err := c.settlement.GetRule(r.Context(), ruleID)
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to read settlement rule")
		return
	}
	api.Success(w, rule)
}

func (c *AdminSettlementController) createRule(w http.ResponseWriter, r *http.Request) {
	var body dto.SettlementRuleInput
	if !decodeBody(w, r, &body) {
		return
	}
	rule, err := c.settlement.CreateRule(r.Context(), body)
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to create settlement rule")
		return
	}
	api.Success(w, rule)
}

// updateRule 整体替换一条规则**连同它的项**（见 service 与 repository 的说明：项是跨行约束的
// 一半，拆成子资源就没法在一个事务里校验完整性）。
func (c *AdminSettlementController) updateRule(w http.ResponseWriter, r *http.Request, id string) {
	ruleID, ok := parsePathUUID(w, id)
	if !ok {
		return
	}
	var body dto.SettlementRuleInput
	if !decodeBody(w, r, &body) {
		return
	}
	rule, err := c.settlement.UpdateRule(r.Context(), ruleID, body)
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to update settlement rule")
		return
	}
	api.Success(w, rule)
}

func (c *AdminSettlementController) deleteRule(w http.ResponseWriter, r *http.Request, id string) {
	ruleID, ok := parsePathUUID(w, id)
	if !ok {
		return
	}
	if err := c.settlement.DeleteRule(r.Context(), ruleID); err != nil {
		writeAdminSettlementError(w, r, err, "failed to delete settlement rule")
		return
	}
	api.Success(w, map[string]any{"id": ruleID})
}

// ============================================================
// 账户
// ============================================================

// Accounts 分发 /v1/admin/settlement/accounts 与 .../accounts/{id}。形状与 Rules 逐条对应。
func (c *AdminSettlementController) Accounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := restOf(r.URL.Path, adminSettlementAccountsPath)
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listAccounts(w, r)
	case rest == "" && r.Method == http.MethodPost:
		c.createAccount(w, r)
	case rest != "" && !strings.Contains(rest, "/"):
		switch r.Method {
		case http.MethodGet:
			c.getAccount(w, r, rest)
		case http.MethodPut:
			c.updateAccount(w, r, rest)
		case http.MethodDelete:
			c.deleteAccount(w, r, rest)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminSettlementController) listAccounts(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	partyType := strings.TrimSpace(query.Get("partyType"))
	if partyType != "" && !model.IsSettlementPartyType(partyType) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !model.IsSettlementRecordStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}

	// provider 不在这里过白名单：它**不是**一份我们维护的词表，而是一个「这几条渠道之一」
	// 的取值，判据在目录里（服务的 settlementChannel）。筛一个不存在的渠道**不该报错**——
	// 它返回空列表恰好就是正确的答案（那条渠道上没有账户），而报错会让「想看看微信上有没有
	// 配过账户」这件事没法问。写入口那一边则必须拦（见 service.buildAccountWrite）。
	items, total, err := c.settlement.ListAccounts(r.Context(), dto.SettlementAccountQuery{
		Keyword:   strings.TrimSpace(query.Get("keyword")),
		PartyType: partyType,
		Provider:  strings.TrimSpace(query.Get("provider")),
		Status:    status,
		Page:      page,
		PageSize:  pageSize,
	})
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to list settlement accounts")
		return
	}
	api.Success(w, api.PageResponse{
		Items: items, Total: int64(total), Page: page, PageSize: pageSize,
	})
}

func (c *AdminSettlementController) getAccount(w http.ResponseWriter, r *http.Request, id string) {
	accountID, ok := parsePathUUID(w, id)
	if !ok {
		return
	}
	account, err := c.settlement.GetAccount(r.Context(), accountID)
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to read settlement account")
		return
	}
	api.Success(w, account)
}

func (c *AdminSettlementController) createAccount(w http.ResponseWriter, r *http.Request) {
	var body dto.SettlementAccountInput
	if !decodeBody(w, r, &body) {
		return
	}
	account, err := c.settlement.CreateAccount(r.Context(), body)
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to create settlement account")
		return
	}
	api.Success(w, account)
}

func (c *AdminSettlementController) updateAccount(w http.ResponseWriter, r *http.Request, id string) {
	accountID, ok := parsePathUUID(w, id)
	if !ok {
		return
	}
	var body dto.SettlementAccountInput
	if !decodeBody(w, r, &body) {
		return
	}
	account, err := c.settlement.UpdateAccount(r.Context(), accountID, body)
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to update settlement account")
		return
	}
	api.Success(w, account)
}

func (c *AdminSettlementController) deleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	accountID, ok := parsePathUUID(w, id)
	if !ok {
		return
	}
	if err := c.settlement.DeleteAccount(r.Context(), accountID); err != nil {
		writeAdminSettlementError(w, r, err, "failed to delete settlement account")
		return
	}
	api.Success(w, map[string]any{"id": accountID})
}

// ============================================================
// 任务（只读）
// ============================================================

// Tasks 分发 /v1/admin/settlement/tasks 与 .../tasks/{id}。两段都只有 GET。
func (c *AdminSettlementController) Tasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := restOf(r.URL.Path, adminSettlementTasksPath)
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listTasks(w, r)
	case rest != "" && r.Method == http.MethodGet && !strings.Contains(rest, "/"):
		c.getTask(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminSettlementController) listTasks(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	status := strings.TrimSpace(query.Get("status"))
	// 任务状态的白名单**只认本服务写的那三个**（model 里那组常量的说明）：submitted /
	// failed / returned 是给微信四步那条路与将来留的，今天库里不可能有，但**也不拦**——
	// 拦了的话，等真有人写进去时这一句会成为「筛不出来」的现场。这里拦的是拼错的值。
	if status != "" && !model.IsSettlementTaskStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}
	scopeType := strings.TrimSpace(query.Get("scopeType"))
	if scopeType != "" && !model.IsSettlementScopeType(scopeType) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidFilter)
		return
	}
	createdFrom, createdTo, ok := timeRange(query.Get("createdFrom"), query.Get("createdTo"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidDate)
		return
	}
	// 三个主体维度都是 uuid 快照列，非 uuid 的值会让 PostgreSQL 在解析参数时报错（500）。
	// 与 userId 那条同一个处置。
	storeRef, ok := parseOptionalUUID(query.Get("storeRef"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidID)
		return
	}
	brandRef, ok := parseOptionalUUID(query.Get("brandRef"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidID)
		return
	}
	merchantRef, ok := parseOptionalUUID(query.Get("merchantRef"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidID)
		return
	}

	items, total, err := c.settlement.ListTasks(r.Context(), dto.SettlementTaskQuery{
		TaskNo:      strings.TrimSpace(query.Get("taskNo")),
		PaymentNo:   strings.TrimSpace(query.Get("paymentNo")),
		OrderNo:     strings.TrimSpace(query.Get("orderNo")),
		Status:      status,
		ScopeType:   scopeType,
		StoreRef:    storeRef,
		BrandRef:    brandRef,
		MerchantRef: merchantRef,
		CreatedFrom: createdFrom,
		CreatedTo:   createdTo,
		Page:        page,
		PageSize:    pageSize,
	})
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to list settlement tasks")
		return
	}
	api.Success(w, api.PageResponse{
		Items: items, Total: int64(total), Page: page, PageSize: pageSize,
	})
}

func (c *AdminSettlementController) getTask(w http.ResponseWriter, r *http.Request, id string) {
	taskID, ok := parsePathUUID(w, id)
	if !ok {
		return
	}
	detail, err := c.settlement.GetTask(r.Context(), taskID)
	if err != nil {
		writeAdminSettlementError(w, r, err, "failed to read settlement task")
		return
	}
	api.Success(w, detail)
}

// ============================================================
// 渠道
// ============================================================

// Channels 给出「分账能挂在哪几条渠道上」，给账户表单的渠道下拉用。
//
// 它**不查库**：能分账的渠道由「哪条渠道的下单报文里能带子单」决定，那是发版的事（见
// catalog.SettlementChannels）。也正因为它不查库，这里没有分页、没有筛选——它是三个值。
func (c *AdminSettlementController) Channels(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	api.Success(w, c.settlement.SettlementChannels())
}

// ============================================================
// 管道
// ============================================================

// parsePathUUID 读路径里的 id。不合法时已经把响应写好了（第二个返回值为 false）。
//
// 与 parseOptionalUUID 分开：那个的语义是「空 = 没筛」，这个的语义是「路径里必须有且只有一个
// uuid」。合成一个之后，「/rules/（空 id）」会落进「没筛」那一支，然后被当成列表查询——
// 一条 PUT 请求变成了对集合的写。
func parsePathUUID(w http.ResponseWriter, id string) (string, bool) {
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidID)
		return "", false
	}
	return parsed.String(), true
}

// maxSettlementBodyBytes 是一次写请求的体积上限，1 MiB。
//
// 一条规则最多 20 项（service.MaxSettlementRuleItems），连备注带账户 id 一起算也就几 KB。
// 给到 1 MiB 是因为这里要防的**不是**「填得太多」，而是把请求体当水管用：没有上限时，一次
// 请求就能让服务把内存读满，而这几条路由是**能写库**的那几条。取值与 coupon-service 的
// decodeBody 一致。
//
// 超限与「不是合法 JSON」回的是同一句 400：两者都是「这个请求体我们不收」，而超限那句英文
// （`http: request body too large`）同样不该弹给用户。区别在日志里——decoder 的原话带着它。
const maxSettlementBodyBytes = 1 << 20

// decodeBody 解一个 JSON 请求体。失败时已经把 400 写好了（返回 false）。
//
// **不把 decoder 的错误串放进响应**：它是英文的（`json: cannot unmarshal string into Go
// struct field ... of type int64`），而里面带着 Go 的结构体字段名——那既不是给用户看的，
// 也不该出现在响应体里。这句话进日志（下面 slog），用户看到的是一句「请求体格式不正确」。
func decodeBody(w http.ResponseWriter, r *http.Request, body any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettlementBodyBytes)).Decode(body); err != nil {
		slog.WarnContext(r.Context(), "settlement admin request body is not valid json", "error", err)
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return false
	}
	return true
}

// settlementConflictErrors 是「请求本身没问题，是现在这个局面不接受它」的那一组（409）。
//
// 四类，都是**撞上了库里的现状**而不是填错了哪一栏：
//
//   - 同档位已有启用中的规则（部分唯一索引 settlement_rules_scope_uniq）
//   - 同一渠道下接收方号重复（settlement_accounts_receiver_uniq，账户表上唯一的一条唯一约束）
//   - 同一账户在同一条规则里出现两次（部分唯一索引 settlement_rule_items_account_uniq，
//     与上面那条 400 的 ErrSettlementRuleItemAccountTwice 是同一件事的两个入口：service 先
//     拦一道，漏到库上还有索引兜着）
//   - 删一条被任务/规则/历史明细引用着的行（三处 ON DELETE RESTRICT）
//
// **与「请求不合法」的分界**：这一组全部由库里的现状决定（同一个请求换一个时间点可能成功），
// 而那一组只由请求体本身决定（怎么重试都一样）。按这条分，账户被停用（service 的
// ErrSettlementRuleItemAccountDisabled）落在 400 那一组——换一个账户就好了，重试是徒劳的。
var settlementConflictErrors = []error{
	repository.ErrSettlementScopeConflict,
	repository.ErrSettlementAccountConflict,
	repository.ErrSettlementRuleItemConflict,
	repository.ErrSettlementRuleInUse,
	repository.ErrSettlementAccountInUse,
	// 改账户的渠道，而它正被启用中的规则引用着——外键只管删除，这个局面只有仓储自己看得出来。
	repository.ErrSettlementAccountChannelInUse,
}

// settlementNotFoundErrors 是三个「查无此物」（404）。
var settlementNotFoundErrors = []error{
	repository.ErrSettlementRuleNotFound,
	repository.ErrSettlementAccountNotFound,
	// 任务那条是**自己**的哨兵，不是支付单那个：GetSettlementTask 在扫描那一步就把
	// pgx.ErrNoRows 翻成了 ErrSettlementTaskNotFound。**裸的 pgx.ErrNoRows 不在这里**，
	// 也不该在——它一旦漏到这里，用户看到的是 500 加一句「服务暂时不可用」。
	repository.ErrSettlementTaskNotFound,
}

// settlementUserMessages 是「错误值 → 用户看到的那句话」。
//
// 表驱动而不是把这个 switch 写在 writeAdminSettlementError 里：controller 的测试要**逐条**
// 核对这张表覆盖了 service.SettlementValidationErrors 的每一条（漏一条的后果是英文串原样
// 弹给运营），而测试只能遍历一张数据，遍历不了一个 switch。
//
// 顺序有意义：同一个错误值不会出现在两行里，但 ErrSettlementAccountConflict 那一条要放在
// 更具体的那些**之后**（它是个兜底，见 repository.mapSettlementError 里 23505 的 default 分支）
// ——今天不会撞，将来加一条更具体的唯一约束时能直接插在它前面。
var settlementUserMessages = []struct {
	err  error
	text string
}{
	// —— 规则那一半 ——
	{service.ErrSettlementRuleNameRequired, "规则名称必填"},
	{service.ErrSettlementRuleBizTypeInvalid, "业务分类不是合法的取值"},
	{service.ErrSettlementRuleScopeTypeInvalid, "范围档位不是合法的取值"},
	{service.ErrSettlementScopeRefRequired, "这个档位必须指定范围（门店 / 设备 / 品牌的 ID）"},
	{service.ErrSettlementScopeRefNotAllowed, "全局档位不能指定范围"},
	{service.ErrSettlementScopeRefInvalid, "范围 ID 不是合法的 UUID"},
	{service.ErrSettlementRuleModeInvalid, "分配模式不是合法的取值"},
	{service.ErrSettlementRuleStatusInvalid, "规则状态只能是启用或停用"},
	{service.ErrSettlementRuleRemarkTooLong, "名称或备注太长了"},
	{service.ErrSettlementRuleItemsRequired, "规则至少要有一项；想让整单归平台，请明确加一条平台自留项"},
	{service.ErrSettlementRuleItemsTooMany, "规则的分账项太多了"},

	// —— 规则项 ——
	{service.ErrSettlementRuleItemPartyInvalid, "分账项的主体类型不是合法的取值"},
	{service.ErrSettlementRuleItemCalcInvalid, "分账项的算法不是合法的取值（平台自留项只能配成差额自留）"},
	{service.ErrSettlementRuleItemRatioInvalid, "比例项的比例必须在 0 与 100 之间（不含 0），最多两位小数"},
	{service.ErrSettlementRuleItemFixedInvalid, "固定额项的金额必须大于 0，且不能同时填比例"},
	{service.ErrSettlementRuleItemRemainderGiven, "平台自留项不能填比例或固定额，它拿的是差额"},
	{service.ErrSettlementRuleItemPlatformAccount, "平台自留项不能指定收款账户"},
	{service.ErrSettlementRuleItemAccountRequired, "每一项都要指定收款账户"},
	{service.ErrSettlementRuleItemAccountInvalid, "收款账户的 ID 不是合法的 UUID"},
	// 这一条是后台写入口最该拦下的一条：computeSettlement 遇到它会**静默整单归平台**
	// （见 service 的说明），配错的人看不到任何报错。
	{service.ErrSettlementRuleItemRatioOverflow, "所有比例项加起来不能超过 100%"},
	{service.ErrSettlementRuleItemPlatformTwice, "一条规则里只能有一个平台自留项"},
	{service.ErrSettlementRuleItemAccountTwice, "同一条规则里同一个账户只能出现一次"},
	{service.ErrSettlementRuleItemAccountDisabled, "所选账户已停用，请换一个（停用的账户不参与新的分账）"},
	// 这一条要说「重新选」：用户手上那份下拉是打开表单那一刻取的，而账户可能刚在另一处被删掉。
	{service.ErrSettlementRuleItemAccountMissing, "这一项挂的账户不存在（可能已在别处被删除），请重新选择"},
	{service.ErrSettlementRuleItemAccountChannel, "一条规则里的账户必须在同一条渠道上，否则其中一部分在任何一笔支付上都分不到钱"},

	// —— 账户 ——
	{service.ErrSettlementAccountPartyNameRequired, "主体名必填"},
	{service.ErrSettlementAccountPartyInvalid, "主体类型不是合法的取值"},
	{service.ErrSettlementAccountProviderInvalid, "渠道不是可用的分账渠道"},
	{service.ErrSettlementAccountReceiverTypeInvalid, "接收方类型不是合法的取值"},
	{service.ErrSettlementAccountReceiverRequired, "渠道侧的子商户号必填"},
	{service.ErrSettlementAccountStatusInvalid, "账户状态只能是启用或停用"},
	{service.ErrSettlementAccountFieldTooLong, "有字段太长了"},

	// —— 局面决定的（409）——
	{repository.ErrSettlementScopeConflict, msgScopeConflict},
	{repository.ErrSettlementRuleInUse, msgRuleInUse},
	{repository.ErrSettlementAccountInUse, msgAccountInUse},
	{repository.ErrSettlementAccountChannelInUse, msgAccountChannelInUse},
	{repository.ErrSettlementAccountConflict, msgAccountConflict},
	{repository.ErrSettlementRuleItemConflict, msgRuleItemConflict},

	// —— 查无此物（404）——
	{repository.ErrSettlementRuleNotFound, msgSettlementRuleNotFound},
	{repository.ErrSettlementAccountNotFound, msgSettlementAccountNotFound},
	{repository.ErrSettlementTaskNotFound, msgSettlementTaskNotFound},

	// —— 兜底 ——
	//
	// 这条不该出现：service 层逐列校验过，uuid 也 parse 过。它落在 400 而不是 500，是因为走到
	// 这里的一定是「这份数据表不收」，那对用户来说是「你填的不对」（虽然我们没告诉他是哪一栏），
	// 而不是「服务坏了，等会儿再试」。**日志里有 PostgreSQL 的原话**。
	{repository.ErrSettlementConstraintViolation, "有字段不符合要求，请检查后重试"},
}

// writeAdminSettlementError 把服务层的错误翻成响应。
//
// 这一层是 HTTP 边界，也是唯一同时知道「这是哪一条错误」和「这句话是给谁看的」的地方——
// 与 writeAdminPaymentError 同一个分工，只是那一半只有一条错误值、这一半有三十来条。
//
// 兜底那一支**必须记日志**：它对用户说「服务暂时不可用」，只有日志里才有真正的原因。
func writeAdminSettlementError(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	// —— 填错了（400）——
	case errors.Is(err, repository.ErrSettlementConstraintViolation) || service.IsValidationError(err):
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", settlementMessage(r.Context(), err))

	// —— 找不到（404）——
	case matchesAny(err, settlementNotFoundErrors):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, settlementMessage(r.Context(), err))

	// —— 局面不接受（409）——
	case matchesAny(err, settlementConflictErrors):
		api.Error(w, http.StatusConflict, api.CodeConflict, settlementMessage(r.Context(), err))

	default:
		slog.ErrorContext(r.Context(), "settlement admin request failed", "error", err, "op", fallback)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, msgGeneric)
	}
}

// matchesAny 判断 err 是不是这一组里的某一个。
func matchesAny(err error, group []error) bool {
	for _, candidate := range group {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

// settlementMessage 取人话，取不到就兜底并记一条 warn。
//
// 记 warn 而不是 error：漏一条表不该让一次请求以 500 收场，那是两件事。但它在日志里必须找得到
// ——不然后台只会说「服务暂时不可用」，谁也看不出漏的是哪一条（admin_settlement_test.go 就是
// 为了让它不可能漏）。
func settlementMessage(ctx context.Context, err error) string {
	for _, entry := range settlementUserMessages {
		if errors.Is(err, entry.err) {
			return entry.text
		}
	}
	slog.WarnContext(ctx, "settlement error has no user-facing message", "error", err)
	return msgGeneric
}
