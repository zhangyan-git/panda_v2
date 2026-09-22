package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// EntryResult 是一次写入的结果。
//
// Replayed 为真时 EntryID / BalanceAfter 是**当时那笔**的值，不是此刻的余额：重放要么
// 是同一个调用方在重试，它要的就是当时那个回答；要么是一笔冲正回放，答当前余额会与
// 它刚刚拿到的 entry_id 对不上。BalanceAfter 的列注释也写着同一件事。
type EntryResult struct {
	EntryID      string
	BalanceAfter int64
	Replayed     bool
}

// GrantLine 是一条要记进流水的发放。
//
// Title 是服务侧渲染好的文案（文案属于账户域，见 fortune_card_entries.title 的列注释）；
// 这个结构体本身不含种类与活动 ID——那是订单域拆快照时才需要的信息，落库后只体现在
// EntryKey 与文案里。
type GrantLine struct {
	Title    string
	Amount   int64
	EntryKey string
}

// GrantParams 是一次「订单完成 ⇒ 福卡到账」。
type GrantParams struct {
	UserID     string
	OrderID    string
	OrderNo    string
	OccurredAt time.Time
	Lines      []GrantLine
}

// DeductParams 是一次扣减。Amount 是正数（要扣掉几张），内部取负后再落流水——
// 调用方不该关心「减号写在哪一边」。
type DeductParams struct {
	UserID        string
	Amount        int64
	Title         string
	ReferenceType string
	ReferenceID   string
	ReferenceNo   string
	EntryKey      string
	Remark        string
	OccurredAt    time.Time
}

// ReverseParams 是一次冲正。
type ReverseParams struct {
	EntryID    string
	Title      string
	Remark     string
	OccurredAt time.Time
}

// FreezeParams 是一次退款申请引起的冻结。
//
// EntryKeys 是订单域按退款范围拆好的发放幂等键（退加购行只给加赠那张）。给键而不是给
// 张数：申请可能早于发放，那时流水还不存在，金额要等发放落库时才算得出来。
type FreezeParams struct {
	UserID      string
	AfterSaleNo string
	OrderID     string
	OrderNo     string
	EntryKeys   []string
	Reason      string
	OccurredAt  time.Time
}

// PreviewFreezeParams 是一次「冻得上吗」的预览：订单域在受理退款申请之前问的那一句。
//
// 只有用户与发放键：预览不看售后单，它问的不是「这张单冻过没有」而是「此刻冻得上几张」。
type PreviewFreezeParams struct {
	UserID    string
	EntryKeys []string
}

// ReleaseParams 是一次解冻（申请被驳回或用户撤销）。
//
// 只有售后单号：解冻不需要知道冻了哪些键、冻了多少——那些都在冻结行上，而这一动作的
// 语义是「把这张申请冻住的全部放回去」。
type ReleaseParams struct {
	AfterSaleNo string
	Reason      string
	OccurredAt  time.Time
}

// RecoverParams 是一次追回（退款成功，这一单送出去的福卡从账上收回来）。
//
// 只有售后单号：要冲哪几笔发放、总共几张，都在冻结行上（entry_keys 与 amount）——
// 让调用方再送一份就是把订单域拆分层的规则复制到第二个地方，两份迟早对不上。
type RecoverParams struct {
	AfterSaleNo string
	Title       string
	Remark      string
	Reason      string
	OccurredAt  time.Time
}

// entryParams 是三条写路径落到 SQL 之前的公共形状。
type entryParams struct {
	UserID          string
	EntryType       string
	Amount          int64
	Title           string
	ReferenceType   string
	ReferenceID     string
	ReferenceNo     string
	EntryKey        string
	ReversesEntryID *string
	Remark          string
	OccurredAt      time.Time
}

