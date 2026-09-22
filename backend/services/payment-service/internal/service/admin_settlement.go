package service

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 分账后台的服务层：规则与账户的写入口，以及任务那一半的只读透传。
//
// # 这一层为什么是厚的（与隔壁 AdminQueryService 正相反）
//
// AdminQueryService 是纯透传——支付单没有可写的东西，页面上没有一条规则需要判断。
// 这里不一样：008 把能钉在表上的约束都钉住了，但**跨行的那几条钉不住**，而它们恰好是配错了
// 不报错、只是钱分错地方的那几条：
//
//   - 同一条规则下 percent 项的 ratio 合计超过 1：computeSettlement 遇到它会**静默整单归
//     平台**（那条「分出去的钱比收进来的多」的兜底）。配错的人和收到钱的人都看不见异常。
//   - 非平台项挂了一个**停用的**或者**别的渠道的**账户：执行期那个 LEFT JOIN 会把这一项滤掉
//     （见 repository.settlementRuleItems 的 JOIN 条件），表现同样是静默少分一笔。
//   - 非 global 档位给了一个不是 uuid 的 scope_ref：值原样进 SQL 时 PostgreSQL 在解析参数时
//     报错，一个填错的搜索框会以「服务出错」的面目弹在运营脸上。
//
// 所以校验全在这里，一条都不往下推。往下推的那些（撞唯一索引、删被引用的行）是**局面**决定的，
// 由仓储翻成哨兵错误再上来。
//
// # 错误串为什么是英文
//
// 与全仓一致：service 的错误串是给日志看的（`fmt.Errorf("...: %w", err)` 那一串），用户看到的
// 中文在 controller 的人话表里翻。见 controller/admin_settlement.go。

// 写入口的校验错误。它们都是「这份请求本身不成立」，与库里现在是什么局面无关——所以归 400，
// 与任务/规则查无此行的 404、被引用删不掉的 409 分开。
//
// 逐条起名字而不是共用一个 ErrInvalidInput：页面上要说清楚**是哪一栏**填错了，而一句话
// 「参数不合法」会让人把整张表单重看一遍。名字即文案的索引（见 controller 的人话表）。
var (
	// —— 规则本身 ——
	ErrSettlementRuleNameRequired     = errors.New("settlement rule name is required")
	ErrSettlementRuleBizTypeInvalid   = errors.New("settlement rule biz_type is not in the vocabulary")
	ErrSettlementRuleScopeTypeInvalid = errors.New("settlement rule scope_type is not in the vocabulary")
	// ErrSettlementScopeRefRequired / ErrSettlementScopeRefNotAllowed：008 的 CHECK 把
	// scope_type='global' ⇔ scope_ref='' 钉死了，两个方向都要在这儿拦——反过来那一半（global
	// 却指着一个门店）如果漏到 SQL 上，是一条 CHECK 违规，用户看到的是一句没有线索的兜底。
	ErrSettlementScopeRefRequired   = errors.New("a non-global settlement rule needs a scope_ref")
	ErrSettlementScopeRefNotAllowed = errors.New("a global settlement rule must not carry a scope_ref")
	ErrSettlementScopeRefInvalid    = errors.New("settlement rule scope_ref is not a valid uuid")
	ErrSettlementRuleModeInvalid    = errors.New("settlement rule allocation_mode is not in the vocabulary")
	ErrSettlementRuleStatusInvalid  = errors.New("settlement rule status is not enabled/disabled")
	ErrSettlementRuleRemarkTooLong  = errors.New("settlement rule remark is too long")

	// —— 规则项 ——
	ErrSettlementRuleItemsRequired       = errors.New("a settlement rule needs at least one item")
	ErrSettlementRuleItemsTooMany        = errors.New("a settlement rule has too many items")
	ErrSettlementRuleItemPartyInvalid    = errors.New("settlement rule item party_type is not in the vocabulary")
	ErrSettlementRuleItemCalcInvalid     = errors.New("settlement rule item calc_type is not in the vocabulary")
	ErrSettlementRuleItemRatioInvalid    = errors.New("a percent item needs a ratio in (0,100] with at most two decimals")
	ErrSettlementRuleItemFixedInvalid    = errors.New("a fixed item needs a positive fixed_amount")
	ErrSettlementRuleItemRemainderGiven  = errors.New("a remainder item must carry neither ratio nor fixed_amount")
	ErrSettlementRuleItemPlatformAccount = errors.New("a platform item must not carry an account")
	ErrSettlementRuleItemAccountRequired = errors.New("a non-platform item needs an account")
	ErrSettlementRuleItemAccountInvalid  = errors.New("settlement rule item account_id is not a valid uuid")
	ErrSettlementRuleItemRatioOverflow   = errors.New("the percent items of one rule add up to more than 100%")
	ErrSettlementRuleItemPlatformTwice   = errors.New("a rule can carry at most one platform item")
	ErrSettlementRuleItemAccountTwice    = errors.New("the same account appears twice in one rule")
	ErrSettlementRuleItemAccountDisabled = errors.New("the account of a rule item is disabled")
	// ErrSettlementRuleItemAccountMissing：这一项挂的账户库里没有。
	//
	// 与「查无此账户」（GET /accounts/{id} 的 404）**分开**：那个是用户打开了一个不存在的 id，
	// 这个是**这份规则本身不成立**——他多半是把账户表单开在另一个标签页里，那边把这一条删了。
	// 换一个账户就能提交，重试多少次都一样，所以它跟「账户已停用」同一档（400）。
	ErrSettlementRuleItemAccountMissing = errors.New("the account of a rule item does not exist")
	ErrSettlementRuleItemAccountChannel = errors.New("the accounts of one rule must all be on the same channel")

	// —— 账户 ——
	ErrSettlementAccountPartyNameRequired   = errors.New("settlement account party_name is required")
	ErrSettlementAccountPartyInvalid        = errors.New("settlement account party_type is not in the vocabulary")
	ErrSettlementAccountProviderInvalid     = errors.New("settlement account provider is not a settlement-capable channel")
	ErrSettlementAccountReceiverTypeInvalid = errors.New("settlement account receiver_type is not in the vocabulary")
	ErrSettlementAccountReceiverRequired    = errors.New("settlement account receiver_id is required")
	ErrSettlementAccountStatusInvalid       = errors.New("settlement account status is not enabled/disabled")
	ErrSettlementAccountFieldTooLong        = errors.New("a settlement account field is too long")
)

