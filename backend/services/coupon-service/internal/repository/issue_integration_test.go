package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
)

// 这一组守的是发券那条路上的三件事：幂等键的并发、批次状态流水、模板窗口。
//
// 三条都**只在真库上才现形**：撞 UNIQUE 是数据库给的、流水与批次行是不是对得上要读库、
// 「窗口已经关了」要一个真模板。所以它们都在这里，不在 service 的假仓储那一侧。
//
// 不给 COUPON_DATABASE_URL / TEST_DATABASE_URL 时 integrationPool 会 t.Skip——全绿是假绿。

// issueFixture 造一份能发券的模板（券类型 + 模板），用例结束时按依赖顺序删干净。
//
// validity 两档：
//
//	"relative"  相对有效期 30 天。发券那条主路走它。
//	"closed"    fixed，且止期在昨天——窗口已经关了的那一格。
func issueFixture(t *testing.T, pool *pgxpool.Pool, validity string) (typeID, templateID string) {
	t.Helper()
	ctx := context.Background()
	typeID, templateID = uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO coupon_types(id,code,name,status) VALUES($1,$2,$3,'active')`, typeID, "integration-"+typeID, "Integration"); err != nil {
		t.Fatalf("insert coupon type: %v", err)
	}
	query := `INSERT INTO coupon_templates(id,coupon_type_id,name,face_value,total_quantity,validity_mode,valid_days,redemption_type,audit_status,audited_at,status) VALUES($1,$2,$3,1000,10,'relative',30,'platform','approved',NOW(),'active')`
	if validity == "closed" {
		query = `INSERT INTO coupon_templates(id,coupon_type_id,name,face_value,total_quantity,validity_mode,valid_from,valid_to,redemption_type,audit_status,audited_at,status) VALUES($1,$2,$3,1000,10,'fixed',NOW()-INTERVAL '10 days',NOW()-INTERVAL '1 day','platform','approved',NOW(),'active')`
	}
	if _, err := pool.Exec(ctx, query, templateID, typeID, "Integration "+templateID); err != nil {
		t.Fatalf("insert coupon template: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var batchIDs []string
		rows, err := pool.Query(ctx, `SELECT id::text FROM coupon_batches WHERE template_id=$1`, templateID)
		if err != nil {
			t.Errorf("cleanup: list batches: %v", err)
			return
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Errorf("cleanup: scan batch: %v", err)
				return
			}
			batchIDs = append(batchIDs, id)
		}
		rows.Close()
		// 两张账本表带只增触发器，直接 DELETE 会被当场拒掉。
		if err := deleteAppendOnlyRows(ctx, pool, "coupon_state_transitions", "aggregate_id", batchIDs); err != nil {
			t.Errorf("cleanup coupon_state_transitions: %v", err)
		}
		if err := deleteAppendOnlyRows(ctx, pool, "coupon_inventory_ledger", "template_id", []string{templateID}); err != nil {
			t.Errorf("cleanup coupon_inventory_ledger: %v", err)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM message_outbox WHERE trace_id LIKE 'integration-%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM user_coupon_scopes WHERE user_coupon_id IN (SELECT id FROM user_coupons WHERE template_id=$1)`, templateID)
		_, _ = pool.Exec(ctx, `DELETE FROM user_coupons WHERE template_id=$1`, templateID)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_idempotency_keys WHERE idempotency_key LIKE 'integration-%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_batches WHERE template_id=$1`, templateID)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_templates WHERE id=$1`, templateID)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_types WHERE id=$1`, typeID)
	})
	return typeID, templateID
}

