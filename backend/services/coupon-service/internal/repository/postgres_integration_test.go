package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
)

// These tests intentionally use only a database URL supplied by the caller.
// They never create/drop a database or run migrations, and all fixture rows use
// unique IDs and are removed in dependency order after each test.
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("COUPON_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set COUPON_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("coupon test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("coupon test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func couponFixture(t *testing.T, pool *pgxpool.Pool, status string) (couponID, templateID, batchID, typeID string) {
	t.Helper()
	ctx := context.Background()
	typeID, templateID, batchID, couponID = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO coupon_types(id,code,name,status) VALUES($1,$2,$3,'active')`, typeID, "integration-"+typeID, "Integration")
	if err != nil {
		t.Fatalf("insert coupon type: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO coupon_templates(id,coupon_type_id,name,face_value,total_quantity,validity_mode,valid_from,valid_to,redemption_type,audit_status,audited_at,status) VALUES($1,$2,$3,1000,1,'fixed',NOW()-INTERVAL '1 minute',NOW()+INTERVAL '1 day','platform','approved',NOW(),'active')`, templateID, typeID, "Integration "+templateID)
	if err != nil {
		t.Fatalf("insert coupon template: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO coupon_batches(id,template_id,batch_no,source,total_quantity,issued_quantity,status) VALUES($1,$2,$3,'admin',1,1,'exhausted')`, batchID, templateID, "integration-"+batchID)
	if err != nil {
		t.Fatalf("insert coupon batch: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO user_coupons(id,template_id,batch_id,user_id,coupon_type_code,claim_type,status,face_value,redemption_type,valid_from,expired_at) VALUES($1,$2,$3,$4,$5,'admin_assign',$6,1000,'platform',NOW()-INTERVAL '1 minute',NOW()+INTERVAL '1 day')`, couponID, templateID, batchID, uuid.NewString(), "integration-"+typeID, status)
	if err != nil {
		t.Fatalf("insert user coupon: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// coupon_state_transitions 从 migrations/coupon/006 起带了只增触发器，直接
		// DELETE 会被那条 BEFORE UPDATE OR DELETE 当场拒掉。摘下来删完再装回去，
		// 与 membership-service 测试里对 membership_changes 的做法同一个意图：
		// 删的只是这个用例自己刚写进去的 fixture，不是把账本防线拆了。
		//
		// 这里比那边多一步——摘、删、装回放在**同一个事务**里。ALTER TABLE 的
		// DISABLE TRIGGER 是可回滚的，于是 DELETE 失败时事务一撤，触发器自动恢复，
		// 不会留下一个「触发器关着」的测试库。
		if tx, err := pool.Begin(ctx); err == nil {
			_, _ = tx.Exec(ctx, `ALTER TABLE coupon_state_transitions DISABLE TRIGGER coupon_state_transitions_append_only`)
			_, _ = tx.Exec(ctx, `DELETE FROM coupon_state_transitions WHERE aggregate_id=$1`, couponID)
			_, _ = tx.Exec(ctx, `ALTER TABLE coupon_state_transitions ENABLE TRIGGER coupon_state_transitions_append_only`)
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("cleanup coupon_state_transitions: %v", err)
			}
		} else {
			t.Errorf("cleanup coupon_state_transitions: begin: %v", err)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_redemptions WHERE user_coupon_id=$1`, couponID)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_idempotency_keys WHERE resource_id=$1 OR idempotency_key LIKE $2`, couponID, "integration-%")
		_, _ = pool.Exec(ctx, `DELETE FROM user_coupons WHERE id=$1`, couponID)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_batches WHERE id=$1`, batchID)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_templates WHERE id=$1`, templateID)
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_types WHERE id=$1`, typeID)
	})
	return
}

func TestPostgresRedeemPersistsStateAndReplaysIdempotently(t *testing.T) {
	pool := integrationPool(t)
	couponID, _, _, _ := couponFixture(t, pool, "claimed")
	// recorder 必须显式给：NewPostgresRepository 会把 nil 换成 audit.Noop，而这里
	// 直接构造结构体绕过了构造函数，nil 会在 Redeem/Revoke 记审计那一行空指针崩溃。
	// 用 Noop 而不是真的 Recorder：本用例验的是券的状态与流水，审计出口是身份库的事。
	repo := &postgresRepository{pool: pool, recorder: audit.Noop{}}
	ctx := context.Background()

	requestID := "integration-redeem-replay-" + uuid.NewString()
	actorID := uuid.NewString()
	first, err := repo.Redeem(ctx, couponID, requestID, actorID)
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if first.Status != "redeemed" || first.RedeemedAt == nil {
		t.Fatalf("first redeem = status %q, redeemed_at %v; want redeemed with timestamp", first.Status, first.RedeemedAt)
	}
	var redemptionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM coupon_redemptions WHERE user_coupon_id=$1 AND status='succeeded'`, couponID).Scan(&redemptionCount); err != nil {
		t.Fatal(err)
	}
	if redemptionCount != 1 {
		t.Fatalf("successful redemption rows = %d, want 1", redemptionCount)
	}
	// 核销这条流水必须留下操作人，与 Revoke 那条一样。这里之前是空的：controller
	// 把身份传给 service，service 校验完就丢了，后台翻这张券的时间线时只看得到
	// 「状态变了」，看不到是谁点的。
	var transitionActor *string
	if err := pool.QueryRow(ctx, `SELECT actor_id::text FROM coupon_state_transitions WHERE aggregate_id=$1 AND to_status='redeemed' AND request_id=$2`, couponID, requestID).Scan(&transitionActor); err != nil {
		t.Fatal(err)
	}
	if transitionActor == nil || *transitionActor != actorID {
		t.Fatalf("redeem transition actor = %v, want %q", transitionActor, actorID)
	}

	// 重放传一个**不同的** actor：走的是同一条幂等键，第一次已经写过了，重放不该再写
	// 一条流水，所以下面复查时 actor 必须还是第一次那个。
	second, err := repo.Redeem(ctx, couponID, requestID, uuid.NewString())
	if err != nil {
		t.Fatalf("replay redeem: %v", err)
	}
	if second.Status != first.Status || second.RedeemedAt == nil || !second.RedeemedAt.Equal(*first.RedeemedAt) {
		t.Fatalf("replayed redeem = %#v, want persisted response %#v", second, first)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM coupon_redemptions WHERE user_coupon_id=$1 AND request_id=$2`, couponID, requestID).Scan(&redemptionCount); err != nil {
		t.Fatal(err)
	}
	if redemptionCount != 1 {
		t.Fatalf("replayed redemption rows = %d, want 1", redemptionCount)
	}
	var transitionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM coupon_state_transitions WHERE aggregate_id=$1 AND to_status='redeemed' AND request_id=$2`, couponID, requestID).Scan(&transitionCount); err != nil {
		t.Fatal(err)
	}
	if transitionCount != 1 {
		t.Fatalf("replayed redeem transitions = %d, want 1", transitionCount)
	}
	if err := pool.QueryRow(ctx, `SELECT actor_id::text FROM coupon_state_transitions WHERE aggregate_id=$1 AND to_status='redeemed' AND request_id=$2`, couponID, requestID).Scan(&transitionActor); err != nil {
		t.Fatal(err)
	}
	if transitionActor == nil || *transitionActor != actorID {
		t.Fatalf("replayed redeem transition actor = %v, want 第一次的 %q", transitionActor, actorID)
	}
}

func TestPostgresRevokePersistsInvalidatedAtAndReplaysIdempotently(t *testing.T) {
	pool := integrationPool(t)
	couponID, _, _, _ := couponFixture(t, pool, "claimed")
	// recorder 必须显式给：NewPostgresRepository 会把 nil 换成 audit.Noop，而这里
	// 直接构造结构体绕过了构造函数，nil 会在 Redeem/Revoke 记审计那一行空指针崩溃。
	// 用 Noop 而不是真的 Recorder：本用例验的是券的状态与流水，审计出口是身份库的事。
	repo := &postgresRepository{pool: pool, recorder: audit.Noop{}}
	ctx := context.Background()
	requestID := "integration-revoke-" + uuid.NewString()

	first, err := repo.Revoke(ctx, couponID, requestID, uuid.NewString(), "integration test")
	if err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if first.Status != "invalidated" || first.InvalidatedAt == nil {
		t.Fatalf("first revoke = status %q, invalidated_at %v; want invalidated with timestamp", first.Status, first.InvalidatedAt)
	}
	var status string
	var persisted time.Time
	if err := pool.QueryRow(ctx, `SELECT status,invalidated_at FROM user_coupons WHERE id=$1`, couponID).Scan(&status, &persisted); err != nil {
		t.Fatal(err)
	}
	if status != "invalidated" || !persisted.Equal(*first.InvalidatedAt) {
		t.Fatalf("persisted revoke = (%q, %v), want (%q, %v)", status, persisted, status, *first.InvalidatedAt)
	}

	second, err := repo.Revoke(ctx, couponID, requestID, "ignored-on-replay", "ignored-on-replay")
	if err != nil {
		t.Fatalf("replay revoke: %v", err)
	}
	if second.Status != first.Status || second.InvalidatedAt == nil || !second.InvalidatedAt.Equal(*first.InvalidatedAt) {
		t.Fatalf("replayed revoke = %#v, want persisted response %#v", second, first)
	}
	var transitionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM coupon_state_transitions WHERE aggregate_id=$1 AND to_status='invalidated' AND request_id=$2`, couponID, requestID).Scan(&transitionCount); err != nil {
		t.Fatal(err)
	}
	if transitionCount != 1 {
		t.Fatalf("replayed revoke transitions = %d, want 1", transitionCount)
	}
}
