package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 分账后台写入口的校验用例。
//
// # 它们守的是什么
//
// 008 把能钉在表上的约束都钉住了，这一批用例守的是**钉不住的那些**——也就是配错了不报错、
// 只是钱分错地方的那些：
//
//   - 比例合计超过 100%：computeSettlement 遇到它会静默整单归平台（那条「分出去的钱比收进来
//     的多」的兜底）。**这一条是本文件存在的首要理由**：它是唯一一条配错之后页面上一切正常、
//     只有钱少了的错。
//   - 账户停用 / 账户挂错渠道：执行期那个 LEFT JOIN 会把这一项滤掉，同样静默。
//   - scope_ref 与档位不匹配：漏到 SQL 上是一条 CHECK 违规，用户拿到的是一句没有线索的兜底。
//
// 每一条都用 errors.Is 断言**具体是哪一个错误值**，不断言「返回了错误」：那些错误的区分正是
// controller 那张人话表要用的东西，一句笼统的「参数不合法」在页面上等于没说。

// fakeSettlementRepository 只实现这一组用例走得到的方法。
//
// 其余的实现一律 panic——**故意的**：它们在这批用例里不该被调用，而一个返回零值的桩会让
// 「校验漏了一处、把一个空值写进了库」看起来像通过。panic 会在第一次跑到时就把这件事喊出来。
//
// 两个 Create 把写入**原样回读成行**，照仓储的真做法（写完之后读回来交给 DTO 映射）。少了
// 这一步，回给调用方的是一个空壳，而「页面上填的 45.5% 原样存下去又原样读回来」就没人验了。
type fakeSettlementRepository struct {
	accounts []*repository.SettlementAccountRow
	// ruleWrites 记下每一次真正写下去的规则（校验通过的那些），供「换算对不对」这一类断言用。
	ruleWrites []repository.SettlementRuleWrite
}

func (f *fakeSettlementRepository) GetSettlementAccounts(_ context.Context, ids []string) ([]*repository.SettlementAccountRow, error) {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	found := make([]*repository.SettlementAccountRow, 0, len(ids))
	for _, account := range f.accounts {
		if wanted[account.ID] {
			found = append(found, account)
		}
	}
	return found, nil
}

func (f *fakeSettlementRepository) CreateSettlementRule(_ context.Context, in repository.SettlementRuleWrite) (*repository.SettlementRuleRow, error) {
	f.ruleWrites = append(f.ruleWrites, in)
	row := &repository.SettlementRuleRow{
		ID: uuid.NewString(), Name: in.Name, BizType: in.BizType, ScopeType: in.ScopeType,
		ScopeRef: in.ScopeRef, AllocationMode: in.AllocationMode, Status: in.Status,
		Remark: in.Remark,
	}
	for _, item := range in.Items {
		row.Items = append(row.Items, repository.SettlementRuleItemRow{
			ID: uuid.NewString(), PartyType: item.PartyType, CalcType: item.CalcType,
			RatioHundredths: item.RatioHundredths, FixedAmount: item.FixedAmount,
			SortOrder: item.SortOrder, Remark: item.Remark, AccountID: item.AccountID,
		})
	}
	return row, nil
}

func (f *fakeSettlementRepository) CreateSettlementAccount(_ context.Context, in repository.SettlementAccountWrite) (*repository.SettlementAccountRow, error) {
	return &repository.SettlementAccountRow{
		ID: uuid.NewString(), PartyName: in.PartyName,
		PartyType: in.PartyType, Provider: in.Provider, ReceiverType: in.ReceiverType,
		ReceiverID: in.ReceiverID, Status: in.Status,
		Remark: in.Remark,
	}, nil
}

// 这一批用例走不到的那些。
func (f *fakeSettlementRepository) ListSettlementAccounts(context.Context, dto.SettlementAccountQuery) ([]*repository.SettlementAccountRow, int, error) {
	panic("ListSettlementAccounts 不该被这条用例调用")
}

