package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/partnerkey"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/repository"
)

// 这个文件盯的是「明文签名密钥只在一处产生、一处出现、一处入库」这条链路：
//
//	partnerkey.Issue 产生明文 → keyring.Seal 变成密文 → 明文跟着返回值到响应体
//	                                              ↘ 密文进 CreateAPIKey 的参数
//
// 每一条断言都对着这条链上的一个具体事故：
//   - 明文被原样写进 APIKeyWrite.Secret（直连 INSERT 那一列，psql 里就能读到）；
//   - 掩码算错（列表里显示的掩码与运营手上那把对不上，而掩码是唯一线索）；
//   - 读路径把密文解出来回给前端（等于把「只有签名密钥能签名」这件事作废）；
//   - 签发在一次输入错误之后仍然落了一行（运营没拿到明文，只能靠再签一把收拾）。

const testMasterKey = "0123456789abcdef0123456789abcdef"

// fakeRepository 是治理层要的那几个方法的桩：写方法把它们收到的参数原样记下来，读方法回
// 事先放好的行。它只实现测试用得到的那几个——接口是完整的，所以多出来的方法一并补空实现。
type fakeRepository struct {
	writes  []repository.APIKeyWrite
	updates []repository.APIKeyWrite
	keys    []*model.APIKey

	createErr error
	updateErr error
	listErr   error

	created   []repository.PartnerWrite
	partners  []*repository.PartnerListRow
	callLogs  []*repository.CallLogRow
	callTotal int
}

func (f *fakeRepository) ListPartners(context.Context, dto.PartnerQuery) ([]*repository.PartnerListRow, int, error) {
	return f.partners, len(f.partners), nil
}

func (f *fakeRepository) FindPartner(context.Context, string) (*model.PartnerAccount, error) {
	return nil, repository.ErrPartnerNotFound
}

func (f *fakeRepository) CreatePartner(_ context.Context, in repository.PartnerWrite, _ string) (*model.PartnerAccount, error) {
	f.created = append(f.created, in)
	return &model.PartnerAccount{ID: "p-1", Code: in.Code}, nil
}

func (f *fakeRepository) UpdatePartner(context.Context, string, repository.PartnerWrite) (*model.PartnerAccount, error) {
	return nil, repository.ErrPartnerNotFound
}

func (f *fakeRepository) SetPartnerStatus(context.Context, string, string) (*model.PartnerAccount, error) {
	return nil, repository.ErrPartnerNotFound
}

func (f *fakeRepository) ListAPIKeys(context.Context, string) ([]*model.APIKey, error) {
	return f.keys, f.listErr
}

func (f *fakeRepository) CreateAPIKey(_ context.Context, _ string, in repository.APIKeyWrite, _ string) (*model.APIKey, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.writes = append(f.writes, in)
	return &model.APIKey{
		ID: "k-1", PartnerID: "p-1", APIKey: in.APIKey, Secret: in.Secret,
		APIKeyMask: in.APIKeyMask, SecretMask: in.SecretMask, Name: in.Name,
		ExpiresAt: in.ExpiresAt, IPWhitelist: in.IPWhitelist,
		RateLimitPerMinute: in.RateLimitPerMinute, Status: model.StatusEnabled,
	}, nil
}

func (f *fakeRepository) UpdateAPIKey(_ context.Context, _, _ string, in repository.APIKeyWrite) (*model.APIKey, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	f.updates = append(f.updates, in)
	return &model.APIKey{ID: "k-1", PartnerID: "p-1", Name: in.Name, SecretMask: in.SecretMask}, nil
}

func (f *fakeRepository) SetAPIKeyStatus(context.Context, string, string, string) (*model.APIKey, error) {
	return nil, repository.ErrAPIKeyNotFound
}

func (f *fakeRepository) ListCallLogs(context.Context, dto.CallLogQuery) ([]*repository.CallLogRow, int, error) {
	return f.callLogs, f.callTotal, nil
}

func newTestKeyring(t *testing.T) *secret.Keyring {
	t.Helper()
	keyring, err := secret.New([]byte(testMasterKey))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	return keyring
}

// ============================================================
// 签发：明文去哪、密文去哪
// ============================================================