// applyEntry 在调用方的事务里把一笔账变落地：锁账户行 → 算新余额 → 追加流水 → 更新余额。
//
// 并发扣减靠那把行锁串行化，不靠乐观重试：两个抽奖请求同时进来时，后一个会等在
// SELECT ... FOR UPDATE 上，拿到的是前一个提交后的余额。没有这把锁，两笔都会读到同一个
// 旧余额，然后在 UPDATE 上互相覆盖——余额少了，但流水多了一笔。
//
// 幂等走唯一索引：entry_key 撞上已有行时不再新增，而是回放那一笔。账户余额在这个分支里
// **一格都不动**，这正是「重投一条 order.completed 不会发两次福卡」的全部依据。
func (r *PostgresRepository) applyEntry(ctx context.Context, tx pgx.Tx, p entryParams) (EntryResult, error) {
	// 账户行懒创建：第一次发放时才 INSERT。ON CONFLICT DO NOTHING 让并发创建也只留一行，
	// 后面的 FOR UPDATE 再把它锁住。
	if _, err := tx.Exec(ctx, `INSERT INTO fortune_card_accounts (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING`, p.UserID); err != nil {
		return EntryResult{}, mapPGError(err)
	}
	// 余额与冻结额一起读：扣减的判据是**可用**（余额 - 冻结），两个数必须来自同一把锁下的
	// 同一个时刻，否则一次并发的冻结会让这里拿着过期冻结额放行一笔本该被挡住的扣减。
	var current, frozen int64
	if err := tx.QueryRow(ctx, `SELECT balance, frozen_balance FROM fortune_card_accounts
		WHERE user_id = $1 FOR UPDATE`, p.UserID).Scan(&current, &frozen); err != nil {
		return EntryResult{}, mapPGError(err)
	}

	// 幂等的判定在余额判定**之前**，顺序不能反：同一笔冲正的重放（调用方超时重试）在
	// 余额已经归零之后必须仍然回放原样，而不是在这里得到一句「余额不足」。反过来的话，
	// 调用方会把一次**已经生效**的冲正读成失败，然后按失败去补偿——代价是双倍的错误。
	// 同一个键也意味着同一个用户，所以这一步顺带把「幂等号发重到别人头上」挡掉。
	//
	// 放在 FOR UPDATE 之后是有意的：同一把键的两个并发请求会先在同一行账户上排队，
	// 后到的那个看到的必然是前一个提交后的流水表，不依赖 INSERT 撞唯一键来兜底。
	if replayed, found, err := lookupByEntryKey(ctx, tx, p); err != nil {
		return EntryResult{}, err
	} else if found {
		return replayed, nil
	}

	balanceAfter := current + p.Amount
	if balanceAfter < 0 {
		if p.EntryType == model.EntryTypeReverse {
			return EntryResult{}, ErrReverseUncovered
		}
		return EntryResult{}, ErrInsufficientFortuneCards
	}
	// 冻结只挡**扣减**（抽奖），不挡发放与冲正。
	//
	// 冲正（entry_type=reverse，金额多为负）明确不受冻结约束：将来的「退款成功 ⇒ 追回福卡」
	// 走的就是它，而那一刻冻结尚未解除（解冻与冲正是同一笔业务的半步，顺序由那条链路定）。
	// 拿可用余额去挡它，会让退款追回永远失败。代价是冲正可能让 frozen_balance > balance——
	// 那是 fortune_card_accounts_frozen_within_balance 那条 CHECK 的活，不是这里的。
	if p.Amount < 0 && p.EntryType != model.EntryTypeReverse && current-frozen+p.Amount < 0 {
		return EntryResult{}, ErrInsufficientFortuneCards
	}

	var entryID string
	var storedBalance int64
	err := tx.QueryRow(ctx, `INSERT INTO fortune_card_entries
		(user_id,entry_type,amount,balance_after,title,reference_type,reference_id,reference_no,
		 entry_key,reverses_entry_id,remark,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (entry_key) DO NOTHING
		RETURNING id::text, balance_after`,
		p.UserID, p.EntryType, p.Amount, balanceAfter, p.Title, p.ReferenceType, p.ReferenceID,
		p.ReferenceNo, p.EntryKey, p.ReversesEntryID, p.Remark, p.OccurredAt).Scan(&entryID, &storedBalance)
	if errors.Is(err, pgx.ErrNoRows) {
		return replayEntry(ctx, tx, p)
	}
	if err != nil {
		return EntryResult{}, mapPGError(err)
	}

	if _, err := tx.Exec(ctx, `UPDATE fortune_card_accounts
		SET balance = $2, updated_at = NOW() WHERE user_id = $1`, p.UserID, balanceAfter); err != nil {
		return EntryResult{}, mapPGError(err)
	}
	return EntryResult{EntryID: entryID, BalanceAfter: storedBalance}, nil
}

// lookupByEntryKey 按幂等键找一条已经存在的流水。找到与否是第二个返回值，没找到不是错误。
//
// 顺带校验归属：同一个键必然属于同一个用户，撞到别人头上说明调用方把幂等号发重了。
// 这时候回放那一笔是**错的**——调用方会拿到另一个人的 entry_id 与余额。宁可报错。
func lookupByEntryKey(ctx context.Context, tx pgx.Tx, p entryParams) (EntryResult, bool, error) {
	var entryID, userID string
	var balanceAfter int64
	err := tx.QueryRow(ctx, `SELECT id::text, user_id::text, balance_after
		FROM fortune_card_entries WHERE entry_key = $1`, p.EntryKey).Scan(&entryID, &userID, &balanceAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return EntryResult{}, false, nil
	}
	if err != nil {
		return EntryResult{}, false, mapPGError(err)
	}
	if userID != p.UserID {
		return EntryResult{}, false, fmt.Errorf("%w: %s", ErrEntryKeyConflict, p.EntryKey)
	}
	return EntryResult{EntryID: entryID, BalanceAfter: balanceAfter, Replayed: true}, true, nil
}

// replayEntry 是 INSERT 撞唯一键之后的那条兜底路径：到了这里键一定已经存在。
//
// 上面那次显式查询已经把绝大多数重放接走了，这条留着是为了「两个请求同时通过查询、
// 其中一个抢先插入」这种情形——那时另一个的 INSERT 会因为 ON CONFLICT DO NOTHING
// 回 0 行，由这里回放抢先去的那一笔。
func replayEntry(ctx context.Context, tx pgx.Tx, p entryParams) (EntryResult, error) {
	replayed, found, err := lookupByEntryKey(ctx, tx, p)
	if err != nil {
		return EntryResult{}, err
	}
	if !found {
		// 撞了唯一键却查不到：只可能是同一个事务里刚插入的行被别处删掉了，而这张表是
		// 只追加的。真到了这里宁可报错，也不能回一个空结果给调用方。
		return EntryResult{}, fmt.Errorf("%w: %s", ErrEntryNotFound, p.EntryKey)
	}
	return replayed, nil
}