// deleteAppendOnlyRows 摘触发器 → 删 → 装回，三步在**同一个事务**里。
//
// 分开做的话，DELETE 失败时事务已经撤了、触发器却留在「关着」的状态上——这台库接下来
// 跑的任何用例都不再有那道防线。ALTER TABLE 的 DISABLE 是可回滚的，放在一个事务里就没有
// 这个中间态（与 postgres_integration_test.go 里那段清理同一个意图）。
//
// 表名与触发器名是调用方写死的常量（这个文件里只有那两张账本），不是外部输入。
func deleteAppendOnlyRows(ctx context.Context, pool *pgxpool.Pool, table, column string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER %s_append_only`, table, table)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s::text = ANY($1::text[])`, table, column), ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE TRIGGER %s_append_only`, table, table)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func issueRepo(pool *pgxpool.Pool) *postgresRepository {
	// recorder 必须显式给：这里直接构造结构体绕过了构造函数，nil 会在记审计那一行空指针崩溃。
	// 用 Noop 而不是真 Recorder：审计出口是身份库的事，不在本用例的范围内。
	return &postgresRepository{pool: pool, recorder: audit.Noop{}}
}

// 同一个幂等键的并发请求：一次真发，其余全部回放同一个批次，没有一条报错。
//
// 这一格原来会随机变成 500：issue() 先 `SELECT … FOR UPDATE`，而**查不到就没有行可锁**——
// 几个请求双双走到 INSERT，后到的撞上 UNIQUE(scope,idempotency_key)，报 23505，调用方看到
// 的是「服务端故障」。service 层那次 Find 预检挡不住它：它们都在彼此写库之前查完了。
//
// 撞不撞是时序问题，所以这条用例不保证旧代码每次都红（真要每次红，看下面那条「还在处理中」
// 的用例，它是确定性的）；它守的是**并发下结果必须只有一个批次**这件事本身。
func TestPostgresIssueReplaysUnderConcurrentSameKey(t *testing.T) {
	pool := integrationPool(t)
	_, templateID := issueFixture(t, pool, "relative")
	repo := issueRepo(pool)
	ctx := context.Background()
	key := "integration-issue-race-" + uuid.NewString()
	req := dto.IssueCouponsRequest{
		TemplateID: templateID, UserIDs: []string{uuid.NewString()},
		QuantityPerUser: 3, Reason: "integration",
	}

	const callers = 8
	batches := make([]string, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, err := repo.IssueCoupons(ctx, key, "", "integration-hash", req)
			if err != nil {
				errs[i] = err
				return
			}
			batches[i] = out.BatchID
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发请求报错：%v（同一个幂等键的并发请求应当回放，不是报错）", i, err)
		}
		if batches[i] != batches[0] {
			t.Fatalf("第 %d 个并发请求拿到批次 %s，第一个拿到 %s；同一个幂等键只能有一个批次",
				i, batches[i], batches[0])
		}
	}
	var batchRows, coupons int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM coupon_batches WHERE template_id=$1),
		(SELECT count(*) FROM user_coupons WHERE template_id=$1)`, templateID).Scan(&batchRows, &coupons); err != nil {
		t.Fatal(err)
	}
	if batchRows != 1 || coupons != 3 {
		t.Fatalf("批次 %d / 券 %d，want 1/3（并发不会多发一张）", batchRows, coupons)
	}
}

// 幂等键那一行已经落库、但还停在 'processing' 时，**不能**把它当成成功响应回放。
//
// 这一格是确定性的：手工提交一行 processing（response 还没写，默认值是 '{}'），发券这边就会
// 撞上它。旧代码先 SELECT … FOR UPDATE，读到行就比对摘要、反序列化 response 返回——既不认
// status 也不认「响应还没写」，于是调用方拿到一个**静默的空批次**（BatchID 空、张数 0、
// 券 id 空）。那比 500 难查得多：接口是 200，券一张没发。
//
// 新代码认状态：还在处理中就是 409 IDEMPOTENCY_IN_PROGRESS（controller 那条分支），
// 卡住超过 15 分钟的旧行由 beginIdempotentOperation 回收，两条路都不会把空响应当答案。
func TestPostgresIssueDoesNotReplayAKeyThatIsStillProcessing(t *testing.T) {
	pool := integrationPool(t)
	_, templateID := issueFixture(t, pool, "relative")
	repo := issueRepo(pool)
	ctx := context.Background()
	key := "integration-issue-processing-" + uuid.NewString()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO coupon_idempotency_keys(scope,idempotency_key,request_hash,resource_type,status) VALUES('admin.coupons.issue',$1,'integration-hash','coupon_batch','processing')`, key); err != nil {
		t.Fatalf("hold the key: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := repo.IssueCoupons(ctx, key, "", "integration-hash", dto.IssueCouponsRequest{
			TemplateID: templateID, UserIDs: []string{uuid.NewString()}, QuantityPerUser: 1, Reason: "integration",
		})
		done <- err
	}()

	// ON CONFLICT DO NOTHING 会等那一行落定（不是绕过去自己插一行），所以这两秒里它不该返回。
	select {
	case err := <-done:
		t.Fatalf("发券在别人握着同一个幂等键时就返回了（err = %v）；它应当等到那一行落定再判", err)
	case <-time.After(2 * time.Second):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the key: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrIdempotencyInProgress) {
			t.Fatalf("发券 err = %v, want ErrIdempotencyInProgress", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("对方放手之后发券没有返回")
	}
	// 判成「还在处理中」不该留下任何东西：券、批次、库存流水都是 0。
	var batches, coupons, ledger int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM coupon_batches WHERE template_id=$1),
		(SELECT count(*) FROM user_coupons WHERE template_id=$1),
		(SELECT count(*) FROM coupon_inventory_ledger WHERE template_id=$1)`, templateID).Scan(&batches, &coupons, &ledger); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || coupons != 0 || ledger != 0 {
		t.Fatalf("批次 %d / 券 %d / 库存流水 %d，want 0/0/0", batches, coupons, ledger)
	}
}