func (f *fakeSettlementRepository) GetSettlementAccount(context.Context, string) (*repository.SettlementAccountRow, error) {
	panic("GetSettlementAccount 不该被这条用例调用")
}

func (f *fakeSettlementRepository) UpdateSettlementAccount(context.Context, string, repository.SettlementAccountWrite) (*repository.SettlementAccountRow, error) {
	panic("UpdateSettlementAccount 不该被这条用例调用")
}

func (f *fakeSettlementRepository) DeleteSettlementAccount(context.Context, string) error {
	panic("DeleteSettlementAccount 不该被这条用例调用")
}

func (f *fakeSettlementRepository) ListSettlementRules(context.Context, dto.SettlementRuleQuery) ([]*repository.SettlementRuleRow, int, error) {
	panic("ListSettlementRules 不该被这条用例调用")
}

func (f *fakeSettlementRepository) GetSettlementRule(context.Context, string) (*repository.SettlementRuleRow, error) {
	panic("GetSettlementRule 不该被这条用例调用")
}

func (f *fakeSettlementRepository) UpdateSettlementRule(context.Context, string, repository.SettlementRuleWrite) (*repository.SettlementRuleRow, error) {
	panic("UpdateSettlementRule 不该被这条用例调用")
}

func (f *fakeSettlementRepository) DeleteSettlementRule(context.Context, string) error {
	panic("DeleteSettlementRule 不该被这条用例调用")
}

func (f *fakeSettlementRepository) ListSettlementTasks(context.Context, dto.SettlementTaskQuery) ([]*repository.SettlementTaskRow, int, error) {
	panic("ListSettlementTasks 不该被这条用例调用")
}

func (f *fakeSettlementRepository) GetSettlementTask(context.Context, string) (*repository.SettlementTaskDetail, error) {
	panic("GetSettlementTask 不该被这条用例调用")
}

// newSettlementService 组装一个分账后台的服务层：目录是真的（用例要验渠道白名单），仓储是假的。
func newSettlementService(accounts ...*repository.SettlementAccountRow) (*AdminSettlementService, *fakeSettlementRepository) {
	repo := &fakeSettlementRepository{accounts: accounts}
	return NewAdminSettlementService(repo, testCatalog()), repo
}

// enabledAccount 造一个启用中的账户：渠道 ums（testCatalog 里那条可分账的渠道），主体是一家门店。
func enabledAccount(provider string) *repository.SettlementAccountRow {
	return &repository.SettlementAccountRow{
		ID: uuid.NewString(), PartyName: "测试门店",
		PartyType: model.SettlementPartyMemberStore, Provider: provider,
		ReceiverType: model.SettlementReceiverMerchantID, ReceiverID: "MID-" + uuid.NewString(),
		Status: model.SettlementRecordEnabled,
	}
}

// validRuleInput 是一份**能过**的规则：门店档、一家门店拿 45%、平台拿差额。
//
// 每一条用例都从它出发只改一处，这样「这条用例验的是哪个字段」不需要读第二遍——而一个从零
// 拼出来的输入会让某条用例其实是被另一个字段的错误拦下的，那种红比绿还糟。
func validRuleInput(accountID string) dto.SettlementRuleInput {
	return dto.SettlementRuleInput{
		Name: "门店分账", BizType: model.SettlementBizCoffee,
		ScopeType: model.SettlementScopeStore, ScopeRef: uuid.NewString(),
		AllocationMode: model.SettlementAllocationNormal,
		Items: []dto.SettlementRuleItemInput{
			{PartyType: model.SettlementPartyMemberStore, CalcType: model.SettlementCalcPercent,
				RatioPercent: 45, AccountID: accountID},
			{PartyType: model.SettlementPartyPlatform, CalcType: model.SettlementCalcRemainder},
		},
	}
}