// GrantOrderFortune 把一单的每一笔发放记进流水，整体一个事务。
//
// 一笔失败整单回滚：只落了一半的发放会让「这单承诺 2 张」变成一句无从追溯的话，
// 而事件还会被重投——重投时那一半又撞在幂等键上被跳过，缺的那一半永远补不上。
//
// 每一条发放落库后还要问一次「有没有退款申请正冻着这个键」：paid 状态就允许申请退款
// （见 order-service 的 ApplyAfterSale），所以**冻结可能早于发放到达**。那时冻结行冻的是
// 0 张（流水还不存在），张数由这里补进去。补的条件是这一笔不是重放——重放意味着当初插入时
// 已经问过一次，再问一遍就是重复计数。
func (r *PostgresRepository) GrantOrderFortune(ctx context.Context, params GrantParams) ([]EntryResult, error) {
	results := make([]EntryResult, 0, len(params.Lines))
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		for _, line := range params.Lines {
			result, err := r.applyEntry(ctx, tx, entryParams{
				UserID:        params.UserID,
				EntryType:     model.EntryTypeGrant,
				Amount:        line.Amount,
				Title:         line.Title,
				ReferenceType: model.ReferenceTypeOrder,
				ReferenceID:   params.OrderID,
				ReferenceNo:   params.OrderNo,
				EntryKey:      line.EntryKey,
				OccurredAt:    params.OccurredAt,
			})
			if err != nil {
				return err
			}
			results = append(results, result)
			if result.Replayed {
				continue
			}
			if err := bindGrantToFreezes(ctx, tx, params.UserID, line.EntryKey, line.Amount); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// bindGrantToFreezes 把刚发下去的张数补进正盖着这个键的冻结行。
//
// 调用时账户行已被本事务锁住、且这一笔发放刚刚落库，所以这里读到的 balance 是更新后的、
// frozen_balance 是这批冻结行算过的。补进去的是**这一笔发放的全部张数**，被可用余额钳住
// 为止——冻的总额不该超过发出去的数，也不该超过此刻还剩下的可用。
//
// 别拿冻结行已有的 amount 去减本次张数：一条冻结行盖的是它那一单的**全部**发放键
// （entry_keys 是数组），行上的 amount 是这些键的**总和**，而这里手里只有本次一个键的
// 张数，两个口径相减没有意义。这么减过的版本在多键订单上会少冻——比如一条冻着 1 张的
// 行（来自基础键）等来了 2 张的幸运杯套，它只补 min(2, 2-1)=1 张，剩下那张卡照旧能抽奖，
// 而「申请退款之后不能再抽」正是这条链路要堵的口子。少冻是静默的：不变量不破，没有报错。
func bindGrantToFreezes(ctx context.Context, tx pgx.Tx, userID, entryKey string, amount int64) error {
	// 只挑一条：同一把发放键被两条冻结行同时盖住是罕见情形（同一行的两张卡会被 order 侧的
	// 占用互斥挡住），真碰上时这一次的张数只能给一边，按申请时间从早到晚。
	var freezeID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM fortune_card_freezes
		WHERE user_id = $1 AND status = $2 AND $3 = ANY(entry_keys)
		ORDER BY occurred_at, id FOR UPDATE LIMIT 1`,
		userID, model.FreezeStatusFrozen, entryKey).Scan(&freezeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapPGError(err)
	}

	var balance, frozen int64
	if err := tx.QueryRow(ctx, `SELECT balance, frozen_balance FROM fortune_card_accounts
		WHERE user_id = $1`, userID).Scan(&balance, &frozen); err != nil {
		return mapPGError(err)
	}
	// 预算 = 这一次发出去的张数，再被可用余额钳住。
	budget := amount
	if available := balance - frozen; available < budget {
		budget = available
	}
	if budget <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE fortune_card_freezes
		SET amount = amount + $2, updated_at = NOW() WHERE id = $1`, freezeID, budget); err != nil {
		return mapPGError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE fortune_card_accounts
		SET frozen_balance = frozen_balance + $2, updated_at = NOW() WHERE user_id = $1`,
		userID, budget); err != nil {
		return mapPGError(err)
	}
	return nil
}

// FreezeAfterSale 冻结一单的福卡：用户提交退款申请，那些卡从这一刻起不能拿去抽奖。
//
// 幂等压在 after_sale_no 的唯一索引上：同一条 applied 事件重投多少次都只有一行冻结、
// frozen_balance 也只加一次。
//
// 冻结**不写流水**：余额没变过。它改的是可用（balance - frozen_balance），也就是扣减的判据。
func (r *PostgresRepository) FreezeAfterSale(ctx context.Context, params FreezeParams) (bool, error) {
	var created bool
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// 账户行懒创建：这一单可能一张福卡都还没发（paid 状态就申请退款），但冻结行有外键
		// 指向账户行，不先建出来就会被挡在门外——而「申请早于发放」正是要支持的情形。
		if _, err := tx.Exec(ctx, `INSERT INTO fortune_card_accounts (user_id) VALUES ($1)
			ON CONFLICT (user_id) DO NOTHING`, params.UserID); err != nil {
			return mapPGError(err)
		}
		var balance, frozen int64
		if err := tx.QueryRow(ctx, `SELECT balance, frozen_balance FROM fortune_card_accounts
			WHERE user_id = $1 FOR UPDATE`, params.UserID).Scan(&balance, &frozen); err != nil {
			return mapPGError(err)
		}
		// 幂等判定在账户行锁之后：同一张申请的两个并发投递会先在这把锁上排队，
		// 后到的那个看到的必然是前一个提交后的冻结表。
		existing, err := lookupFreeze(ctx, tx, params.AfterSaleNo)
		if err != nil {
			return err
		}
		if existing {
			return nil
		}

		net, err := freezableAmount(ctx, tx, params.UserID, params.EntryKeys)
		if err != nil {
			return err
		}
		// 可用钳制：这单发的卡可能已经被抽掉一部分，冻不满是正常的——那正是审核时
		// 「福卡未参与抽奖」那个人工确认闸门在管的事。冻出个负数可用不是。
		amount := min(net, max(balance-frozen, 0))

		var freezeID string
		if err := tx.QueryRow(ctx, `INSERT INTO fortune_card_freezes
			(user_id, after_sale_no, order_id, order_no, entry_keys, amount, status, reason, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
			params.UserID, params.AfterSaleNo, params.OrderID, params.OrderNo, params.EntryKeys,
			amount, model.FreezeStatusFrozen, params.Reason, params.OccurredAt).Scan(&freezeID); err != nil {
			return mapPGError(err)
		}
		if amount == 0 {
			// 冻结行照建：申请早于发放时它就是个空壳，等发放落库时由 bindGrantToFreezes
			// 补进来。解冻时借它把「这一单申请过退款」这件事留在账上。
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE fortune_card_accounts
			SET frozen_balance = frozen_balance + $2, updated_at = NOW() WHERE user_id = $1`,
			params.UserID, amount); err != nil {
			return mapPGError(err)
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// freezableAmount 是这批发放键现在**还挂着**多少张：发放总额里没被冲正过的那些。
//
// 「只算还挂着的」那个条件今天真的会用上：退款成功后的追回会把发放冲掉，而重投一条旧的
// applied 事件（或者用户第二次申请退款）会碰到这种已经被冲正的行——一笔冲正过的发放没有
// 任何可冻的东西，按全额冻会让可用变成负数。
//
// RecoverAfterSale 也拿它算「追不回来多少张」：这个数比冻结额大出来的部分，正是申请退款
// 之前就被抽掉的卡。两处用的是同一句 SQL，对「这笔还挂着」的判断必须是同一个。
func freezableAmount(ctx context.Context, q rowQuerier, userID string, entryKeys []string) (int64, error) {
	var net int64
	err := q.QueryRow(ctx, `SELECT coalesce(sum(e.amount), 0)
		FROM fortune_card_entries e
		WHERE e.user_id = $1 AND e.entry_key = ANY($2) AND e.entry_type = $3
		  AND NOT EXISTS (SELECT 1 FROM fortune_card_entries r WHERE r.reverses_entry_id = e.id)`,
		userID, entryKeys, model.EntryTypeGrant).Scan(&net)
	if err != nil {
		return 0, mapPGError(err)
	}
	return net, nil
}

// rowQuerier 是「能查一行」的最小面：*pgxpool.Pool 与 pgx.Tx 都满足它。
//
// 拆出来只为一件事：freezableAmount 那句 SQL 要被**两条路**用——写的那条在事务里
// （FreezeAfterSale），只读的那条不开事务（PreviewFreezeAfterSale）。让两边各抄一遍
// SQL 的话，「还挂着多少张」这个判断就会有两个版本，而它正是冻结与预览必须一致的那个数。
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PreviewFreezeAfterSale 是「此刻冻这一批发放冻得上几张」：FreezeAfterSale 的只读版本。
//
// 两个返回值是**两个不同的事实**，调用方要分开看：
//
//	granted   这几笔发放还挂着多少张（没被冲正过的）。**0 表示发放还没落库**——一张订单
//	          可以先申请退款再完成（申请早于发放是设计支持的，那时冻结行是个空壳，等
//	          发放落库时由 bindGrantToFreezes 补上）。所以它不能读成「卡被用掉了」。
//	freezable 此刻真的冻得上的张数 = min(granted, 账户可用)。
//
// 少了 granted 就没法把「还没发」与「发过但已经抽光了」分开：两种情况下 freezable 都是 0，
// 而前者该放行、后者该拒。
func (r *PostgresRepository) PreviewFreezeAfterSale(ctx context.Context, params PreviewFreezeParams) (granted, freezable int64, err error) {
	// 键已经在 service 那一层清过空白（与 FreezeAfterSale 同一条规矩），这里只管空列表：
	// 这一单没承诺福卡、或者退的是不送福卡的会员套餐，两个数都是 0。
	keys := params.EntryKeys
	if len(keys) == 0 {
		return 0, 0, nil
	}

	// 账户行可能是懒创建之前的：没有行就是余额 0、可用 0，与「一张都没有」同一件事。
	var balance, frozen int64
	err = r.pool.QueryRow(ctx, `SELECT balance, frozen_balance FROM fortune_card_accounts
		WHERE user_id = $1`, params.UserID).Scan(&balance, &frozen)
	if errors.Is(err, pgx.ErrNoRows) {
		balance, frozen = 0, 0
	} else if err != nil {
		return 0, 0, mapPGError(err)
	}

	granted, err = freezableAmount(ctx, r.pool, params.UserID, keys)
	if err != nil {
		return 0, 0, err
	}
	return granted, min(granted, max(balance-frozen, 0)), nil
}

// lookupFreeze 说这张售后单是不是已经冻过了。
func lookupFreeze(ctx context.Context, tx pgx.Tx, afterSaleNo string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fortune_card_freezes WHERE after_sale_no = $1)`,
		afterSaleNo).Scan(&exists)
	if err != nil {
		return false, mapPGError(err)
	}
	return exists, nil
}

// ReleaseAfterSale 解冻：申请被驳回，或者用户撤销了。冻着的卡放回去。
//
// 找不到、或已经解冻过，都是 **no-op 而不是错误**。解冻的两条来路（驳回事件、撤销事件）
// 是各自独立投递的，顺序不定、也可能重投；把「没什么可解的」做成错误，会让一条正常的重投
// 卡在重试链上，一路重试到死信——而它要表达的事早就完成了。
//
// 不发流水：余额没变过，变的只是可用。amount 也**不清零**：审计要看当初冻了多少。
func (r *PostgresRepository) ReleaseAfterSale(ctx context.Context, params ReleaseParams) (bool, error) {
	var released bool
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// 不加锁地读一次，只为拿到 user_id：锁账户行需要它当参数。这一读的结果只用来决定
		// 「排到哪把锁上」，权威值在锁内重读一遍（user_id 不可变；会变的是下面那个状态）。
		var userID string
		err := tx.QueryRow(ctx, `SELECT user_id::text FROM fortune_card_freezes
			WHERE after_sale_no = $1`, params.AfterSaleNo).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapPGError(err)
		}

		// 账户行先锁再加回去：与发放/扣减/冻结抢的是同一把锁，frozen_balance 与冻结行
		// 才不会这两处各写各的。
		//
		// **先锁账户行、再锁冻结行，顺序不能反**——发放那条路正是这个顺序（applyEntry 先锁
		// 账户行，再由 bindGrantToFreezes 去锁冻结行）。反过来的话两条路成环：发放握着账户行
		// 等冻结行，解冻握着冻结行等账户行，PostgreSQL 只能挑一条报死锁，那条事件落一次失败
		// 重试。要撞上得有两个消费者同时处理同一个人的「发放」与「解冻」（多副本），单副本
		// 串行消费碰不到——但撞到的代价是一次谁也说不清的中断，而代价只在多发一条 SQL 上。
		var locked string
		if err := tx.QueryRow(ctx, `SELECT user_id::text FROM fortune_card_accounts
			WHERE user_id = $1 FOR UPDATE`, userID).Scan(&locked); err != nil {
			return mapPGError(err)
		}

		var (
			id     string
			amount int64
			status string
		)
		err = tx.QueryRow(ctx, `SELECT id::text, amount, status
			FROM fortune_card_freezes WHERE after_sale_no = $1 FOR UPDATE`,
			params.AfterSaleNo).Scan(&id, &amount, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			// 锁账户行之前它还在、之后没了。这一行没有删除方（解冻只是改状态），真走到这里
			// 说明世界变了——按同一条规矩当没事发生。
			return nil
		}
		if err != nil {
			return mapPGError(err)
		}
		if status == model.FreezeStatusReleased {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE fortune_card_freezes
			SET status = $2, reason = $3, released_at = $4, updated_at = NOW() WHERE id = $1`,
			id, model.FreezeStatusReleased, params.Reason, params.OccurredAt); err != nil {
			return mapPGError(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE fortune_card_accounts
			SET frozen_balance = frozen_balance - $2, updated_at = NOW() WHERE user_id = $1`,
			userID, amount); err != nil {
			return mapPGError(err)
		}
		released = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

// RecoverAfterSale 追回：钱退成了，这一单送出去的福卡收回来。
//
// 它是「退款成功」这一拍的收口，冻结行只有在这里才走到 recovered。与 ReleaseAfterSale 的
// 差别是一件事：解冻只把可用放回去（余额一分不动），追回还要**真的把余额扣掉**——所以它
// 写冲正流水，而解冻不写。
//
// 两条铁律，都写在这段代码的形状里：
//
//  1. **先解冻，再冲正。** 顺序反过来写的那一瞬间 frozen_balance > balance，
//     fortune_card_accounts_frozen_within_balance 那条 CHECK 当场炸（migrations/account/003
//     的注释就是为这一步留的）。
//  2. **部分冲正是常态。** 申请退款之前就已经被抽掉的卡追不回来——冻结时按可用余额钳过
//     （见 FreezeAfterSale），冻结额本来就小于发放额。所以冲正总额以冻结额为上限，逐笔
//     「有多少冲多少」，冲不满的那一笔只冲差额。整笔进整笔出会把余额打成负数。
//
// 幂等压在 status 上：不是 frozen 就直接返回，重投与乱序都安全。**不靠流水键兜底**是因为
// 一次追回要冲好几笔发放，那几把键各管各的，凑不出「这件事做过了」这一个判断。
//
// 返回的是实际追回的张数（可能小于冻结额，见上）。追不回来的那部分写进冻结行的 reason，
// 让它在页面上看得见——「钱退了、卡还剩几张在外面」是要有人知道的事。
func (r *PostgresRepository) RecoverAfterSale(ctx context.Context, params RecoverParams) (int64, error) {
	var recovered int64
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// 不加锁地读一次，只为拿到 user_id：锁账户行需要它当参数。与 ReleaseAfterSale 同一句，
		// 权威值在锁内重读一遍。
		var userID string
		err := tx.QueryRow(ctx, `SELECT user_id::text FROM fortune_card_freezes
			WHERE after_sale_no = $1`, params.AfterSaleNo).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			// 这一单没承诺过福卡（没有冻结行），追回无从谈起。不是错误。
			return nil
		}
		if err != nil {
			return mapPGError(err)
		}

		// 账户行先锁、冻结行后锁：与发放/扣减/冻结/解冻抢的是同一把锁，顺序也必须一样，
		// 反过来两条路成环（见 ReleaseAfterSale 那段注释）。
		var locked string
		if err := tx.QueryRow(ctx, `SELECT user_id::text FROM fortune_card_accounts
			WHERE user_id = $1 FOR UPDATE`, userID).Scan(&locked); err != nil {
			return mapPGError(err)
		}

		var (
			id        string
			amount    int64
			status    string
			orderNo   string
			entryKeys []string
		)
		err = tx.QueryRow(ctx, `SELECT id::text, amount, status, order_no, entry_keys
			FROM fortune_card_freezes WHERE after_sale_no = $1 FOR UPDATE`,
			params.AfterSaleNo).Scan(&id, &amount, &status, &orderNo, &entryKeys)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapPGError(err)
		}
		if status != model.FreezeStatusFrozen {
			// 已经解冻（驳回/撤销/退款失败）或者已经追回过。重投到这里结束。
			return nil
		}

		// 第一步：把冻结额放掉。
		if _, err := tx.Exec(ctx, `UPDATE fortune_card_accounts
			SET frozen_balance = frozen_balance - $2, updated_at = NOW() WHERE user_id = $1`,
			userID, amount); err != nil {
			return mapPGError(err)
		}

		// 第二步：冲正。预算就是冻结额，一笔一笔用到完。
		//
		// 动手之前先问一句「这些键下现在还挂着多少张」。它比冻结额大出来的那一部分，就是
		// 申请退款之前已经被抽掉的卡：冻结按可用余额钳过，它们从来没有被冻上，这一刀也就
		// 追不回来。这个数只用来写下面那句说明，不参与「怎么冲」——预算仍然是冻结额。
		live, err := freezableAmount(ctx, tx, userID, entryKeys)
		if err != nil {
			return err
		}
		budget := amount
		for _, key := range entryKeys {
			if budget <= 0 {
				break
			}
			taken, err := r.reverseGrantsUnderKey(ctx, tx, userID, key, orderNo, &budget, params)
			if err != nil {
				return err
			}
			recovered += taken
		}

		reason := params.Reason
		if short := live - recovered; short > 0 {
			// 追不回来的张数写进原因里，让它在页面上看得见——「钱退了、卡还有几张在外面」
			// 是客服要知道的事。
			//
			// **不报错**：钱是真的退回去了，为这个失败只会让这条事件一路重试到死信，而它
			// 要表达的事早就完成了。
			reason = fmt.Sprintf("%s（%d 张已参与抽奖，追不回来）", reason, short)
		}
		if _, err := tx.Exec(ctx, `UPDATE fortune_card_freezes
			SET status = $2, reason = $3, recovered_at = $4, updated_at = NOW() WHERE id = $1`,
			id, model.FreezeStatusRecovered, reason, params.OccurredAt); err != nil {
			return mapPGError(err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return recovered, nil
}

// reverseGrantsUnderKey 把一把发放键下还没被冲正的发放逐笔冲掉，直到预算用完。
//
// 键下可能有多笔（同一单命中两次加赠、或者订单域拆重了），所以是「逐笔」而不是「一笔」。
// 已经被冲正的发放不在此列：那把 `NOT EXISTS` 与 freezableAmount 用的是同一句，两处对
// 「这笔还挂着」的判断必须是同一个。
//
// 冲正的幂等键用 reverse:{entryId}，与 gRPC 的 Reverse 同一形状——同一笔发放冲两次会撞上
// 那把唯一索引，这里的循环也因此在多副本下是安全的。
func (r *PostgresRepository) reverseGrantsUnderKey(ctx context.Context, tx pgx.Tx, userID, entryKey, orderNo string, budget *int64, params RecoverParams) (int64, error) {
	rows, err := tx.Query(ctx, `SELECT e.id::text, e.amount
		FROM fortune_card_entries e
		WHERE e.user_id = $1 AND e.entry_key = $2 AND e.entry_type = $3
		  AND NOT EXISTS (SELECT 1 FROM fortune_card_entries r WHERE r.reverses_entry_id = e.id)
		ORDER BY e.occurred_at, e.id`, userID, entryKey, model.EntryTypeGrant)
	if err != nil {
		return 0, mapPGError(err)
	}
	type grant struct {
		id     string
		amount int64
	}
	var grants []grant
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.id, &g.amount); err != nil {
			rows.Close()
			return 0, mapPGError(err)
		}
		grants = append(grants, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, mapPGError(err)
	}

	var taken int64
	for _, g := range grants {
		if *budget <= 0 {
			break
		}
		// 有多少冲多少：这一笔可能比剩下的预算大（申请前被抽掉过几张），也可能小
		// （同一把键下有好几笔）。差额那一半留在账上，不属于这次退款要追回的部分。
		take := min(g.amount, *budget)
		if take <= 0 {
			continue
		}
		entryID := g.id
		if _, err := r.applyEntry(ctx, tx, entryParams{
			UserID:    userID,
			EntryType: model.EntryTypeReverse,
			Amount:    -take,
			Title:     params.Title,
			// 与 Reverse 同一套：冲正指向被冲的那笔流水，单号带着订单号，订单详情那个
			// 福卡页签按订单号取全——填错这一笔就不会出现在它冲掉的那笔旁边。
			ReferenceType:   model.ReferenceTypeEntry,
			ReferenceID:     entryID,
			ReferenceNo:     orderNo,
			EntryKey:        model.ReverseKey(entryID),
			ReversesEntryID: &entryID,
			Remark:          params.Remark,
			OccurredAt:      params.OccurredAt,
		}); err != nil {
			return 0, err
		}
		*budget -= take
		taken += take
	}
	return taken, nil
}

// freezeColumns 是 fortune_card_freezes 的读取列，顺序与 scanFreeze 严格一一对应。
const freezeColumns = `id::text, user_id::text, after_sale_no, order_id, order_no, entry_keys,
	amount, status, reason, occurred_at, released_at, recovered_at, created_at, updated_at`

func scanFreeze(row interface{ Scan(...any) error }) (*model.FortuneCardFreeze, error) {
	freeze := &model.FortuneCardFreeze{}
	if err := row.Scan(&freeze.ID, &freeze.UserID, &freeze.AfterSaleNo, &freeze.OrderID,
		&freeze.OrderNo, &freeze.EntryKeys, &freeze.Amount, &freeze.Status, &freeze.Reason,
		&freeze.OccurredAt, &freeze.ReleasedAt, &freeze.RecoveredAt, &freeze.CreatedAt,
		&freeze.UpdatedAt); err != nil {
		return nil, err
	}
	return freeze, nil
}

// ListFreezes 按过滤条件分页读冻结，返回 (当页, 总数)。
//
// 排序与 fortune_card_freezes_user_frozen_idx 一致（occurred_at DESC, id）：订单详情那个
// 页签与客服页都按时间看，同一毫秒的两笔也有稳定顺序。
func (r *PostgresRepository) ListFreezes(ctx context.Context, q dto.FreezeQuery) ([]*model.FortuneCardFreeze, int, error) {
	conds := make([]string, 0, 3)
	args := make([]any, 0, 3)
	add := func(clause string, value any) {
		args = append(args, value)
		conds = append(conds, fmt.Sprintf(clause, len(args)))
	}
	if q.OrderNo != "" {
		add("order_no = $%d", q.OrderNo)
	}
	if q.UserID != "" {
		add("user_id = $%d", q.UserID)
	}
	if q.Status != "" {
		add("status = $%d", q.Status)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM fortune_card_freezes`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), q.PageSize, api.PageOffset(q.Page, q.PageSize))
	rows, err := r.pool.Query(ctx, `SELECT `+freezeColumns+` FROM fortune_card_freezes`+where+
		fmt.Sprintf(` ORDER BY occurred_at DESC, id LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	freezes := make([]*model.FortuneCardFreeze, 0, q.PageSize)
	for rows.Next() {
		freeze, err := scanFreeze(rows)
		if err != nil {
			return nil, 0, err
		}
		freezes = append(freezes, freeze)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return freezes, total, nil
}

// Deduct 扣减福卡。
func (r *PostgresRepository) Deduct(ctx context.Context, params DeductParams) (EntryResult, error) {
	var result EntryResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		result, err = r.applyEntry(ctx, tx, entryParams{
			UserID:    params.UserID,
			EntryType: model.EntryTypeDraw,
			// 扣减在流水上是负数：调用方给的是「扣几张」，符号由这里定，不该由它关心。
			Amount:        -params.Amount,
			Title:         params.Title,
			ReferenceType: params.ReferenceType,
			ReferenceID:   params.ReferenceID,
			ReferenceNo:   params.ReferenceNo,
			EntryKey:      params.EntryKey,
			Remark:        params.Remark,
			OccurredAt:    params.OccurredAt,
		})
		return err
	})
	if err != nil {
		return EntryResult{}, err
	}
	return result, nil
}

// Reverse 冲正一笔流水：新增一条反向记录，不原地改数。
//
// 三条拒绝，各有各的话说：
//   - 原流水不存在（ErrEntryNotFound）；
//   - 原流水本身就是一条冲正（ErrNotReversible）——反向的反向就是原来的那笔，
//     那不该是一次新的冲正，而是「当时冲错了」，得由人来判断；
//   - 冲正后余额会变成负数（ErrReverseUncovered）——这笔发放已经被抽奖用掉了。
//
// 幂等由 entry_key 的 reverse:{entryId} 提供：同一笔只能冲一次，重放会撞上那把唯一索引
// 并回放已有那笔，所以这里不需要调用方再给一个请求号，也不需要另写一段「已经冲过了」
// 的判断——那段判断恰好会把正常的重放误判成冲突。
func (r *PostgresRepository) Reverse(ctx context.Context, params ReverseParams) (EntryResult, error) {
	var result EntryResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var (
			userID      string
			entryType   string
			amount      int64
			referenceNo string
		)
		if err := tx.QueryRow(ctx, `SELECT user_id::text, entry_type, amount, reference_no
			FROM fortune_card_entries WHERE id = $1`, params.EntryID).Scan(&userID, &entryType, &amount, &referenceNo); err != nil {
			return mapPGError(err)
		}
		if entryType == model.EntryTypeReverse {
			return ErrNotReversible
		}
		title := strings.TrimSpace(params.Title)
		if title == "" {
			title = "冲正"
		}
		reversed := params.EntryID
		var err error
		result, err = r.applyEntry(ctx, tx, entryParams{
			UserID:    userID,
			EntryType: model.EntryTypeReverse,
			// 与它冲掉的那笔相反：冲掉一条发放就是扣回来，冲掉一次扣减就是还回去。
			Amount:        -amount,
			Title:         title,
			ReferenceType: model.ReferenceTypeEntry,
			ReferenceID:   params.EntryID,
			// 带上原流水的订单号：订单详情那个「福卡」页签按订单号取全，
			// 少了这一步，冲正就不会出现在它冲掉的那笔旁边。
			ReferenceNo:     referenceNo,
			EntryKey:        model.ReverseKey(params.EntryID),
			ReversesEntryID: &reversed,
			Remark:          params.Remark,
			OccurredAt:      params.OccurredAt,
		})
		return err
	})
	if err != nil {
		return EntryResult{}, err
	}
	return result, nil
}

