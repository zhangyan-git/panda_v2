package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Transactor 提供一个跨 repository 的事务边界。
//
// 一个业务动作常常要写两张表：实体（门店/品牌）与它的待审核记录。各 repository 的
// 自有方法各自 Begin/Commit，中间没有任何共同边界——进程死在两次提交之间，库里就留下
// 一个「写进去了、却永远不进待审列表」的实体，页面上看不出异常。InTx 把这几步收进同一次
// 提交：fn 返回 nil 才 Commit，返回任何错误都整体回滚。
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error
}

type pgTransactor struct {
	pool *pgxpool.Pool
}

func NewTransactor(pool *pgxpool.Pool) Transactor {
	return &pgTransactor{pool: pool}
}

func (t *pgTransactor) InTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