// TestSettlementRuleValidation 逐条走一遍写入口的校验，每条只改 validRuleInput 的一处。
func TestSettlementRuleValidation(t *testing.T) {
	account := enabledAccount("ums")

	cases := []struct {
		name   string
		mutate func(*dto.SettlementRuleInput)
		want   error
	}{
		{
			name:   "名称空",
			mutate: func(in *dto.SettlementRuleInput) { in.Name = "  " },
			want:   ErrSettlementRuleNameRequired,
		},
		{
			name:   "业务分类不在词表",
			mutate: func(in *dto.SettlementRuleInput) { in.BizType = "coffe" },
			want:   ErrSettlementRuleBizTypeInvalid,
		},
		{
			name:   "档位不在词表",
			mutate: func(in *dto.SettlementRuleInput) { in.ScopeType = "shop" },
			want:   ErrSettlementRuleScopeTypeInvalid,
		},
		{
			// global ⇔ scope_ref='' 的其中一个方向：008 用 CHECK 钉住，漏到 SQL 上是一条
			// 没有线索的 400 兜底。
			name:   "全局档位却带了范围",
			mutate: func(in *dto.SettlementRuleInput) { in.ScopeType = model.SettlementScopeGlobal },
			want:   ErrSettlementScopeRefNotAllowed,
		},
		{
			name:   "非全局档位没给范围",
			mutate: func(in *dto.SettlementRuleInput) { in.ScopeRef = "" },
			want:   ErrSettlementScopeRefRequired,
		},
		{
			// 不是 uuid 的值原样进 SQL 会让 PostgreSQL 在解析参数时报错，然后以 500 的
			// 面目弹在一个只是填错了的输入框上。
			name:   "范围不是合法 uuid",
			mutate: func(in *dto.SettlementRuleInput) { in.ScopeRef = "store-1" },
			want:   ErrSettlementScopeRefInvalid,
		},
		{
			name:   "分配模式不在词表",
			mutate: func(in *dto.SettlementRuleInput) { in.AllocationMode = "normal_first" },
			want:   ErrSettlementRuleModeInvalid,
		},
		{
			name:   "状态不在词表",
			mutate: func(in *dto.SettlementRuleInput) { in.Status = "paused" },
			want:   ErrSettlementRuleStatusInvalid,
		},
		{
			name:   "一项都没有",
			mutate: func(in *dto.SettlementRuleInput) { in.Items = nil },
			want:   ErrSettlementRuleItemsRequired,
		},
		{
			name: "项太多",
			mutate: func(in *dto.SettlementRuleInput) {
				items := make([]dto.SettlementRuleItemInput, 0, MaxSettlementRuleItems+1)
				for range MaxSettlementRuleItems + 1 {
					items = append(items, dto.SettlementRuleItemInput{
						PartyType: model.SettlementPartyPlatform,
						CalcType:  model.SettlementCalcRemainder,
					})
				}
				in.Items = items
			},
			want: ErrSettlementRuleItemsTooMany,
		},
		{
			name:   "主体类型不在词表",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[0].PartyType = "shop" },
			want:   ErrSettlementRuleItemPartyInvalid,
		},
		{
			// 平台项不能是比例项：008 的 CHECK 把「算法与主体」绑死了，漏到 SQL 上是一条
			// 按约束名分不出来的 CHECK 违规。
			name: "平台项配成比例",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items[1].CalcType = model.SettlementCalcPercent
				in.Items[1].RatioPercent = 10
			},
			want: ErrSettlementRuleItemCalcInvalid,
		},
		{
			name: "非平台项配成差额自留",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items[0].CalcType = model.SettlementCalcRemainder
				in.Items[0].RatioPercent = 0
			},
			want: ErrSettlementRuleItemCalcInvalid,
		},
		{
			name:   "比例项的比例是 0",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[0].RatioPercent = 0 },
			want:   ErrSettlementRuleItemRatioInvalid,
		},
		{
			name:   "比例项的比例超过 100",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[0].RatioPercent = 100.01 },
			want:   ErrSettlementRuleItemRatioInvalid,
		},
		{
			// 三位小数的值不做四舍五入：悄悄改成 45.01 之后，页面上显示的数字与运营填的
			// 对不上，而分账金额是照那个数字算的。
			name:   "比例有三位小数",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[0].RatioPercent = 45.005 },
			want:   ErrSettlementRuleItemRatioInvalid,
		},
		{
			name:   "比例项同时填了固定额",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[0].FixedAmount = 100 },
			want:   ErrSettlementRuleItemRatioInvalid,
		},
		{
			name: "固定额项金额是 0",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items[0].CalcType = model.SettlementCalcFixed
				in.Items[0].RatioPercent = 0
				in.Items[0].FixedAmount = 0
			},
			want: ErrSettlementRuleItemFixedInvalid,
		},
		{
			name: "固定额项同时填了比例",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items[0].CalcType = model.SettlementCalcFixed
				in.Items[0].FixedAmount = 100
			},
			want: ErrSettlementRuleItemFixedInvalid,
		},
		{
			name:   "平台自留项填了比例",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[1].RatioPercent = 5 },
			want:   ErrSettlementRuleItemRemainderGiven,
		},
		{
			name:   "平台自留项指定了账户",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[1].AccountID = account.ID },
			want:   ErrSettlementRuleItemPlatformAccount,
		},
		{
			name:   "非平台项没指定账户",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[0].AccountID = "" },
			want:   ErrSettlementRuleItemAccountRequired,
		},
		{
			name:   "账户 id 不是合法 uuid",
			mutate: func(in *dto.SettlementRuleInput) { in.Items[0].AccountID = "acc-1" },
			want:   ErrSettlementRuleItemAccountInvalid,
		},
		{
			name: "两个平台自留项",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items = append(in.Items, dto.SettlementRuleItemInput{
					PartyType: model.SettlementPartyPlatform, CalcType: model.SettlementCalcRemainder,
				})
			},
			want: ErrSettlementRuleItemPlatformTwice,
		},
		{
			// 平台自留项的备注走的是**另一条支路**（那一支在读完账户之前就 return 了），
			// 所以长度校验要有一条专门打它的用例：只测比例项那条的话，平台项超长会静默写进库。
			name: "平台自留项的备注太长",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items[1].Remark = strings.Repeat("备", MaxSettlementRemarkLength+1)
			},
			want: ErrSettlementRuleRemarkTooLong,
		},
		{
			name: "比例项的备注太长",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items[0].Remark = strings.Repeat("备", MaxSettlementRemarkLength+1)
			},
			want: ErrSettlementRuleRemarkTooLong,
		},
		{
			// 008 有一条部分唯一索引钉它，但撞索引会回一句「这一行和已有的撞了」——用户要
			// 的是「第 1 项和第 3 项是同一个账户」，所以要在进库之前拦。
			name: "同一个账户出现两次",
			mutate: func(in *dto.SettlementRuleInput) {
				in.Items = append(in.Items, dto.SettlementRuleItemInput{
					PartyType: model.SettlementPartyMemberStore, CalcType: model.SettlementCalcPercent,
					RatioPercent: 10, AccountID: account.ID,
				})
			},
			want: ErrSettlementRuleItemAccountTwice,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSettlementService(account)
			in := validRuleInput(account.ID)
			tc.mutate(&in)

			_, err := svc.CreateRule(context.Background(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateRule 的错误 = %v，期望 %v", err, tc.want)
			}
			// 每一条都得是「请求不合法」：controller 按这个判定回 400，而漏标一条的后果是
			// 一次填错变成 500（用户看到「服务暂时不可用」，日志里也没有他能改的东西）。
			if !IsValidationError(err) {
				t.Fatalf("%v 没有被算成「请求不合法」", err)
			}
		})
	}
}

