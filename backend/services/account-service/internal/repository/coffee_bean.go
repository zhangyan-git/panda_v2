package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

var (
	// ErrInsufficientCoffeeBeans：余额不够扣。这是本服务**认识**的业务结果，不是故障
	// ——调用方（发起支付）应当把它翻译成「换一种支付方式」，而不是当成服务坏了去重试。
	ErrInsufficientCoffeeBeans = errors.New("coffee bean balance is insufficient")
	// ErrBeanEntryKeyConflict：幂等键撞上了别人的流水。与 ErrEntryKeyConflict 同一个理由：
	// 正常重放不会走到这里（同一个键必然属于同一个用户），撞上说明调用方把号发重了。
	ErrBeanEntryKeyConflict = errors.New("coffee bean entry key belongs to another account")
	// ErrBeanEntryNotFound：按主键或幂等键去读一笔刚刚确认存在的流水，却读不到。正常路径
	// 到不了这里，留着是为了那时候有一个说得清的错误，而不是一个裸的 pgx.ErrNoRows。
	ErrBeanEntryNotFound = errors.New("coffee bean entry not found")
	// ErrBeanAmountZero：调整额为 0。充 0 分等于什么都没做，几乎总是手滑。
	ErrBeanAmountZero = errors.New("coffee bean adjustment amount must not be zero")
	// ErrBeanAmountNotPositive：扣减额或冲正额不是正数。这两个方向的金额**必须**为正：
	// 负的扣减会被内部取负变成一次充值（余额涨了，约束一条都不违反），是最难发现的一类错。
	ErrBeanAmountNotPositive = errors.New("coffee bean amount must be positive")
)

// mapBeanPGError 把咖啡豆这两张表的约束冲突翻成调用方分得清的业务错误。
//
// 与 mapPGError 分开而不是合并：那张表里 pgx.ErrNoRows 翻的是福卡的 ErrEntryNotFound，
// 而这里要翻成 ErrBeanEntryNotFound——两个哨兵在 HTTP/gRPC 层是两句话，合并就会让「这笔
// 流水不存在」变成一句指错表的话。约束名同样是唯一稳定可依赖的东西，不按错误文本匹配。
func mapBeanPGError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBeanEntryNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "coffee_bean_entries_key_unique":
			return ErrBeanEntryKeyConflict
		case "coffee_bean_accounts_balance_check", "coffee_bean_entries_balance_after_check":
			// 只有一列能被约束，所以这两条 CHECK 是「余额不会被扣穿」的最后一道防线：
			// 正常路径在锁里已经先判过一次，走到这里说明有人绕开了那条判断。
			return ErrInsufficientCoffeeBeans
		}
	}
	return err
}

// beanEntryParams 是豆的三条写路径落到 SQL 之前的公共形状。
//
// 与 entryParams 同形，多了 operator_id / operator_name（后台调整是谁动的），少了冻结
// 相关的字段（豆没有冻结）。
type beanEntryParams struct {
	UserID          string
	EntryType       string
	Amount          int64
	Title           string
	ReferenceType   string
	ReferenceID     string
	ReferenceNo     string
	EntryKey        string
	ReversesEntryID *string
	OperatorID      string
	OperatorName    string
	Remark          string
	OccurredAt      time.Time
}

