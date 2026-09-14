package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
		_, _ = pool.Exec(ctx, `DELETE FROM coupon_state_transitions WHERE aggregate_id=$1`, couponID)
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
	repo := &postgresRepository{pool: pool}
	ctx := context.Background()

	requestID := "integration-redeem-replay-" + uuid.NewString()
	first, err := repo.Redeem(ctx, couponID, requestID)
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

	second, err := repo.Redeem(ctx, couponID, requestID)
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
}

func TestPostgresRevokePersistsInvalidatedAtAndReplaysIdempotently(t *testing.T) {
	pool := integrationPool(t)
	couponID, _, _, _ := couponFixture(t, pool, "claimed")
	repo := &postgresRepository{pool: pool}
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
