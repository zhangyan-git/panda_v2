package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// TestFindWechatIdentityByUser 覆盖「按 user_id 正查 openid」这条新查询。
//
// 它与登录链路那条反查（FindWechatIdentity，按 app_type+openid）方向相反，
// 而支付链路要的正是这一条：发起微信支付时手里只有 user_id，问的是「用哪个
// openid 去发起」。方向写反的代价不是查不到，而是**查到了别人**——所以夹具里
// 刻意放两个用户、两条 miniapp 身份。
func TestFindWechatIdentityByUser(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repo := NewUserRepository(pool)
	suffix := uuid.NewString()[:8]

	// 时间戳写死而不是 NOW()：下面那条「同应用两行时取先绑的那条」需要一个确定的次序，
	// 同一批插入的 NOW() 会撞在一起。
	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	newUser := func(tag string) *model.User {
		return &model.User{
			ID: uuid.NewString(), Nickname: "it-openid-" + tag + "-" + suffix,
			Gender: "unknown", Status: "active", RegisterSource: "miniapp",
			CreatedAt: created, UpdatedAt: created,
		}
	}
	newIdentity := func(userID, openid, unionid string) *model.UserWechatIdentity {
		return &model.UserWechatIdentity{
			ID: uuid.NewString(), UserID: userID, AppType: model.WechatAppMiniapp,
			OpenID: openid, UnionID: unionid, CreatedAt: created, UpdatedAt: created,
		}
	}

	// openid 必须全局唯一（库里的唯一键是 (app_type, openid)，跨用户也唯一），
	// 所以带上这次运行的后缀，重复跑不会互撞。
	target := newUser("target")
	other := newUser("other")
	targetOpenID := "it-openid-" + suffix + "-target"
	otherOpenID := "it-openid-" + suffix + "-other"

	if err := repo.RegisterWithWechat(ctx, target, newIdentity(target.ID, targetOpenID, "it-unionid-"+suffix)); err != nil {
		t.Fatalf("建 target 账号失败: %v", err)
	}
	if err := repo.RegisterWithWechat(ctx, other, newIdentity(other.ID, otherOpenID, "")); err != nil {
		t.Fatalf("建 other 账号失败: %v", err)
	}
	t.Cleanup(func() {
		// user_wechat_identities 对 users 是 ON DELETE CASCADE，删账号就带走了身份行。
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM users WHERE id = ANY($1::uuid[])`,
			[]string{target.ID, other.ID}); err != nil {
			t.Logf("清理测试账号失败: %v", err)
		}
	})

	ident, err := repo.FindWechatIdentityByUser(ctx, target.ID, model.WechatAppMiniapp)
	if err != nil {
		t.Fatalf("FindWechatIdentityByUser: %v", err)
	}
	if ident.OpenID != targetOpenID {
		t.Fatalf("openid = %q; 期望 %q（查到了别人的身份说明 user_id 没进 WHERE）", ident.OpenID, targetOpenID)
	}
	if ident.UserID != target.ID {
		t.Fatalf("user_id = %q; 期望 %q", ident.UserID, target.ID)
	}
	if ident.UnionID != "it-unionid-"+suffix {
		t.Fatalf("unionid = %q; 期望 %q", ident.UnionID, "it-unionid-"+suffix)
	}

	// 另一个应用下没有绑定。这条同时守住了 app_type 确实进了 WHERE：少了它，
	// 上面那次查询会退化成「这个人随便哪条身份」。
	if _, err := repo.FindWechatIdentityByUser(ctx, target.ID, model.WechatAppOfficialAccount); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("公众号身份查询 err = %v; 期望 pgx.ErrNoRows", err)
	}
	// 没有绑定过的用户同样回 ErrNoRows，而不是零值身份——调用方要靠它区分
	// 「没绑微信」和「查不到这个人」。
	if _, err := repo.FindWechatIdentityByUser(ctx, uuid.NewString(), model.WechatAppMiniapp); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("无绑定用户查询 err = %v; 期望 pgx.ErrNoRows", err)
	}
}

// TestFindWechatIdentityByUserPicksTheEarliestBinding：库里只有 UNIQUE (app_type, openid)，
// 同一个用户在同一应用下可以留下两行（换绑过 openid 就会）。
//
// 没有 ORDER BY 的 LIMIT 1 拿到的是任意一行：同一个用户两次调用可能拿到两个不同的
// openid，而渠道报文里换了 openid 就是换了一个付款人——重试会失败在支付侧那条
// 「幂等键被换过请求体」上，或者更糟，让 A 的支付发起成 B 的。
func TestFindWechatIdentityByUserPicksTheEarliestBinding(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repo := NewUserRepository(pool)
	suffix := uuid.NewString()[:8]
	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)

	user := &model.User{
		ID: uuid.NewString(), Nickname: "it-openid-order-" + suffix,
		Gender: "unknown", Status: "active", RegisterSource: "miniapp",
		CreatedAt: created, UpdatedAt: created,
	}
	first := &model.UserWechatIdentity{
		ID: uuid.NewString(), UserID: user.ID, AppType: model.WechatAppMiniapp,
		OpenID: "it-openid-" + suffix + "-first", CreatedAt: created, UpdatedAt: created,
	}
	if err := repo.RegisterWithWechat(ctx, user, first); err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	// 补绑一条：created_at 更晚。BindWechatIdentity 是唯一允许同用户多行的入口。
	second := &model.UserWechatIdentity{
		ID: uuid.NewString(), UserID: user.ID, AppType: model.WechatAppMiniapp,
		OpenID:    "it-openid-" + suffix + "-second",
		CreatedAt: created.Add(time.Minute), UpdatedAt: created.Add(time.Minute),
	}
	if err := repo.BindWechatIdentity(ctx, second); err != nil {
		t.Fatalf("补绑第二条身份失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM users WHERE id = $1::uuid`, user.ID); err != nil {
			t.Logf("清理测试账号失败: %v", err)
		}
	})

	ident, err := repo.FindWechatIdentityByUser(ctx, user.ID, model.WechatAppMiniapp)
	if err != nil {
		t.Fatalf("FindWechatIdentityByUser: %v", err)
	}
	if ident.OpenID != first.OpenID {
		t.Fatalf("openid = %q; 期望先绑的那条 %q", ident.OpenID, first.OpenID)
	}
	// 两次调用必须是同一个答案：随机取一行会让支付重试变成另一次发起。
	again, err := repo.FindWechatIdentityByUser(ctx, user.ID, model.WechatAppMiniapp)
	if err != nil {
		t.Fatalf("第二次 FindWechatIdentityByUser: %v", err)
	}
	if again.OpenID != ident.OpenID {
		t.Fatalf("两次查询结果不一致: %q / %q", ident.OpenID, again.OpenID)
	}
}
