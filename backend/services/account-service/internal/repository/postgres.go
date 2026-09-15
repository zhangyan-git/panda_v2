// Package repository 是资产账户库的数据访问层。
//
// 两个域各占一份文件：fortune_card.go（福卡：发放、扣减、冲正、冻结、解冻）与
// coffee_bean.go（咖啡豆：扣减、退款冲正、账户与流水读取）。两边的写路径都是同一个形状
// ——「锁住账户行 → 算余额 → 追加一笔流水 → 更新余额」，整体一个事务；读路径在各文件的
// 下半部分。两张表结构同形不是巧合：它们是同一个服务拥有的同一类东西。
//
// 人工余额调整单独一个仓储（admin_bean.go）：它比别的写路径多一个审计依赖，而读路径不该
// 为了一个用不到的 recorder 多传一个参数。
//
// 这里没有给**业务事件**用的 appendOutbox：福卡的发放与豆的余额变化都不发下游事件（今天
// 没有消费者，见 cmd/main.go 的说明）。唯一的平台事件是审计（admin.operation.logged），
// 它由 platform/audit 在同事务内追加到 message_outbox——那要求 cmd/main.go 给 runtime 配上
// Outbox，否则写进去的审计只会躺在库里等人投。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrInsufficientFortuneCards：余额不够扣。这是本服务**认识**的业务结果，不是故障
	// ——调用方（抽奖）应当把它翻译成给用户看的一句话。
	ErrInsufficientFortuneCards = errors.New("fortune card balance is insufficient")
	// ErrReverseUncovered：冲正会把余额扣成负数，即这笔发放已经被抽奖用掉了。
	// 「已经抽过奖的福卡追不回来」是业务规则，同样不是故障。
	ErrReverseUncovered = errors.New("reversing this entry would drive the balance negative")
	// ErrEntryKeyConflict：幂等键撞上了别人的流水。正常重放不会走到这里（同一个键
	// 必然属于同一个用户），撞上说明调用方自己把号发重了，必须有人看。
	ErrEntryKeyConflict = errors.New("entry key belongs to another account")
	// ErrEntryNotFound：要冲正的那笔流水不存在。
	ErrEntryNotFound = errors.New("fortune card entry not found")
	// ErrNotReversible：冲正一笔冲正。反向的反向就是原来的，那不是冲正的语义。
	ErrNotReversible = errors.New("a reversal entry cannot be reversed")
)

// PostgresRepository 是资产账户库的数据访问实现。
type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// inTx 跑一个事务，出错即回滚。写路径必须整体成功或整体不动：流水追加了而余额没改，
// 或反过来，都会让「SUM(amount) = balance」这条不变式当场失效。
func (r *PostgresRepository) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// 提交后这次 Rollback 是无操作（返回 ErrTxClosed），所以不必判断返回值。
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// mapPGError 把约束冲突翻成调用方分得清的业务错误。
//
// 约束名是这里唯一稳定可依赖的东西：靠错误文本匹配会在 PG 换语言或换措辞时静默失效。
// 余额那条 CHECK 是「不会被扣穿」的最后一道防线——正常路径在锁里已经先判过一次，
// 走到这里说明有人绕开了那条判断。
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEntryNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "fortune_card_entries_key_unique":
			return ErrEntryKeyConflict
		case "fortune_card_accounts_balance_check", "fortune_card_entries_balance_after_check":
			return ErrInsufficientFortuneCards
		}
	}
	return err
}