// SettlementValidationErrors 是分账后台这一批「请求不合法」。
//
// 它是 service.go 里那个 ValidationErrors 全集的一部分（那个 var 把它一并收了进去），在这里
// 单独列一份是因为 controller 的人话表要**逐条**核对这一批——支付那几条错误走的是别的出口，
// 它们的文案不在这张表里。
//
// 单独列而不是让 controller 逐个 case：新增一条校验就要在 controller 里同步加一个 case，
// 那是必然漏掉的写法（漏掉的后果是英文错误串原样弹给用户）。
var SettlementValidationErrors = []error{
	ErrSettlementRuleNameRequired, ErrSettlementRuleBizTypeInvalid,
	ErrSettlementRuleScopeTypeInvalid, ErrSettlementScopeRefRequired,
	ErrSettlementScopeRefNotAllowed, ErrSettlementScopeRefInvalid,
	ErrSettlementRuleModeInvalid, ErrSettlementRuleStatusInvalid, ErrSettlementRuleRemarkTooLong,
	ErrSettlementRuleItemsRequired, ErrSettlementRuleItemsTooMany,
	ErrSettlementRuleItemPartyInvalid, ErrSettlementRuleItemCalcInvalid,
	ErrSettlementRuleItemRatioInvalid, ErrSettlementRuleItemFixedInvalid,
	ErrSettlementRuleItemRemainderGiven, ErrSettlementRuleItemPlatformAccount,
	ErrSettlementRuleItemAccountRequired, ErrSettlementRuleItemAccountInvalid,
	ErrSettlementRuleItemRatioOverflow, ErrSettlementRuleItemPlatformTwice,
	ErrSettlementRuleItemAccountTwice, ErrSettlementRuleItemAccountChannel,
	ErrSettlementRuleItemAccountDisabled, ErrSettlementRuleItemAccountMissing,
	ErrSettlementAccountPartyNameRequired, ErrSettlementAccountPartyInvalid,
	ErrSettlementAccountProviderInvalid, ErrSettlementAccountReceiverTypeInvalid,
	ErrSettlementAccountReceiverRequired, ErrSettlementAccountStatusInvalid,
	ErrSettlementAccountFieldTooLong,
}

const (
	// MaxSettlementRuleItems 是一条规则的项数上限。
	//
	// 分账的接收方是个位数的事实（一家门店 + 一个城市中心 + 平台），20 已经宽到不可能误伤；
	// 它防的是「前端循环出错，一次提交进来一万项」——那会让下面那条 percent 合计的校验跟着
	// 变成一万次循环，而更糟的是规则一旦存下，每次发起支付都要按它算一遍。
	MaxSettlementRuleItems = 20
	// MaxSettlementRemarkLength 是备注的长度上限，按**字符**算。与 membership-service 的
	// MaxReasonLength 同一条道理：中文一个字符三字节，按字节限会让运营写到一半被拒。
	MaxSettlementRemarkLength = 200
	// MaxSettlementNameLength 是给用户看的短文本的上限（主体名、规则名）。取值跟着页面上输入框
	// 的 maxLength 走，两边一起改——账户表单与规则表单上那两栏都写 60。
	MaxSettlementNameLength = 60
	// MaxSettlementReceiverIDLength 是渠道侧子商户号的长度上限。
	MaxSettlementReceiverIDLength = 64
)