// TestSettlementRulePercentOverflow 单独测那一条最要紧的校验。
//
// 它值得从上面那张表里单拎出来：**它是唯一一条配错之后页面上一切正常的错**。
// computeSettlement 遇到「分出去的钱比收进来的多」会整单归平台（那条兜底），所以比例合计
// 超过 100% 的规则不会报错、不会异常，只是门店一分钱都收不到。上面那张表里的用例改的是 45% 那
// 一项自己，测不到**多项相加**这件事。
func TestSettlementRulePercentOverflow(t *testing.T) {
	cases := []struct {
		name    string
		ratios  []float64
		wantErr error
	}{
		// 95.29 + 2.93 + 1.78 在十进制上正好 100，整数百分点求和也是 10000；但 float64
		// 加起来是 100.00000000000002，乘 100 之后比 10000 大出 2e-12——一条**完全合法**的
		// 规则会被一条浮点判据误拒。这条用例守的就是那个换算（见 service.ratioHundredths）。
		{name: "三项凑满 100（浮点会多出一点）", ratios: []float64{95.29, 2.93, 1.78}},
		{name: "两项凑满 100", ratios: []float64{60, 40}},
		{name: "最小的一项", ratios: []float64{0.01}},
		{name: "超出一分", ratios: []float64{60, 40.01}, wantErr: ErrSettlementRuleItemRatioOverflow},
		{name: "远超", ratios: []float64{80, 80}, wantErr: ErrSettlementRuleItemRatioOverflow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每一项各挂一个账户：同一账户出现两次会先撞上另一条校验，那样这条用例就不再
			// 是在验合计比例了。
			accounts := make([]*repository.SettlementAccountRow, 0, len(tc.ratios))
			for range tc.ratios {
				accounts = append(accounts, enabledAccount("ums"))
			}
			svc, _ := newSettlementService(accounts...)

			in := validRuleInput(accounts[0].ID)
			in.Items = []dto.SettlementRuleItemInput{
				{PartyType: model.SettlementPartyPlatform, CalcType: model.SettlementCalcRemainder},
			}
			for i, ratio := range tc.ratios {
				in.Items = append(in.Items, dto.SettlementRuleItemInput{
					PartyType: model.SettlementPartyPartner, CalcType: model.SettlementCalcPercent,
					RatioPercent: ratio, AccountID: accounts[i].ID,
				})
			}

			_, err := svc.CreateRule(context.Background(), in)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("CreateRule 回了 %v，期望通过", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateRule 的错误 = %v，期望 %v", err, tc.wantErr)
			}
		})
	}
}