// applyBeanEntry 在调用方的事务里把一笔豆账变落地：锁账户行 → 算新余额 → 追加流水 → 更新余额。
//
// 顺序与 applyEntry（福卡）逐条相同，理由也一样：并发扣减靠行锁串行化而不是乐观重试；
// 幂等走唯一索引，撞上已有的行就回放那一笔，账户余额在这个分支里一格都不动。
//
// 两处不同，都是口径带来的：
//   - 没有冻结，所以「可用」就是 balance，判定只有 balance >= 0 一条；
//   - 没有「冲正不受冻结约束」那一段——豆的冲正只会把钱加回来（Amount 恒为正），
//     负数判定天然碰不到它。
//
// **幂等判定仍然必须在余额判定之前**，与福卡完全一样：同一笔扣减的重发（发起支付时的
// 超时重试）在余额已经扣走之后，必须回放原样，而不是得到一句「余额不足」——那会让调用方
// 把一次**已经生效**的扣减读成失败，然后按失败去补偿。
func (r *PostgresRepository) applyBeanEntry(ctx context.Context, tx pgx.Tx, p beanEntryParams) (EntryResult, error) {
	// 账户行懒创建：第一次调整或第一次扣减时才 INSERT。ON CONFLICT DO NOTHING 让并发
	// 创建也只留一行，后面的 FOR UPDATE 再把它锁住。
	if _, err := tx.Exec(ctx, `INSERT INTO coffee_bean_accounts (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING`, p.UserID); err != nil {
		return EntryResult{}, mapBeanPGError(err)
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT balance FROM coffee_bean_accounts
		WHERE user_id = $1 FOR UPDATE`, p.UserID).Scan(&current); err != nil {
		return EntryResult{}, mapBeanPGError(err)
	}

	// 幂等查在余额判定之前，顺序不能反，理由见函数头。放在 FOR UPDATE 之后是有意的：
	// 同一把键的两个并发请求会先在同一行账户上排队，后到的那个看到的必然是前一个提交后
	// 的流水表，不依赖 INSERT 撞唯一键来兜底。
	if replayed, found, err := lookupBeanByEntryKey(ctx, tx, p); err != nil {
		return EntryResult{}, err
	} else if found {
		return replayed, nil
	}

	balanceAfter := current + p.Amount
	if balanceAfter < 0 {
		// 冲正不会走到这里（见函数头）。扣减余额不足、以及后台调整填了一个大于余额的
		// 负数，都会落在这条上——两者对调用方是同一句话：这个数现在的余额撑不住。
		return EntryResult{}, ErrInsufficientCoffeeBeans
	}

	var entryID string
	var storedBalance int64
	err := tx.QueryRow(ctx, `INSERT INTO coffee_bean_entries
		(user_id,entry_type,amount,balance_after,title,reference_type,reference_id,reference_no,
		 entry_key,reverses_entry_id,operator_id,operator_name,remark,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (entry_key) DO NOTHING
		RETURNING id::text, balance_after`,
		p.UserID, p.EntryType, p.Amount, balanceAfter, p.Title, p.ReferenceType, p.ReferenceID,
		p.ReferenceNo, p.EntryKey, p.ReversesEntryID, p.OperatorID, p.OperatorName, p.Remark,
		p.OccurredAt).Scan(&entryID, &storedBalance)
	if errors.Is(err, pgx.ErrNoRows) {
		return replayBeanEntry(ctx, tx, p)
	}
	if err != nil {
		return EntryResult{}, mapBeanPGError(err)
	}

	if _, err := tx.Exec(ctx, `UPDATE coffee_bean_accounts
		SET balance = $2, updated_at = NOW() WHERE user_id = $1`, p.UserID, balanceAfter); err != nil {
		return EntryResult{}, mapBeanPGError(err)
	}
	return EntryResult{EntryID: entryID, BalanceAfter: storedBalance}, nil
}

// lookupBeanByEntryKey 按幂等键找一条已经存在的豆流水。找到与否是第二个返回值，
// 没找到不是错误。顺带校验归属，理由与福卡那份逐字相同：回放别人的流水比报错更糟。
func lookupBeanByEntryKey(ctx context.Context, tx pgx.Tx, p beanEntryParams) (EntryResult, bool, error) {
	var entryID, userID string
	var balanceAfter int64
	err := tx.QueryRow(ctx, `SELECT id::text, user_id::text, balance_after
		FROM coffee_bean_entries WHERE entry_key = $1`, p.EntryKey).Scan(&entryID, &userID, &balanceAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return EntryResult{}, false, nil
	}
	if err != nil {
		return EntryResult{}, false, mapBeanPGError(err)
	}
	if userID != p.UserID {
		return EntryResult{}, false, fmt.Errorf("%w: %s", ErrBeanEntryKeyConflict, p.EntryKey)
	}
	return EntryResult{EntryID: entryID, BalanceAfter: balanceAfter, Replayed: true}, true, nil
}

// replayBeanEntry 是 INSERT 撞唯一键之后的那条兜底路径：到了这里键一定已经存在。
//
// 上面那次显式查询已经把绝大多数重放接走了，这条留着是为了「两个请求同时通过查询、
// 其中一个抢先插入」这种情形——那时另一个的 INSERT 会因为 ON CONFLICT DO NOTHING
// 回 0 行，由这里回放抢先去的那一笔。
func replayBeanEntry(ctx context.Context, tx pgx.Tx, p beanEntryParams) (EntryResult, error) {
	replayed, found, err := lookupBeanByEntryKey(ctx, tx, p)
	if err != nil {
		return EntryResult{}, err
	}
	if !found {
		// 撞了唯一键却查不到：只可能是同一个事务里刚插入的行被别处删掉了，而这张表是
		// 只追加的。真到了这里宁可报错，也不能回一个空结果给调用方。
		return EntryResult{}, fmt.Errorf("%w: %s", ErrBeanEntryNotFound, p.EntryKey)
	}
	return replayed, nil
}

// BeanConsumeParams 是一次纯豆出资的扣减。
//
// Amount 是**正数**（这张订单要用掉多少豆），内部取负后再落流水——与福卡的 Deduct 同一条
// 规矩：符号写在哪一边是记账的事，调用方不该关心。
//
// OrderID 同时是幂等键的来源（order:{orderID}），所以必填；OrderNo 只是给人看的号。
type BeanConsumeParams struct {
	UserID     string
	OrderID    string
	OrderNo    string
	Amount     int64
	Title      string
	Remark     string
	OccurredAt time.Time
}

// ConsumeBeans 扣减咖啡豆：一张订单用豆全额付掉了。
//
// 幂等键由订单 ID 派生（见 model.BeanConsumeKey），所以重发一次支付请求撞在同一个键上，
// 回放原来那笔而不是扣第二次——payment-service 的超时重试就靠这一条。
func (r *PostgresRepository) ConsumeBeans(ctx context.Context, params BeanConsumeParams) (EntryResult, error) {
	if params.Amount <= 0 {
		return EntryResult{}, fmt.Errorf("%w: %d", ErrBeanAmountNotPositive, params.Amount)
	}
	title := strings.TrimSpace(params.Title)
	if title == "" {
		title = "咖啡豆支付"
	}
	var result EntryResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		result, err = r.applyBeanEntry(ctx, tx, beanEntryParams{
			UserID:    params.UserID,
			EntryType: model.BeanEntryTypeConsume,
			// 扣减在流水上是负数：调用方给的是「这单要用掉多少豆」，符号由这里定。
			Amount:        -params.Amount,
			Title:         title,
			ReferenceType: model.BeanReferenceTypeOrder,
			ReferenceID:   params.OrderID,
			ReferenceNo:   params.OrderNo,
			EntryKey:      model.BeanConsumeKey(params.OrderID),
			// 备注原样带过去，与 ReverseBeans 同一写法（TrimSpace 由服务层做）。漏掉这一行
			// 的后果是静默的：扣减照样成功、流水照样有，只是调用方写的那句话永远不在上面
			// ——而调用方写它，正是因为事后只有它能解释这笔钱是为什么扣的。
			Remark:     params.Remark,
			OccurredAt: params.OccurredAt,
		})
		return err
	})
	if err != nil {
		return EntryResult{}, err
	}
	return result, nil
}

// BeanReverseParams 是一次退款冲正：这一单的退款成功了，把扣掉的豆还回去。
//
// Amount 是这一次要还回去的金额（正数，单位分）。与福卡的 Reverse 按被冲正那笔**原样
// 反向**不同，这里允许只冲一部分——部分退款（先退加购行、再退整单）是常态，所以金额由
// 调用方给，上限由这里按「这笔扣减还没冲回的部分」卡住。
//
// AfterSaleNo 既是幂等键（after_sale:{no}，一张售后只能冲一次），也是流水的 reference_no；
// AfterSaleID 进 reference_id。没有 UserID：冲回谁的钱由那笔扣减记录说了算，事件里说的
// 是同一件事的第二遍，不该成为第二个来源。
type BeanReverseParams struct {
	OrderID     string
	AfterSaleID string
	AfterSaleNo string
	Amount      int64
	Title       string
	Remark      string
	OccurredAt  time.Time
}

// ReverseBeans 冲正一单的纯豆扣减。第二个返回值是「本次真的冲了一笔」。
//
// **找不到扣减流水就 no-op 返回**，不报错：绝大多数订单是渠道支付的，这条路径天天会被
// 走到，报错会把一条正常的事件推进重试链，一路重试到死信——而它要表达的事是「这一单没用
// 豆付过，没什么可退的」，那本来就不是错误。重放同一条售后时 bool 是 false（那一笔已经
// 在库里了），事件的处理方因此不必靠事件幂等来推断这件事。
//
// 上限判定不能省：订单域已经按「实付 - 已退 - 在途」钳过退款额，理论上撞不到，真撞到说明
// 有一处算错了。这里报错而不是静默钳制——欠退比错退更容易被忽略。
//
// **「这条售后冲过了吗」必须在上限判定之前问**，次序在这里是有后果的：一条
// order.after_sale.refunded 重投进来时（relay 是 at-least-once，重投是常态，不是异常），
// 若先算剩余可冲额，一笔已经全额冲回的扣减算出来的剩余是 0，于是这次重投会撞成
// ErrReverseUncovered——调用方看到的是一次失败，把这条本该结束的事件一路重试到死信，
// 而这笔账其实早就结清了。与 applyBeanEntry 里那条「幂等查在余额判定之前」是同一条规则。
func (r *PostgresRepository) ReverseBeans(ctx context.Context, params BeanReverseParams) (bool, error) {
	if params.Amount <= 0 {
		return false, fmt.Errorf("%w: %d", ErrBeanAmountNotPositive, params.Amount)
	}
	title := strings.TrimSpace(params.Title)
	if title == "" {
		title = "退款冲正"
	}
	var reversed bool
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var (
			entryID  string
			userID   string
			consumed int64 // 负数
		)

		// 这一条售后冲过了没有。命中就什么都不做，reversed 保持 false——与「这单没用豆
		// 付过」是同一个结论、同一个返回值：调用方什么都不必做。
		var alreadyReversedEntryID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM coffee_bean_entries WHERE entry_key = $1`,
			model.BeanReverseKey(params.AfterSaleNo)).Scan(&alreadyReversedEntryID)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return mapBeanPGError(err)
		}

		// FOR UPDATE 锁的是**那笔扣减**，锁到本条事务结束。下面那句「已经冲回过多少」是按
		// 它反查出来的，而它一旦被读出来就该是这整段判断的一个快照：没有这把锁，两条针对
		// 同一张订单的冲正（两条不同的售后单同时退款成功）读到的已冲回额都是同一个旧值，
		// 各自都觉得自己没超，于是合起来退掉超过扣减额的钱——余额只增不减，没有任何约束
		// 拦得住，也不会有报错。
		//
		// 全仓只有这一条路径会锁**已有的流水行**，而且它在下面 applyBeanEntry 去锁账户行之前
		// 就拿到了；其余路径（发放/扣减/后台调整）都是先账户行、且从不回头等某一行流水。
		// 所以这两把锁之间的方向是单向的，不会成环——把这一句挪到账户行锁之后就会。
		err = tx.QueryRow(ctx, `SELECT id::text, user_id::text, amount FROM coffee_bean_entries
			WHERE entry_key = $1 AND entry_type = $2 FOR UPDATE`,
			model.BeanConsumeKey(params.OrderID), model.BeanEntryTypeConsume).
			Scan(&entryID, &userID, &consumed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapBeanPGError(err)
		}

		// 已经冲回过多少：这一笔扣减的所有冲正流水之和（正数）。必须减掉它，否则第二次
		// 部分退（先退加购行、再退整单）会把上一次退过的部分又退一遍。
		var alreadyReversed int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount), 0) FROM coffee_bean_entries
			WHERE reverses_entry_id = $1`, entryID).Scan(&alreadyReversed); err != nil {
			return mapBeanPGError(err)
		}
		if remaining := -consumed - alreadyReversed; params.Amount > remaining {
			return fmt.Errorf("%w: order %s has %d left to reverse, asked for %d",
				ErrReverseUncovered, params.OrderID, remaining, params.Amount)
		}

		reversedID := entryID
		result, err := r.applyBeanEntry(ctx, tx, beanEntryParams{
			UserID:    userID,
			EntryType: model.BeanEntryTypeReverse,
			// 与它冲掉的那笔相反：扣减是负的，还回去就是正的。
			Amount:          params.Amount,
			Title:           title,
			ReferenceType:   model.BeanReferenceTypeAfterSale,
			ReferenceID:     params.AfterSaleID,
			ReferenceNo:     params.AfterSaleNo,
			EntryKey:        model.BeanReverseKey(params.AfterSaleNo),
			ReversesEntryID: &reversedID,
			Remark:          params.Remark,
			OccurredAt:      params.OccurredAt,
		})
		if err != nil {
			return err
		}
		reversed = !result.Replayed
		return nil
	})
	if err != nil {
		return false, err
	}
	return reversed, nil
}

// beanEntryColumns 是 coffee_bean_entries 的读取列，顺序与 scanBeanEntry 严格一一对应。
// UUID 列一律 ::text：pgx 把 uuid 扫进 string 需要这一步。
const beanEntryColumns = `id::text, user_id::text, entry_type, amount, balance_after, title,
	reference_type, reference_id, reference_no, entry_key, reverses_entry_id::text,
	operator_id, operator_name, remark, occurred_at, created_at`

func scanBeanEntry(row interface{ Scan(...any) error }) (*model.CoffeeBeanEntry, error) {
	entry := &model.CoffeeBeanEntry{}
	if err := row.Scan(&entry.ID, &entry.UserID, &entry.EntryType, &entry.Amount, &entry.BalanceAfter,
		&entry.Title, &entry.ReferenceType, &entry.ReferenceID, &entry.ReferenceNo, &entry.EntryKey,
		&entry.ReversesEntryID, &entry.OperatorID, &entry.OperatorName, &entry.Remark,
		&entry.OccurredAt, &entry.CreatedAt); err != nil {
		return nil, err
	}
	return entry, nil
}

// GetBeanAccount 读一个用户的豆账户。没有账户行时返回 pgx.ErrNoRows——那是「还没充过豆」，
// 由服务层翻译成余额 0，不是「查不到这个人」。
func (r *PostgresRepository) GetBeanAccount(ctx context.Context, userID string) (*model.CoffeeBeanAccount, error) {
	account := &model.CoffeeBeanAccount{}
	err := r.pool.QueryRow(ctx, `SELECT user_id::text, balance, created_at, updated_at
		FROM coffee_bean_accounts WHERE user_id = $1`, userID).
		Scan(&account.UserID, &account.Balance, &account.CreatedAt, &account.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return account, nil
}

// ListBeanEntries 按过滤条件分页读豆流水，返回 (当页, 总数)。
//
// 排序与 coffee_bean_entries_user_idx 一致（occurred_at DESC, id）：同一毫秒的两笔也有
// 稳定顺序，翻页不会因为并列而跳着走。
func (r *PostgresRepository) ListBeanEntries(ctx context.Context, q dto.BeanEntryQuery) ([]*model.CoffeeBeanEntry, int, error) {
	conds := make([]string, 0, 5)
	args := make([]any, 0, 5)
	add := func(clause string, value any) {
		args = append(args, value)
		conds = append(conds, fmt.Sprintf(clause, len(args)))
	}
	if q.UserID != "" {
		add("user_id = $%d", q.UserID)
	}
	if q.ReferenceNo != "" {
		add("reference_no = $%d", q.ReferenceNo)
	}
	if q.EntryType != "" {
		add("entry_type = $%d", q.EntryType)
	}
	// 闭开区间 [From, To)：同一条流水只落在相邻两天的其中一边，导出拼接不漏不重。
	if q.From != nil {
		add("occurred_at >= $%d", *q.From)
	}
	if q.To != nil {
		add("occurred_at < $%d", *q.To)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM coffee_bean_entries`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), q.PageSize, api.PageOffset(q.Page, q.PageSize))
	rows, err := r.pool.Query(ctx, `SELECT `+beanEntryColumns+` FROM coffee_bean_entries`+where+
		fmt.Sprintf(` ORDER BY occurred_at DESC, id LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	entries := make([]*model.CoffeeBeanEntry, 0, q.PageSize)
	for rows.Next() {
		entry, err := scanBeanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}