// AdminSettlementRepository 是分账后台要的那一组读写方法。
//
// 与 AdminQueryRepository 分开声明：那两个接口一个只读、一个只写，混在一起之后「后台的只读
// 那一半不碰任何写依赖」这件事从类型上就看不出来了。
type AdminSettlementRepository interface {
	ListSettlementAccounts(ctx context.Context, q dto.SettlementAccountQuery) ([]*repository.SettlementAccountRow, int, error)
	GetSettlementAccount(ctx context.Context, id string) (*repository.SettlementAccountRow, error)
	GetSettlementAccounts(ctx context.Context, ids []string) ([]*repository.SettlementAccountRow, error)
	CreateSettlementAccount(ctx context.Context, in repository.SettlementAccountWrite) (*repository.SettlementAccountRow, error)
	UpdateSettlementAccount(ctx context.Context, id string, in repository.SettlementAccountWrite) (*repository.SettlementAccountRow, error)
	DeleteSettlementAccount(ctx context.Context, id string) error

	ListSettlementRules(ctx context.Context, q dto.SettlementRuleQuery) ([]*repository.SettlementRuleRow, int, error)
	GetSettlementRule(ctx context.Context, id string) (*repository.SettlementRuleRow, error)
	CreateSettlementRule(ctx context.Context, in repository.SettlementRuleWrite) (*repository.SettlementRuleRow, error)
	UpdateSettlementRule(ctx context.Context, id string, in repository.SettlementRuleWrite) (*repository.SettlementRuleRow, error)
	DeleteSettlementRule(ctx context.Context, id string) error

	ListSettlementTasks(ctx context.Context, q dto.SettlementTaskQuery) ([]*repository.SettlementTaskRow, int, error)
	GetSettlementTask(ctx context.Context, id string) (*repository.SettlementTaskDetail, error)
}

// AdminSettlementService 是分账后台的服务层。
//
// 它要一份目录，只为一件事：账户上的渠道取值必须是**能把钱分出去的**那几条（见
// AccountsChannels）。这件事不是数据库能回答的——渠道今天写在代码里。
type AdminSettlementService struct {
	repository AdminSettlementRepository
	catalog    *catalog.Catalog
}

func NewAdminSettlementService(repository AdminSettlementRepository, directory *catalog.Catalog) *AdminSettlementService {
	return &AdminSettlementService{repository: repository, catalog: directory}
}

// ============================================================
// 渠道（给账户表单的下拉）
// ============================================================

// SettlementChannels 列出分账能挂上去的渠道。
//
// 它读的是**代码里的目录**（catalog.Channel.Settlement），不是一张表：能分账的渠道由「哪条
// 渠道的下单报文里能带子单」决定，那是发版的事，不是运营的输入。
func (s *AdminSettlementService) SettlementChannels() []dto.SettlementChannel {
	channels := s.catalog.SettlementChannels()
	items := make([]dto.SettlementChannel, 0, len(channels))
	for _, channel := range channels {
		items = append(items, dto.SettlementChannel{
			Code: channel.Code, Name: channel.Name, Provider: channel.Provider,
		})
	}
	return items
}

// settlementProvider 判断一个渠道名是不是可分账的渠道，并把它的展示名带回来。
//
// 拿 Provider 比而不是 Code：账户上存的是 Provider（见 settlement_accounts.provider 的列
// 注释）。今天这两个值相等（渠道代码就是适配器注册名），但那是巧合——将来接一条同族渠道
// （同一个适配器、另一个商户号）时就会分岔，而账户上要存的一直是「谁来发这份报文」那一个。
func (s *AdminSettlementService) settlementChannel(provider string) (catalog.Channel, error) {
	for _, channel := range s.catalog.SettlementChannels() {
		if channel.Provider == provider {
			return channel, nil
		}
	}
	return catalog.Channel{}, ErrSettlementAccountProviderInvalid
}

// ============================================================
// 账户
// ============================================================