// TestSettlementRuleAccountState 守的是「账户现在能不能用」这三件事。
//
// 三件都会让这一项在**执行期被静默滤掉**（repository.settlementRuleItems 那个 LEFT JOIN 带
// a.status='enabled' AND a.provider=$渠道），表现是这家门店的钱一直分不到，而页面上一切正常。
func TestSettlementRuleAccountState(t *testing.T) {
	enabled := enabledAccount("ums")
	disabled := enabledAccount("ums")
	disabled.Status = model.SettlementRecordDisabled
	// 目录里今天只有 ums 一条可分账的渠道，所以「另一条渠道的账户」只能用一条不存在的渠道名
	// 造出来——这正是执行期那个 JOIN 看不见的那一半：账户在库里好好的，只是永远配不上。
	otherChannel := enabledAccount("wechat_pay")

	cases := []struct {
		name     string
		accounts []*repository.SettlementAccountRow
		useID    string
		want     error
	}{
		{name: "启用中的账户", accounts: []*repository.SettlementAccountRow{enabled}, useID: enabled.ID},
		{
			name: "账户已停用", accounts: []*repository.SettlementAccountRow{disabled},
			useID: disabled.ID, want: ErrSettlementRuleItemAccountDisabled,
		},
		{
			// 挂了一个库里没有的账户是**这份请求不成立**（400），不是「查无此账户」（404）：
			// 这条路由是 PUT/POST 一条规则，用户要改的是这一项，报 404 会把他引到账户列表去。
			name: "账户不存在", accounts: []*repository.SettlementAccountRow{enabled},
			useID: uuid.NewString(), want: ErrSettlementRuleItemAccountMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSettlementService(tc.accounts...)
			_, err := svc.CreateRule(context.Background(), validRuleInput(tc.useID))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CreateRule 回了 %v，期望通过", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateRule 的错误 = %v，期望 %v", err, tc.want)
			}
		})
	}

	t.Run("同一条规则里混了两条渠道的账户", func(t *testing.T) {
		svc, _ := newSettlementService(enabled, otherChannel)
		in := validRuleInput(enabled.ID)
		in.Items = append(in.Items, dto.SettlementRuleItemInput{
			PartyType: model.SettlementPartyAgent, CalcType: model.SettlementCalcPercent,
			RatioPercent: 10, AccountID: otherChannel.ID,
		})
		_, err := svc.CreateRule(context.Background(), in)
		if !errors.Is(err, ErrSettlementRuleItemAccountChannel) {
			t.Fatalf("CreateRule 的错误 = %v，期望 %v", err, ErrSettlementRuleItemAccountChannel)
		}
	})
}