// entryColumns 是 fortune_card_entries 的读取列，顺序与 scanEntry 严格一一对应。
// UUID 列一律 ::text：pgx 把 uuid 扫进 string 需要这一步。
const entryColumns = `id::text, user_id::text, entry_type, amount, balance_after, title,
	reference_type, reference_id, reference_no, entry_key, reverses_entry_id::text, remark,
	occurred_at, created_at`

func scanEntry(row interface{ Scan(...any) error }) (*model.FortuneCardEntry, error) {
	entry := &model.FortuneCardEntry{}
	if err := row.Scan(&entry.ID, &entry.UserID, &entry.EntryType, &entry.Amount, &entry.BalanceAfter,
		&entry.Title, &entry.ReferenceType, &entry.ReferenceID, &entry.ReferenceNo, &entry.EntryKey,
		&entry.ReversesEntryID, &entry.Remark, &entry.OccurredAt, &entry.CreatedAt); err != nil {
		return nil, err
	}
	return entry, nil
}

// GetAccount 读一个用户的账户。没有账户行时返回 pgx.ErrNoRows——那是「还没收到过福卡」，
// 由服务层翻译成余额 0，不是「查不到这个人」。
func (r *PostgresRepository) GetAccount(ctx context.Context, userID string) (*model.FortuneCardAccount, error) {
	account := &model.FortuneCardAccount{}
	err := r.pool.QueryRow(ctx, `SELECT user_id::text, balance, frozen_balance, created_at, updated_at
		FROM fortune_card_accounts WHERE user_id = $1`, userID).
		Scan(&account.UserID, &account.Balance, &account.FrozenBalance, &account.CreatedAt, &account.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return account, nil
}

// ListEntries 按过滤条件分页读流水，返回 (当页, 总数)。
//
// 排序与 fortune_card_entries_user_idx 一致（occurred_at DESC, id）：同一毫秒的两笔
// 也有稳定顺序，翻页不会因为并列而跳着走。
func (r *PostgresRepository) ListEntries(ctx context.Context, q dto.EntryQuery) ([]*model.FortuneCardEntry, int, error) {
	conds := make([]string, 0, 5)
	args := make([]any, 0, 5)
	add := func(clause string, value any) {
		args = append(args, value)
		conds = append(conds, fmt.Sprintf(clause, len(args)))
	}
	if q.UserID != "" {
		add("user_id = $%d", q.UserID)
	}
	if q.OrderNo != "" {
		add("reference_no = $%d", q.OrderNo)
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
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM fortune_card_entries`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), q.PageSize, api.PageOffset(q.Page, q.PageSize))
	rows, err := r.pool.Query(ctx, `SELECT `+entryColumns+` FROM fortune_card_entries`+where+
		fmt.Sprintf(` ORDER BY occurred_at DESC, id LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	entries := make([]*model.FortuneCardEntry, 0, q.PageSize)
	for rows.Next() {
		entry, err := scanEntry(rows)
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