func (s *AdminSettlementService) ListAccounts(ctx context.Context, q dto.SettlementAccountQuery) ([]dto.SettlementAccount, int, error) {
	rows, total, err := s.repository.ListSettlementAccounts(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	items := make([]dto.SettlementAccount, 0, len(rows))
	for _, row := range rows {
		items = append(items, settlementAccountDTO(row))
	}
	return items, total, nil
}

func (s *AdminSettlementService) GetAccount(ctx context.Context, id string) (*dto.SettlementAccount, error) {
	row, err := s.repository.GetSettlementAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	account := settlementAccountDTO(row)
	return &account, nil
}

func (s *AdminSettlementService) CreateAccount(ctx context.Context, in dto.SettlementAccountInput) (*dto.SettlementAccount, error) {
	write, err := s.buildAccountWrite(in)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.CreateSettlementAccount(ctx, write)
	if err != nil {
		return nil, err
	}
	account := settlementAccountDTO(row)
	return &account, nil
}

func (s *AdminSettlementService) UpdateAccount(ctx context.Context, id string, in dto.SettlementAccountInput) (*dto.SettlementAccount, error) {
	write, err := s.buildAccountWrite(in)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.UpdateSettlementAccount(ctx, id, write)
	if err != nil {
		return nil, err
	}
	account := settlementAccountDTO(row)
	return &account, nil
}

func (s *AdminSettlementService) DeleteAccount(ctx context.Context, id string) error {
	return s.repository.DeleteSettlementAccount(ctx, id)
}

// buildAccountWrite 校验并归一化一份账户写入。
//
// 归一化（而不是把原样值写进去）的两处：渠道与接收方类型。`provider` 收的是渠道名，页面下拉
// 给的就是它，但**接口不能假设调用方只可能是那个页面**；`receiver_type` 空表示 MERCHANT_ID
// ——银联商务按子商户号直接分，那是最常见的一种，008 的列默认值也是它。
func (s *AdminSettlementService) buildAccountWrite(in dto.SettlementAccountInput) (repository.SettlementAccountWrite, error) {
	write := repository.SettlementAccountWrite{}

	write.PartyType = strings.TrimSpace(in.PartyType)
	if !model.IsSettlementPartyType(write.PartyType) {
		return write, ErrSettlementAccountPartyInvalid
	}
	// 主体名必填（017）：账户号那列没了，它是这一行**唯一**给人看的名字——规则项的账户下拉、
	// 分账接收方快照、账户列表用的都是它。空着的话这一行在页面上只剩一个子商户号，
	// 而 017 的 CHECK 也会把空值挡在库外。
	write.PartyName = strings.TrimSpace(in.PartyName)
	switch {
	case write.PartyName == "":
		return write, ErrSettlementAccountPartyNameRequired
	case len([]rune(write.PartyName)) > MaxSettlementNameLength:
		return write, ErrSettlementAccountFieldTooLong
	}

	// 渠道：必须在可分账的那几条里（见 settlementChannel）。挡在这里而不是等执行期那个
	// JOIN 静默滤掉，是这一条能救回真金白银的地方——挂错渠道的账户配出来就会一直分不到钱，
	// 而且页面上一切正常。
	write.Provider = strings.TrimSpace(in.Provider)
	if _, err := s.settlementChannel(write.Provider); err != nil {
		return write, err
	}

	write.ReceiverType = strings.TrimSpace(in.ReceiverType)
	if write.ReceiverType == "" {
		write.ReceiverType = model.SettlementReceiverMerchantID
	}
	if !model.IsSettlementReceiverType(write.ReceiverType) {
		return write, ErrSettlementAccountReceiverTypeInvalid
	}
	write.ReceiverID = strings.TrimSpace(in.ReceiverID)
	switch {
	case write.ReceiverID == "":
		return write, ErrSettlementAccountReceiverRequired
	case len([]rune(write.ReceiverID)) > MaxSettlementReceiverIDLength:
		return write, ErrSettlementAccountFieldTooLong
	}
	// 状态空 = enabled：新建的账户默认可用（与 008 的列默认值一致）。停用是一个明确动作。
	write.Status = strings.TrimSpace(in.Status)
	if write.Status == "" {
		write.Status = model.SettlementRecordEnabled
	}
	if !model.IsSettlementRecordStatus(write.Status) {
		return write, ErrSettlementAccountStatusInvalid
	}

	write.Remark = strings.TrimSpace(in.Remark)
	if len([]rune(write.Remark)) > MaxSettlementRemarkLength {
		return write, ErrSettlementAccountFieldTooLong
	}
	return write, nil
}

// ============================================================
// 规则
// ============================================================

func (s *AdminSettlementService) ListRules(ctx context.Context, q dto.SettlementRuleQuery) ([]dto.SettlementRule, int, error) {
	rows, total, err := s.repository.ListSettlementRules(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	items := make([]dto.SettlementRule, 0, len(rows))
	for _, row := range rows {
		items = append(items, settlementRuleDTO(row))
	}
	return items, total, nil
}

func (s *AdminSettlementService) GetRule(ctx context.Context, id string) (*dto.SettlementRule, error) {
	row, err := s.repository.GetSettlementRule(ctx, id)
	if err != nil {
		return nil, err
	}
	rule := settlementRuleDTO(row)
	return &rule, nil
}

func (s *AdminSettlementService) CreateRule(ctx context.Context, in dto.SettlementRuleInput) (*dto.SettlementRule, error) {
	write, err := s.buildRuleWrite(ctx, in)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.CreateSettlementRule(ctx, write)
	if err != nil {
		return nil, err
	}
	rule := settlementRuleDTO(row)
	return &rule, nil
}

func (s *AdminSettlementService) UpdateRule(ctx context.Context, id string, in dto.SettlementRuleInput) (*dto.SettlementRule, error) {
	write, err := s.buildRuleWrite(ctx, in)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.UpdateSettlementRule(ctx, id, write)
	if err != nil {
		return nil, err
	}
	rule := settlementRuleDTO(row)
	return &rule, nil
}

func (s *AdminSettlementService) DeleteRule(ctx context.Context, id string) error {
	return s.repository.DeleteSettlementRule(ctx, id)
}

// buildRuleWrite 校验一份规则写入，并把它归一化成仓储要的形状。
//
// 校验的顺序是**从外到内**：先把规则本身那几栏定下来，再逐项查，最后把所有项放在一起查。
// 「最后」那一批是跨行的——合计比例、平台项至多一条、同一账户至多一次、账户必须同渠道——
// 它们里的任何一条都不是逐项能看出来的。
func (s *AdminSettlementService) buildRuleWrite(ctx context.Context, in dto.SettlementRuleInput) (repository.SettlementRuleWrite, error) {
	write := repository.SettlementRuleWrite{}

	write.Name = strings.TrimSpace(in.Name)
	if write.Name == "" {
		return write, ErrSettlementRuleNameRequired
	}
	if len([]rune(write.Name)) > MaxSettlementNameLength {
		return write, ErrSettlementRuleRemarkTooLong
	}
	write.BizType = strings.TrimSpace(in.BizType)
	if !model.IsSettlementBizType(write.BizType) {
		return write, ErrSettlementRuleBizTypeInvalid
	}

	write.ScopeType = strings.TrimSpace(in.ScopeType)
	if !model.IsSettlementScopeType(write.ScopeType) {
		return write, ErrSettlementRuleScopeTypeInvalid
	}
	// global 与非 global 的 scope_ref 是**两个方向都要拦**的（008 的 CHECK 是个等价式）：
	// 少了任何一个方向，用户拿到的都是一句没有线索的「配置不合法」。
	scopeRef := strings.TrimSpace(in.ScopeRef)
	if write.ScopeType == model.SettlementScopeGlobal {
		if scopeRef != "" {
			return write, ErrSettlementScopeRefNotAllowed
		}
		write.ScopeRef = ""
	} else {
		if scopeRef == "" {
			return write, ErrSettlementScopeRefRequired
		}
		parsed, err := uuid.Parse(scopeRef)
		if err != nil {
			return write, ErrSettlementScopeRefInvalid
		}
		write.ScopeRef = parsed.String()
	}

	write.AllocationMode = strings.TrimSpace(in.AllocationMode)
	if write.AllocationMode == "" {
		write.AllocationMode = model.SettlementAllocationNormal
	}
	if !model.IsSettlementAllocationMode(write.AllocationMode) {
		return write, ErrSettlementRuleModeInvalid
	}

	write.Status = strings.TrimSpace(in.Status)
	if write.Status == "" {
		write.Status = model.SettlementRecordEnabled
	}
	if !model.IsSettlementRecordStatus(write.Status) {
		return write, ErrSettlementRuleStatusInvalid
	}

	write.Remark = strings.TrimSpace(in.Remark)
	if len([]rune(write.Remark)) > MaxSettlementRemarkLength {
		return write, ErrSettlementRuleRemarkTooLong
	}

	switch {
	case len(in.Items) == 0:
		// 一条没有项的规则等于「这条业务整单归平台」，与「这个档位没有规则」是同一件事——
		// 配两条长得一样、效果也一样的规则只会让下次命中哪条取决于查询顺序。要表达「全归平台」
		// 就明确地放一条 remainder 的平台项。
		return write, ErrSettlementRuleItemsRequired
	case len(in.Items) > MaxSettlementRuleItems:
		return write, ErrSettlementRuleItemsTooMany
	}

	write.Items = make([]repository.SettlementRuleItemWrite, 0, len(in.Items))
	accountIDs := make([]string, 0, len(in.Items))
	seenAccounts := make(map[string]bool, len(in.Items))
	// percentRatioHundredths 是**求和用**的整数：百分点 × 100。用 float 求和会让
	// 33.33 + 33.33 + 33.34 变成一个比 100 略大或略小的数，而这条校验只在「刚好超了」
	// 那一刻有意义。
	var percentRatioHundredths int64
	platformSeen := false

	for index, item := range in.Items {
		converted, accountID, err := s.buildRuleItem(item)
		if err != nil {
			return write, err
		}
		switch converted.CalcType {
		case model.SettlementCalcPercent:
			percentRatioHundredths += converted.RatioHundredths
		case model.SettlementCalcRemainder:
			if platformSeen {
				return write, ErrSettlementRuleItemPlatformTwice
			}
			platformSeen = true
		}
		if accountID != "" {
			// 同一个账户在同一条规则里出现两次：008 有一条部分唯一索引钉它，但撞索引会回一句
			// 「这一行和已有的撞了」，而用户要的是「第 3 项和第 5 项是同一个账户」。这件事只由
			// 请求体决定，所以在进库之前就按 400 拒掉。
			if seenAccounts[accountID] {
				return write, ErrSettlementRuleItemAccountTwice
			}
			seenAccounts[accountID] = true
			accountIDs = append(accountIDs, accountID)
		}
		write.Items = append(write.Items, converted)
		// sort_order 不取用户传的值，按提交顺序重排：页面上的顺序就是 ProFormList 里的顺序，
		// 而「顺序」这一栏今天没有任何语义（命中与计算都与它无关，它只影响显示），让调用方填
		// 一个不影响任何事的数字只会多一种「两项都是 0，谁在前」的不确定。
		write.Items[len(write.Items)-1].SortOrder = index
	}

	// 合计比例超过 100%：computeSettlement 遇到它会**静默整单归平台**（那条「分出去的钱比
	// 收进来的多」的兜底）。配错的人看不到报错，收到钱的人只是没收到——这是后台写入口最该
	// 拦下的一条。
	if percentRatioHundredths > 10000 {
		return write, ErrSettlementRuleItemRatioOverflow
	}

	if err := s.checkRuleAccounts(ctx, accountIDs); err != nil {
		return write, err
	}
	return write, nil
}

// buildRuleItem 校验一项，并把它归一化成仓储要的形状（返回第二值是账户 id，平台项为空）。
//
// 逐项要查四件事，两两对应 008 那条三选一的 CHECK：算法与主体绑死（平台项不可能是比例项）、
// 算法与金额绑死（percent 只看 ratio、fixed 只看 fixed_amount）、主体与账户绑死（平台项没有
// 账户）。表上的 CHECK 会在写库那一刻拒掉不合的那些，但那时错误已经没法指出是第几项了。
func (s *AdminSettlementService) buildRuleItem(item dto.SettlementRuleItemInput) (repository.SettlementRuleItemWrite, string, error) {
	write := repository.SettlementRuleItemWrite{}

	write.PartyType = strings.TrimSpace(item.PartyType)
	if !model.IsSettlementPartyType(write.PartyType) {
		return write, "", ErrSettlementRuleItemPartyInvalid
	}
	write.CalcType = strings.TrimSpace(item.CalcType)
	if !model.IsSettlementCalcType(write.CalcType) {
		return write, "", ErrSettlementRuleItemCalcInvalid
	}
	isPlatform := write.PartyType == model.SettlementPartyPlatform

	switch write.CalcType {
	case model.SettlementCalcRemainder:
		if !isPlatform {
			return write, "", ErrSettlementRuleItemCalcInvalid
		}
		if item.RatioPercent != 0 || item.FixedAmount != 0 {
			return write, "", ErrSettlementRuleItemRemainderGiven
		}

	case model.SettlementCalcPercent:
		if isPlatform {
			return write, "", ErrSettlementRuleItemCalcInvalid
		}
		hundredths, ok := ratioHundredths(item.RatioPercent)
		// (0, 100]：0 的比例项等于什么也不分，留着它只会让「这条规则总共分了 45%」这句话
		// 在页面上对不上（有一个 0% 的项却没有那笔钱）。008 的 CHECK 只管 ratio >= 0。
		if !ok || hundredths <= 0 || hundredths > 10000 {
			return write, "", ErrSettlementRuleItemRatioInvalid
		}
		if item.FixedAmount != 0 {
			return write, "", ErrSettlementRuleItemRatioInvalid
		}
		write.RatioHundredths = hundredths

	case model.SettlementCalcFixed:
		if isPlatform {
			return write, "", ErrSettlementRuleItemCalcInvalid
		}
		if item.FixedAmount <= 0 {
			return write, "", ErrSettlementRuleItemFixedInvalid
		}
		if item.RatioPercent != 0 {
			return write, "", ErrSettlementRuleItemFixedInvalid
		}
		write.FixedAmount = item.FixedAmount
	}

	// 备注先落下来，**必须排在平台项那一支之前**：那一支是个提前 return，写在它后面的话
	// 平台项的备注会被丢掉——用户填了、保存成功、再打开没了，而库里那一列是有默认值的，
	// 从数据上完全看不出「这里本来有个备注」。
	write.Remark = strings.TrimSpace(item.Remark)
	if len([]rune(write.Remark)) > MaxSettlementRemarkLength {
		return write, "", ErrSettlementRuleRemarkTooLong
	}

	// 平台项没有账户，其余项必须有。008 的 CHECK 是个等价式，两个方向都要管——写成
	// 「platform 时清空 accountId」会让一个误填的账户悄无声息地消失，而那个人下次打开这条
	// 规则时会疑惑自己填的账户去哪了。
	accountID := strings.TrimSpace(item.AccountID)
	if isPlatform {
		if accountID != "" {
			return write, "", ErrSettlementRuleItemPlatformAccount
		}
		write.AccountID = ""
		return write, "", nil
	}
	if accountID == "" {
		return write, "", ErrSettlementRuleItemAccountRequired
	}
	parsed, err := uuid.Parse(accountID)
	if err != nil {
		return write, "", ErrSettlementRuleItemAccountInvalid
	}
	write.AccountID = parsed.String()
	return write, write.AccountID, nil
}

// checkRuleAccounts 查这条规则挂的账户现在能不能用。
//
// 三件事，缺一件都会让这一项在**执行期被静默滤掉**（repository.settlementRuleItems 的 LEFT
// JOIN 带 a.status='enabled' AND a.provider=$渠道）：账户不存在、账户停用了、账户挂的不是同一
// 条渠道。第三种是这里唯一能拦下的地方——执行期那个 JOIN 是按「这笔支付走哪条渠道」去比的，
// 一条规则里混了两条渠道的账户时，其中一半在任何一笔支付上都会被过滤掉，而数据上看不出
// 任何异常。
//
// 账户不存在与停用分开报：前者是「你选的那个账户没了」（多半另开了一页删掉了），后者是
// 「它被停用了，换成别的或者先把它启用」。两句话让用户做的事不一样。
//
// 两个都**不往上报 404**：这条路由是 PUT/POST 一条规则，用户要的不是「去找那个账户」而是
// 「把这一项改对」。仓储那个 ErrSettlementAccountNotFound 属于 GET /accounts/{id}。
func (s *AdminSettlementService) checkRuleAccounts(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.repository.GetSettlementAccounts(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[string]*repository.SettlementAccountRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}

	provider := ""
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			return ErrSettlementRuleItemAccountMissing
		}
		if row.Status != model.SettlementRecordEnabled {
			return ErrSettlementRuleItemAccountDisabled
		}
		if provider == "" {
			provider = row.Provider
			continue
		}
		if row.Provider != provider {
			return ErrSettlementRuleItemAccountChannel
		}
	}
	return nil
}