// TestSettlementRuleKeepsEveryItemRemark 守的是「每一项的备注都真的写下去了」。
//
// 单拎出来是因为**平台项那一支是个提前 return**（它不需要账户，读完主体与算法就走了），备注
// 一旦写在它后面就会被丢掉——用户填了、保存成功、再打开没了。这一列的默认值是空串，所以从
// 库里的数据完全看不出「这里本来有个备注」，而审计快照里也一样是空的：丢得**无声无息**。
//
// 三项各带备注，是为了让「只处理了其中一支」这种写法必红。
func TestSettlementRuleKeepsEveryItemRemark(t *testing.T) {
	account := enabledAccount("ums")
	svc, repo := newSettlementService(account)

	in := validRuleInput(account.ID)
	in.Items[0].Remark = "门店那一份"
	in.Items[1].Remark = "平台自留"

	if _, err := svc.CreateRule(context.Background(), in); err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if len(repo.ruleWrites) != 1 {
		t.Fatalf("写下去的次数 = %d，期望 1", len(repo.ruleWrites))
	}
	stored := repo.ruleWrites[0].Items
	if len(stored) != 2 {
		t.Fatalf("写下去的项 = %d，期望 2", len(stored))
	}
	if stored[0].Remark != "门店那一份" {
		t.Errorf("比例项的备注 = %q，期望 %q", stored[0].Remark, "门店那一份")
	}
	// 这一条就是那个提前 return 的回归。
	if stored[1].Remark != "平台自留" {
		t.Errorf("平台自留项的备注 = %q，期望 %q（它在 buildRuleItem 里走的是另一条 return 支路）",
			stored[1].Remark, "平台自留")
	}
}