// TestIssueAPIKeyStoresCiphertextAndReturnsPlaintextOnce 是本文件的核心：它把「库里那一列
// 不是明文」从一个设计承诺变成一个被检查过的事实。
//
// 库里那一列的值就是 CreateAPIKey 收到的 APIKeyWrite.Secret。所以这里断言三件事：
//  1. 它不是明文（psql 里 select 出来的就是它，一个只读数据库的人也读不到能签名的东西）；
//  2. 用同一把 Keyring 能解回明文（封对了，验签时解得开——这是「不是明文」不能替代的一半）；
//  3. 明文确实随返回值出去了**一次**（运营要拿得到，否则这把密钥没人能用）。
func TestIssueAPIKeyStoresCiphertextAndReturnsPlaintextOnce(t *testing.T) {
	repo := &fakeRepository{}
	keyring := newTestKeyring(t)
	svc := NewAdminService(repo, keyring)

	issued, err := svc.IssueAPIKey(context.Background(), "p-1", dto.APIKeyInput{}, "operator-1")
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if len(repo.writes) != 1 {
		t.Fatalf("应当只写一次，写了 %d 次", len(repo.writes))
	}
	write := repo.writes[0]

	// 1. 进库的不是明文。
	if write.Secret.Ciphertext == "" {
		t.Fatal("进库的必须是密文信封，得到空值")
	}
	if strings.Contains(write.Secret.Ciphertext, issued.Secret) || write.Secret.Ciphertext == issued.Secret {
		t.Fatal("进库的是签名密钥的明文——那一列在 psql 里谁都读得到")
	}

	// 2. 解得回来（封的时候槽名与 kind 都对）。
	opened, err := keyring.Open(model.SecretSlot, write.Secret)
	if err != nil {
		t.Fatalf("密文应当能用同一把主密钥解开: %v", err)
	}
	if opened != issued.Secret {
		t.Fatalf("解出来的明文与签发时的那一串不一致")
	}

	// 3. 明文只随返回值出现，且它的掩码与它自己对得上。
	if issued.Secret == "" {
		t.Fatal("签发必须把明文带回一次（否则这把密钥没人能用）")
	}
	if issued.Key.Secret != write.Secret {
		t.Fatal("返回值里的密钥行应当是刚才写进去的那一份")
	}
	if issued.Key.SecretMask == issued.Secret {
		t.Fatal("掩码不能等于明文")
	}
	if !strings.HasPrefix(issued.Key.SecretMask, issued.Secret[:4]) || !strings.HasSuffix(issued.Key.SecretMask, issued.Secret[len(issued.Secret)-4:]) {
		t.Fatalf("掩码应当是「前 4 + … + 后 4」，得到 %q", issued.Key.SecretMask)
	}
	// 掩码里不含中间那一段（否则列表页把密钥泄漏了大半）。
	if strings.Contains(issued.Key.SecretMask, issued.Secret[8:24]) {
		t.Fatalf("掩码里出现了密钥的中段: %q", issued.Key.SecretMask)
	}
	// api_key 与 secret 是两次独立取随机数：它们不该相等（相等说明有人把它们派生成了一个）。
	if issued.Key.APIKey == issued.Secret {
		t.Fatal("api_key 与 secret 不能是同一个值")
	}
	if issued.Key.APIKey != write.APIKey {
		t.Fatal("返回值里的 api_key 应当与写进去的一致")
	}
}

// TestIssueAPIKeyNeverFallsBackToPlaintextWithoutAKeyring 钉住「封不起来就不签」。
//
// 没有主密钥时**不能**退化成明文入库：那一列是 NOT NULL，写明文等于把能签名的东西放进一张
// psql 里谁都读得到的表。所以失败，并且**库里不留任何一行**——不存在「密钥行在了、而运营没
// 拿到明文」这种只能靠重签一把来收拾的中间态（见 IssueAPIKey 的顺序注释）。
func TestIssueAPIKeyNeverFallsBackToPlaintextWithoutAKeyring(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewAdminService(repo, nil) // 主密钥缺失

	issued, err := svc.IssueAPIKey(context.Background(), "p-1", dto.APIKeyInput{}, "operator-1")
	if !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("应当是 ErrSecretUnavailable，得到 %v", err)
	}
	if issued != nil {
		t.Fatal("失败时不该有返回值")
	}
	if len(repo.writes) != 0 {
		t.Fatalf("失败时库里不该留下任何一行，写了 %d 次", len(repo.writes))
	}
}