// ratioHundredths 把百分数换成「百分点的百分之一」这个整数（45 → 4500）。
//
// 落库那一侧是 `$n::numeric / 10000`（见 repository.insertSettlementRuleItems），两边一乘
// 一除，中间**没有任何一处过浮点**。第二返回值是「这个值能不能精确表示成两位小数」：1.005
// 这类三位小数的值不做四舍五入，而是拒掉——悄悄改成 1.01 之后，页面上显示的数字与运营填的
// 对不上，而分账金额是照那个数字算的。
func ratioHundredths(percent float64) (int64, bool) {
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, false
	}
	scaled := percent * 100
	rounded := math.Round(scaled)
	// 容差取 1e-6：percent 是从 JSON 里解出来的 float64，像 33.33 这样的十进制小数本来就
	// 存不下精确值，乘 100 之后会落在 3332.9999999999995 这种地方。差得比这更大的，是真有
	// 第三位小数。
	if math.Abs(scaled-rounded) > 1e-6 {
		return 0, false
	}
	return int64(rounded), true
}

// ============================================================
// 任务（只读）
// ============================================================

func (s *AdminSettlementService) ListTasks(ctx context.Context, q dto.SettlementTaskQuery) ([]dto.SettlementTask, int, error) {
	rows, total, err := s.repository.ListSettlementTasks(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	items := make([]dto.SettlementTask, 0, len(rows))
	for _, row := range rows {
		items = append(items, settlementTaskDTO(row))
	}
	return items, total, nil
}

// GetTask 读一条任务连同它的接收方明细。
//
// 明细**原样透传那几列快照**（比例、金额、主体名、子商户号），不回头 JOIN 账户补当前值：一条
// 历史明细的意义是「当时分给了谁」，而账户改名或停用之后它必须还是当初那个样子。
func (s *AdminSettlementService) GetTask(ctx context.Context, id string) (*dto.SettlementTaskDetail, error) {
	detail, err := s.repository.GetSettlementTask(ctx, id)
	if err != nil {
		return nil, err
	}
	receivers := make([]dto.SettlementReceiver, 0, len(detail.Receivers))
	for _, row := range detail.Receivers {
		receivers = append(receivers, settlementReceiverDTO(row))
	}
	return &dto.SettlementTaskDetail{
		Task:      settlementTaskDTO(detail.Task),
		Receivers: receivers,
	}, nil
}

// ============================================================
// 行 → DTO
// ============================================================
//
// 这一层只做搬运：字段一对一，不做判断、不改单位以外的任何东西。**不在这里把空指针换成 0**：
// finishedAt 的 null 与「发生在零时刻」在前端是两个显示（后者根本不该存在），把它压成 0 会让
// 那个 `—` 再也回不来。

func settlementAccountDTO(row *repository.SettlementAccountRow) dto.SettlementAccount {
	if row == nil {
		return dto.SettlementAccount{}
	}
	return dto.SettlementAccount{
		ID: row.ID, PartyName: row.PartyName, PartyType: row.PartyType,
		Provider: row.Provider, ReceiverType: row.ReceiverType, ReceiverID: row.ReceiverID,
		Status: row.Status, Remark: row.Remark,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func settlementRuleDTO(row *repository.SettlementRuleRow) dto.SettlementRule {
	if row == nil {
		return dto.SettlementRule{}
	}
	items := make([]dto.SettlementRuleItem, 0, len(row.Items))
	for _, item := range row.Items {
		items = append(items, dto.SettlementRuleItem{
			ID: item.ID, PartyType: item.PartyType, CalcType: item.CalcType,
			RatioPercent: ratioPercent(item.RatioHundredths), FixedAmount: item.FixedAmount,
			AccountID: item.AccountID, AccountName: item.AccountName, ReceiverID: item.AccountReceiver,
			SortOrder: item.SortOrder, Remark: item.Remark,
		})
	}
	return dto.SettlementRule{
		ID: row.ID, Name: row.Name, BizType: row.BizType, ScopeType: row.ScopeType,
		ScopeRef: row.ScopeRef, AllocationMode: row.AllocationMode, Status: row.Status,
		Remark: row.Remark, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		Items: items,
	}
}

func settlementTaskDTO(row *repository.SettlementTaskRow) dto.SettlementTask {
	if row == nil {
		return dto.SettlementTask{}
	}
	return dto.SettlementTask{
		ID: row.ID, TaskNo: row.TaskNo, PaymentNo: row.PaymentNo, OrderNo: row.OrderNo,
		BaseAmount: row.BaseAmount, PlatformAmount: row.PlatformAmount, Status: row.Status,
		ScopeType: row.ScopeType, ScopeRef: row.ScopeRef, StoreRef: row.StoreRef,
		BrandRef: row.BrandRef, MerchantRef: row.MerchantRef,
		RuleID: row.RuleID, RuleName: row.RuleName,
		Provider: row.Provider, Method: row.Method,
		ProviderTaskNo: row.ProviderTaskNo, ProviderTransactionID: row.ProviderTransactionID,
		Attempts: row.Attempts, LastError: row.LastError,
		CreatedAt: row.CreatedAt, FinishedAt: row.FinishedAt, UpdatedAt: row.UpdatedAt,
	}
}

func settlementReceiverDTO(row *repository.SettlementReceiverRow) dto.SettlementReceiver {
	if row == nil {
		return dto.SettlementReceiver{}
	}
	return dto.SettlementReceiver{
		ID: row.ID, AccountID: row.AccountID, PartyType: row.PartyType, PartyName: row.PartyName,
		MerchantRef: row.MerchantRef, BrandRef: row.BrandRef, StoreRef: row.StoreRef,
		ReceiverType: row.ReceiverType, ReceiverID: row.ReceiverID,
		RatioPercent: ratioPercent(row.RatioHundredths), Amount: row.Amount,
		ReversedAmount: row.ReversedAmount, ProviderDetailNo: row.ProviderDetailNo,
		Status: row.Status, LastError: row.LastError, CreatedAt: row.CreatedAt,
	}
}

// ratioPercent 把「百分点的百分之一」换回百分数（4500 → 45）。与 ratioHundredths 互为逆运算，
// 两个都在这一层里成对出现，好对照。
func ratioPercent(hundredths int64) float64 {
	return float64(hundredths) / 100
}