// TestSettlementRuleRatioLandsExact 验的是「页面上填的百分数原样落到库里、再原样读回来」。
//
// 45.5% 必须落成 0.455 而不是 0.45499999999999996：列是 NUMERIC(20,6)，而中间只要过了一次
// float 就会带上尾数——将来拿这个比例去算金额时，那点尾数会一路走到分账明细上。换算成整数的
// 百分点一百倍（4550）再由 SQL 除以 10000，是这条路上唯一不碰浮点的走法。
func TestSettlementRuleRatioLandsExact(t *testing.T) {
	account := enabledAccount("ums")
	svc, repo := newSettlementService(account)

	in := validRuleInput(account.ID)
	in.Items[0].RatioPercent = 45.5
	in.Items[0].Remark = "门店那一份"

	rule, err := svc.CreateRule(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if len(repo.ruleWrites) != 1 {
		t.Fatalf("写下去的规则有 %d 条，期望 1 条", len(repo.ruleWrites))
	}
	write := repo.ruleWrites[0]

	// 45.5% → 4550（百分点的百分之一）。仓储那一侧写的是 `$n::numeric / 10000`。
	if write.Items[0].RatioHundredths != 4550 {
		t.Fatalf("比例换算 = %d，期望 4550", write.Items[0].RatioHundredths)
	}
	// sort_order 按提交顺序重排，不取调用方传的值：顺序今天只影响显示，让调用方填一个不影响
	// 任何事的数字只会多一种「两项都是 0，谁在前」的不确定。
	if write.Items[0].SortOrder != 0 || write.Items[1].SortOrder != 1 {
		t.Fatalf("sort_order = %d,%d，期望 0,1", write.Items[0].SortOrder, write.Items[1].SortOrder)
	}
	if write.Items[1].RatioHundredths != 0 || write.Items[1].FixedAmount != 0 {
		t.Fatal("平台自留项不该带比例或固定额")
	}
	if write.Items[0].Remark != "门店那一份" {
		t.Fatalf("项的备注 = %q，期望原样带下去", write.Items[0].Remark)
	}
	if write.ScopeRef != in.ScopeRef {
		t.Fatalf("scope_ref = %q，期望 %q", write.ScopeRef, in.ScopeRef)
	}
	// 回来的 DTO 要把整数换算回百分数：前端那个百分比输入框显示的就是它。
	if len(rule.Items) != 2 {
		t.Fatalf("回读的项有 %d 条，期望 2 条", len(rule.Items))
	}
	if rule.Items[0].RatioPercent != 45.5 {
		t.Fatalf("回读的比例 = %v，期望 45.5", rule.Items[0].RatioPercent)
	}
}

// TestSettlementRuleDefaults 验两个「空值 = 默认」的归一化。
//
// 它们不是「无所谓」的默认：状态空默认 enabled 意味着**新建的规则立刻参与命中**（这是绝大
// 多数情况），把一条正在生效的规则关掉是一个明确动作，不该是「忘了传」的结果；分配模式空默认
// normal 与 008 的列默认值一致。
func TestSettlementRuleDefaults(t *testing.T) {
	account := enabledAccount("ums")
	svc, repo := newSettlementService(account)

	in := validRuleInput(account.ID)
	in.Status = ""
	in.AllocationMode = ""
	if _, err := svc.CreateRule(context.Background(), in); err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	write := repo.ruleWrites[0]
	if write.Status != model.SettlementRecordEnabled {
		t.Fatalf("状态 = %q，期望 %q", write.Status, model.SettlementRecordEnabled)
	}
	if write.AllocationMode != model.SettlementAllocationNormal {
		t.Fatalf("分配模式 = %q，期望 %q", write.AllocationMode, model.SettlementAllocationNormal)
	}
}

// TestSettlementAccountValidation 走一遍账户的写入口。
//
// 这里面最要紧的是**渠道白名单**：挂错渠道的账户配出来是会一直分不到钱的，而页面上一切正常
// （执行期那个 JOIN 按渠道过滤，过滤在数据上看不见）。它是这里唯一一条能救回真金白银的校验。
func TestSettlementAccountValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*dto.SettlementAccountInput)
		want   error
	}{
		{
			// 017 起主体名是必填：账户号那列没了之后，它是这一行唯一给人看的名字。
			name:   "主体名空",
			mutate: func(in *dto.SettlementAccountInput) { in.PartyName = " " },
			want:   ErrSettlementAccountPartyNameRequired,
		},
		{
			name:   "主体类型不在词表",
			mutate: func(in *dto.SettlementAccountInput) { in.PartyType = "shop" },
			want:   ErrSettlementAccountPartyInvalid,
		},
		{
			name:   "渠道不在可分账的那几条里",
			mutate: func(in *dto.SettlementAccountInput) { in.Provider = "wechat_pay" },
			want:   ErrSettlementAccountProviderInvalid,
		},
		{
			name:   "渠道空",
			mutate: func(in *dto.SettlementAccountInput) { in.Provider = "" },
			want:   ErrSettlementAccountProviderInvalid,
		},
		{
			name:   "接收方类型不在词表",
			mutate: func(in *dto.SettlementAccountInput) { in.ReceiverType = "OPENID" },
			want:   ErrSettlementAccountReceiverTypeInvalid,
		},
		{
			name:   "子商户号空",
			mutate: func(in *dto.SettlementAccountInput) { in.ReceiverID = "" },
			want:   ErrSettlementAccountReceiverRequired,
		},
		{
			name:   "状态不在词表",
			mutate: func(in *dto.SettlementAccountInput) { in.Status = "paused" },
			want:   ErrSettlementAccountStatusInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSettlementService()
			in := dto.SettlementAccountInput{
				PartyName: "测试门店", PartyType: model.SettlementPartyMemberStore,
				Provider: "ums", ReceiverID: "MID-1",
			}
			tc.mutate(&in)
			_, err := svc.CreateAccount(context.Background(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateAccount 的错误 = %v，期望 %v", err, tc.want)
			}
			if !IsValidationError(err) {
				t.Fatalf("%v 没有被算成「请求不合法」", err)
			}
		})
	}

	t.Run("最小的一份能过，且补上默认值", func(t *testing.T) {
		svc, _ := newSettlementService()
		account, err := svc.CreateAccount(context.Background(), dto.SettlementAccountInput{
			PartyName: "测试门店", PartyType: model.SettlementPartyMemberStore,
			Provider: "ums", ReceiverID: "MID-1",
		})
		if err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
		// 银联商务按子商户号直接分，取不到「个人 openid」那个概念——008 的列默认值也是它。
		if account.ReceiverType != model.SettlementReceiverMerchantID {
			t.Fatalf("接收方类型 = %q，期望 %q", account.ReceiverType, model.SettlementReceiverMerchantID)
		}
		if account.Status != model.SettlementRecordEnabled {
			t.Fatalf("状态 = %q，期望 %q", account.Status, model.SettlementRecordEnabled)
		}
	})
}

