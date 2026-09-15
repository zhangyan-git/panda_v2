package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// ErrBeanDuplicateRequest：同一个 requestId 又来了一次。
//
// 与扣减那条路**刻意不同**：那里同一个幂等键回放原样（调用方是 payment-service，它要的
// 就是「这笔扣过的结果」）。这里是**人点的**——回放原样会让界面上显示成功，而管理员多半
// 会再点一次，于是充两次。宁可回一个 409 让人去查流水，也不替它下「已经充过了」这个结论。
var ErrBeanDuplicateRequest = errors.New("request_id already recorded")

// BeanAdjustParams 是一次后台人工余额调整。
//
// Amount **带符号**（单位分）：充值为正、把充错的豆调回来为负，0 不允许。RequestID 是幂等
// 号且必填——重试一次就得分辨出「这是同一次调整」，否则管理员的一次点击在超时重发之后
// 会变成两次加钱。
//
// Operator 是操作人的管理员用户 ID（来自令牌的 UserID）：它同时落进流水的 operator_id 与
// 审计的 actor_id，两处因此说的是同一个人。
type BeanAdjustParams struct {
	UserID     string
	Amount     int64
	RequestID  string
	Remark     string
	Operator   string
	OccurredAt time.Time
}

// BeanAdminRepository 是咖啡豆域的后台写路径。
//
// 单独一个类型、而不是给 PostgresRepository 加一个 recorder 字段，理由与
// coffee-machine-service 的 AdminRepository 逐字相同：读路径（账户、流水）不该为了一个
// 用不到的 recorder 多传一个参数。
//
// 它复用同一个 PostgresRepository 的写原语与事务包装，而不是自己再拿一个 pool：人工调整
// 与扣减落的是**同一本流水**，「两条路径抢的是同一把行锁」这句话只有在它们走同一份代码时
// 才成立。
type BeanAdminRepository struct {
	beans *PostgresRepository
	audit audit.Recorder
}

// NewBeanAdminRepository 创建后台调整的数据访问实现。
func NewBeanAdminRepository(beans *PostgresRepository, recorder audit.Recorder) *BeanAdminRepository {
	// 传 nil 等价于关掉审计，而不是「每次写都 panic」——调用方少传一个参数时，失败的应该
	// 是审计开关这么明显的东西，不是运行期的空指针。
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &BeanAdminRepository{beans: beans, audit: recorder}
}

// beanAdjustSnapshot 是余额在审计里的样子。
//
// 这就是方案 11.6 L893 那张必审清单要的 before-image：改之前是多少、这次动了多少、改完是
// 多少，三样一起落进 admin_operation_logs。没有它，一次改错的余额在库里就只剩「现在是 X」，
// 无从回溯是谁把它从哪改过来的。
//
// 方案 11.6 还列了一个 reason 字段，而身份库的 admin_operation_logs **没有对应列**。调整
// 理由因此落在 Remark 上，与运营自己填的那句话逐字一致——不是静默丢掉，只是换了存放的
// 位置（见本文件 AdjustBeans 的调用处）。
type beanAdjustSnapshot struct {
	Balance   int64  `json:"balance"`
	Amount    int64  `json:"amount"`
	RequestID string `json:"request_id"`
	Remark    string `json:"remark"`
}

// AdjustBeans 调整一个用户的咖啡豆余额：改余额、追加流水、记审计，三件事一个事务。
//
// 这是**唯一**能凭空改动豆余额的入口（另一条写路径是扣减，它只能把钱扣走；冲正只能把扣过
// 的还回来）。所以它必须留痕，而且留痕要与这次写入同生共死——审计行进了同一事务的
// message_outbox，由 relay 投到身份库（见 platform/audit 的包注释）。
func (r *BeanAdminRepository) AdjustBeans(ctx context.Context, in BeanAdjustParams) (EntryResult, error) {
	// 0 在进事务之前就挡掉。数据库那条 amount <> 0 的 CHECK 也会挡住它，但那会以约束违反
	// 的形式回一个 500，而真正的原因是「填了 0」。
	if in.Amount == 0 {
		return EntryResult{}, ErrBeanAmountZero
	}
	// 文案由本服务拥有。同一个列带符号，但人按方向理解这件事：正数是充值，负数是纠错
	// （把充错的豆调回来）。两处都写「后台调整」会让人以为后台只有充值一条路。
	title := "后台充值"
	if in.Amount < 0 {
		title = "后台调整"
	}
	remark := strings.TrimSpace(in.Remark)

	var result EntryResult
	err := r.beans.inTx(ctx, func(tx pgx.Tx) error {
		res, err := r.beans.applyBeanEntry(ctx, tx, beanEntryParams{
			UserID:    in.UserID,
			EntryType: model.BeanEntryTypeAdjust,
			Amount:    in.Amount,
			Title:     title,
			// 调整没有「起因对象」的 ID：后台填的那张单子不在这个系统里。reference_no
			// 因此放调用方给的幂等号，后台明细按它检索；reference_id 留空。
			ReferenceType: model.BeanReferenceTypeManual,
			ReferenceNo:   in.RequestID,
			EntryKey:      model.BeanAdjustKey(in.RequestID),
			OperatorID:    in.Operator,
			Remark:        remark,
			OccurredAt:    in.OccurredAt,
		})
		if err != nil {
			return err
		}
		// 重放（同一个 requestId 已经记过账）在这里变成一次明确的失败，整个事务回滚。
		// 见 ErrBeanDuplicateRequest 的说明：这一次是人点的，回放原样是个更坏的回答。
		if res.Replayed {
			return ErrBeanDuplicateRequest
		}

		// before 由 after 倒推：两个数出自同一个事务、同一把行锁下的同一次读改写，不另开
		// 一次查询（那只会读到同一行，白多一次往返）。BalanceAfter 也只在非重放分支可信，
		// 而重放已经在上面返回了。
		before := res.BalanceAfter - in.Amount
		if err := r.audit.Record(ctx, tx, audit.Entry{
			// ActorID 用传进来的操作人，不留给 Recorder 去 ctx 里找：这条路径已经把操作人
			// 写进了流水的 operator_id，两处各取各的来源就会出现「流水说是甲改的、审计说
			// 找不到人」。传空串时 Recorder 才回落到 ctx（见 platform/audit），所以非 HTTP
			// 的调用方也不会把操作人整个丢掉。
			ActorID:    in.Operator,
			Module:     "coffee_bean",
			Action:     "adjust",
			Operation:  title,
			TargetType: "user", TargetID: in.UserID,
			Before: audit.Snapshot(beanAdjustSnapshot{Balance: before}),
			After: audit.Snapshot(beanAdjustSnapshot{
				Balance: res.BalanceAfter, Amount: in.Amount, RequestID: in.RequestID, Remark: remark,
			}),
		}); err != nil {
			return err
		}
		result = res
		return nil
	})
	if err != nil {
		return EntryResult{}, err
	}
	return result, nil
}