// TestIssueAPIKeyRejectsBadInputBeforeWriting 钉住「校验在落库之前」。
//
// 一次「白名单里写错一个字母」的请求不该在库里留下任何痕迹——而这条路径的失败点全部在
// CreateAPIKey 之前，所以 repo.writes 必须是空的。
func TestIssueAPIKeyRejectsBadInputBeforeWriting(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewAdminService(repo, newTestKeyring(t))

	cases := []struct {
		name string
		in   dto.APIKeyInput
		want error
	}{
		{"白名单里有一条解释不了", dto.APIKeyInput{IPWhitelist: []string{"10.0.0.0/8", "oops"}}, ErrIPWhitelistInvalid},
		{"额度是负数", dto.APIKeyInput{RateLimitPerMinute: -1}, ErrRateLimitInvalid},
		{"有效期不是 RFC3339", dto.APIKeyInput{ExpiresAt: ptr("2026-13-45")}, ErrExpiresAtInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.IssueAPIKey(context.Background(), "p-1", tc.in, "operator-1"); !errors.Is(err, tc.want) {
				t.Fatalf("应当是 %v，得到 %v", tc.want, err)
			}
			if len(repo.writes) != 0 {
				t.Fatalf("校验失败时不该写库，写了 %d 次", len(repo.writes))
			}
		})
	}

	// 没有操作人：审计里 created_by 为空的行意味着「不知道是谁签发的这把密钥」。
	if _, err := svc.IssueAPIKey(context.Background(), "p-1", dto.APIKeyInput{}, "  "); !errors.Is(err, ErrOperatorRequired) {
		t.Fatalf("应当是 ErrOperatorRequired，得到 %v", err)
	}
}

// TestIssueAPIKeyNormalizesTheRateLimit 钉住「0 是没填、负数才是错」。
//
// 0 原样传到库里会当场撞 CHECK (> 0)——一条「额度没填」的 500。所以它归一成默认值。默认值
// 只有一处定义（model.DefaultRateLimitPerMinute），这条测试同时盯着「别处又抄了一个 60」。
func TestIssueAPIKeyNormalizesTheRateLimit(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewAdminService(repo, newTestKeyring(t))

	if _, err := svc.IssueAPIKey(context.Background(), "p-1", dto.APIKeyInput{}, "op"); err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if got := repo.writes[0].RateLimitPerMinute; got != model.DefaultRateLimitPerMinute {
		t.Fatalf("没填额度时应当归一成默认值 %d，得到 %d", model.DefaultRateLimitPerMinute, got)
	}

	// 填了就用填的（不能顺手把大客户的额度也压回默认值）。
	if _, err := svc.IssueAPIKey(context.Background(), "p-1", dto.APIKeyInput{RateLimitPerMinute: 5000}, "op"); err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}
	if got := repo.writes[1].RateLimitPerMinute; got != 5000 {
		t.Fatalf("应当用调用方填的额度 5000，得到 %d", got)
	}
}

// TestUpdateAPIKeyCannotTouchTheCredential 钉住「改访问控制这条路碰不到密钥本身」。
//
// 它不是靠一句注释：dto.APIKeyInput 里根本没有 secret 字段，而这里再确认一次写结构里那两列
// 是零值——一个「顺手把旧密钥带上」的改动会让 UPDATE 把密文写回成空。
func TestUpdateAPIKeyCannotTouchTheCredential(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewAdminService(repo, newTestKeyring(t))

	if _, err := svc.UpdateAPIKey(context.Background(), "p-1", "k-1", dto.APIKeyInput{Name: "生产主用", RateLimitPerMinute: 120}); err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	if len(repo.updates) != 1 {
		t.Fatalf("应当写一次，写了 %d 次", len(repo.updates))
	}
	if len(repo.writes) != 0 {
		t.Fatal("改访问控制不该走签发的写路径")
	}
	write := repo.updates[0]
	if write.APIKey != "" || write.APIKeyMask != "" || write.SecretMask != "" || write.Secret.Ciphertext != "" {
		t.Fatalf("改访问控制时不该带任何凭据列: %+v", write)
	}
	if write.Name != "生产主用" || write.RateLimitPerMinute != 120 {
		t.Fatalf("该改的列没改到: %+v", write)
	}
}