// 批次行的状态与状态流水上的 to_status 必须是同一个值。
//
// 批次是一次发完的：建行时 issued_quantity 就等于 total_quantity，所以那一行生下来就是
// 'exhausted'，从来没有经历过 'active'。流水里原来写的正是 'active'——一次没发生过的迁移。
// 按这张只增不改的表重建批次历史的人，会得到一份与 coupon_batches 对不上的账。
func TestPostgresIssueRecordsTheBatchStateItActuallyWrote(t *testing.T) {
	pool := integrationPool(t)
	_, templateID := issueFixture(t, pool, "relative")
	repo := issueRepo(pool)
	ctx := context.Background()
	key := "integration-issue-state-" + uuid.NewString()
	req := dto.IssueCouponsRequest{
		TemplateID: templateID, UserIDs: []string{uuid.NewString(), uuid.NewString()},
		QuantityPerUser: 2, Reason: "integration",
	}

	first, err := repo.IssueCoupons(ctx, key, "", "integration-hash", req)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if first.IssuedQuantity != 4 {
		t.Fatalf("issued = %d, want 4", first.IssuedQuantity)
	}

	var batchStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM coupon_batches WHERE id=$1`, first.BatchID).Scan(&batchStatus); err != nil {
		t.Fatal(err)
	}
	if batchStatus != "exhausted" {
		t.Fatalf("批次状态 = %q，want exhausted（一次发完的批次不会先经过 active）", batchStatus)
	}
	var fromStatus, toStatus string
	if err := pool.QueryRow(ctx, `SELECT from_status,to_status FROM coupon_state_transitions WHERE aggregate_type='batch' AND aggregate_id=$1`, first.BatchID).Scan(&fromStatus, &toStatus); err != nil {
		t.Fatal(err)
	}
	if fromStatus != "" || toStatus != batchStatus {
		t.Fatalf("状态流水记的是 %q → %q，而批次行是 %q；两者必须是同一个值", fromStatus, toStatus, batchStatus)
	}

	// 重放：同一把键、同一份请求体，回到同一个批次，且不再写第二行流水、第二张券。
	second, err := repo.IssueCoupons(ctx, key, "", "integration-hash", req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.BatchID != first.BatchID || second.IssuedQuantity != first.IssuedQuantity {
		t.Fatalf("重放得到 %+v，want 与第一次同一个批次 %+v", second, first)
	}
	var batches, coupons, transitions, ledger int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM coupon_batches WHERE template_id=$1),
		(SELECT count(*) FROM user_coupons WHERE template_id=$1),
		(SELECT count(*) FROM coupon_state_transitions WHERE aggregate_id=$2),
		(SELECT count(*) FROM coupon_inventory_ledger WHERE template_id=$1)`, templateID, first.BatchID).
		Scan(&batches, &coupons, &transitions, &ledger); err != nil {
		t.Fatal(err)
	}
	if batches != 1 || coupons != 4 || transitions != 1 || ledger != 1 {
		t.Fatalf("批次 %d / 券 %d / 状态流水 %d / 库存流水 %d，want 1/4/1/1", batches, coupons, transitions, ledger)
	}
}

// 窗口已经关了的模板不能再发券。
//
// 它发出去的不是「暂时用不了」：fixed 档的每一张券都抄模板这一个窗口，窗口在发之前就关掉的
// 话，券一落地就是过期的——用户账上多一张永远用不了的券，模板的发行量与库存流水却照扣，
// 中间没有任何一步会报错。这是最坏的那一类错：静默、不可逆。
func TestPostgresIssueRefusesTemplateWhoseWindowHasClosed(t *testing.T) {
	pool := integrationPool(t)
	_, templateID := issueFixture(t, pool, "closed")
	repo := issueRepo(pool)
	ctx := context.Background()
	key := "integration-issue-closed-" + uuid.NewString()

	_, err := repo.IssueCoupons(ctx, key, "", "integration-hash", dto.IssueCouponsRequest{
		TemplateID: templateID, UserIDs: []string{uuid.NewString()}, QuantityPerUser: 1, Reason: "integration",
	})
	if !errors.Is(err, ErrTemplateUnavailable) {
		t.Fatalf("err = %v, want ErrTemplateUnavailable（controller 据此回 409，不是 500）", err)
	}

	// 什么都没写出去：券、批次、库存流水、模板的发行量，以及那把幂等键——失败的一次发券
	// 不该把键占掉，否则同一个 key 重试（比如运营先把窗口改对）会被判成冲突。
	var batches, coupons, keys int
	var issued int64
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM coupon_batches WHERE template_id=$1),
		(SELECT count(*) FROM user_coupons WHERE template_id=$1),
		(SELECT count(*) FROM coupon_idempotency_keys WHERE idempotency_key=$2),
		(SELECT issued_quantity FROM coupon_templates WHERE id=$1)`, templateID, key).
		Scan(&batches, &coupons, &keys, &issued); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || coupons != 0 || keys != 0 || issued != 0 {
		t.Fatalf("批次 %d / 券 %d / 幂等键 %d / 模板已发行 %d，want 0/0/0/0", batches, coupons, keys, issued)
	}
}