// TestSettlementChannelsComeFromTheCatalog 验渠道下拉的取值来源。
//
// 它**不查库**：能分账的渠道由「哪条渠道的下单报文里能带子单」决定（catalog.Channel.
// Settlement），那是发版的事。今天只有银联商务一条；微信那条是委托代扣签约，适配器里没有一处
// 拼分账字段，所以它不该出现在这个下拉里——出现的话，运营会给它配账户，而那条渠道永远分不出钱。
func TestSettlementChannelsComeFromTheCatalog(t *testing.T) {
	svc, _ := newSettlementService()
	channels := svc.SettlementChannels()
	if len(channels) != 1 {
		t.Fatalf("可分账的渠道有 %d 条，期望 1 条", len(channels))
	}
	if channels[0].Provider != "ums" {
		t.Fatalf("渠道 = %q，期望 ums", channels[0].Provider)
	}
	if channels[0].Name == "" {
		t.Fatal("渠道名是空的：下拉里会出现一个没有名字的选项")
	}
}

// TestRatioHundredths 是那个换算的边界用例。
//
// 它是纯函数，也正是「比例」这件事上唯一一处会引入误差的地方——浮点尾差、NaN、负数、超范围
// 都要在这里被挡住，而不是等到写库那一刻。
func TestRatioHundredths(t *testing.T) {
	cases := []struct {
		percent float64
		want    int64
		ok      bool
	}{
		{percent: 45, want: 4500, ok: true},
		{percent: 45.5, want: 4550, ok: true},
		{percent: 0.01, want: 1, ok: true},
		{percent: 100, want: 10000, ok: true},
		{percent: 33.33, want: 3333, ok: true},
		{percent: 0, want: 0, ok: true},
		// 三位小数：不做四舍五入，拒掉（见 buildRuleItem 的说明）。
		{percent: 45.005, ok: false},
		{percent: 1.234, ok: false},
		// 负数与超范围由调用方按 (0,100] 判，但换算本身不能把它们算成一个看似合法的整数。
		{percent: -1, want: -100, ok: true},
		{percent: 100.5, want: 10050, ok: true},
	}
	for _, tc := range cases {
		got, ok := ratioHundredths(tc.percent)
		if ok != tc.ok {
			t.Fatalf("ratioHundredths(%v) 的可表示性 = %v，期望 %v", tc.percent, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("ratioHundredths(%v) = %d，期望 %d", tc.percent, got, tc.want)
		}
	}

	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, ok := ratioHundredths(bad); ok {
			t.Fatalf("ratioHundredths(%v) 被判成可表示", bad)
		}
	}
}