// TestListAPIKeysReturnsOnlyMasks 钉住读路径「一次解密都不做」。
//
// 读方法直接把仓储的行交出去（不做映射也不解密），所以这条路能泄漏的只有仓储交出来的东西。
// 这里放一行**带密文信封**的密钥进去，断言列表回来的行里没有任何一个字段等于明文，而掩码在。
func TestListAPIKeysReturnsOnlyMasks(t *testing.T) {
	keyring := newTestKeyring(t)
	plaintext := "0123456789abcdef0123456789abcdef"
	envelope, err := keyring.Seal(model.SecretSlot, secret.KindText, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	mask, err := partnerkey.Mask(plaintext)
	if err != nil {
		t.Fatalf("Mask: %v", err)
	}

	repo := &fakeRepository{keys: []*model.APIKey{{
		ID: "k-1", PartnerID: "p-1", APIKey: "KEY-1", APIKeyMask: "KEY-…EY-1",
		Secret: envelope, SecretMask: mask, Status: model.StatusEnabled,
	}}}
	svc := NewAdminService(repo, keyring)

	keys, err := svc.ListAPIKeys(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("应当回来一行，得到 %d 行", len(keys))
	}
	row := keys[0]
	if row.SecretMask != mask {
		t.Fatalf("列表里应当带掩码（运营靠它辨认是哪一把），得到 %q", row.SecretMask)
	}
	// 逐个字段找明文：model.APIKey 上没有任何一个字段叫明文密钥，而这条断言盯的是「将来有
	// 人加了一个」——加字段的人必须顺手让这条测试变红，而不是让明文静默地跟着结构体走。
	for name, value := range map[string]string{
		"ID": row.ID, "PartnerID": row.PartnerID, "APIKey": row.APIKey, "APIKeyMask": row.APIKeyMask,
		"SecretMask": row.SecretMask, "Name": row.Name, "Status": string(row.Status),
		"CreatedBy": row.CreatedBy,
	} {
		if strings.Contains(value, plaintext) {
			t.Fatalf("读路径的第 %s 个字段里出现了明文签名密钥", name)
		}
	}
	// 密文的 nonce 也不该出现在任何字符串字段里（它同样只在库里）。
	if strings.Contains(row.APIKeyMask+row.SecretMask+row.Name, envelope.Nonce) {
		t.Fatal("读路径泄漏了密文信封的一部分")
	}
}

// TestParseKeyAccessKeepsTheWhitelistVerbatim 钉住「白名单存的是调用方填的原串」。
//
// 回显成归一化后的形状（10.0.0.1 → 10.0.0.1/32）会让运营看到与自己填的不一样的东西，而那个
// 差别的解释成本全在他那边。校验仍然走读那一侧的解析器（写读不一致会让一条存得进去的白名单
// 每次调用都 401）。
func TestParseKeyAccessKeepsTheWhitelistVerbatim(t *testing.T) {
	access, err := parseKeyAccess(dto.APIKeyInput{
		Name:               "  生产环境 主用  ",
		IPWhitelist:        []string{"203.0.113.7", "10.0.0.0/8"},
		RateLimitPerMinute: 60,
		ExpiresAt:          ptr(time.Now().UTC().Add(time.Hour).Format(time.RFC3339)),
	})
	if err != nil {
		t.Fatalf("parseKeyAccess: %v", err)
	}
	if len(access.IPWhitelist) != 2 || access.IPWhitelist[0] != "203.0.113.7" {
		t.Fatalf("白名单应当原样保留，得到 %v", access.IPWhitelist)
	}
	if access.Name != "生产环境 主用" {
		t.Fatalf("备注只该去两端空白（中间的空格是正常写法），得到 %q", access.Name)
	}
}

// TestParseExpiresAtNormalizesToUTC 钉住时区归一：库里的列是 timestamptz，但读回来的展示
// 依赖连接时区，归一在写入口就省掉了读路径的猜。
func TestParseExpiresAtNormalizesToUTC(t *testing.T) {
	value := "2026-01-02T03:04:05+08:00"
	parsed, err := parseExpiresAt(&value)
	if err != nil {
		t.Fatalf("parseExpiresAt: %v", err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("应当归一成 UTC，得到 %v", parsed.Location())
	}
	if want := "2026-01-01T19:04:05Z"; parsed.Format(time.RFC3339) != want {
		t.Fatalf("时刻换算错了：%s，期望 %s", parsed.Format(time.RFC3339), want)
	}

	// 空串与 nil 都表示「不过期」，不是错误（不是「立刻过期」）。
	for _, empty := range []*string{nil, ptr(""), ptr("   ")} {
		got, err := parseExpiresAt(empty)
		if err != nil || got != nil {
			t.Fatalf("%v 应当归一成 nil（不过期），得到 %v / %v", empty, got, err)
		}
	}

	// 过去的时间**不是**错误：那是一个明确的「立刻失效」的手势（比停用更轻，因为到点之后
	// 还能改回来）。
	past := "2020-01-01T00:00:00Z"
	if _, err := parseExpiresAt(&past); err != nil {
		t.Fatalf("过去的时间应当被接受: %v", err)
	}
}

func ptr(value string) *string { return &value }
