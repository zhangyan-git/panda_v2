package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/partnerkey"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/service"
)

// 这个文件盯的是 HTTP 边界上两件 service 层的测试看不见的事。
//
// # 一、明文签名密钥只出一次、只出一条路
//
// service 层的测试证明了「进库的是密文」（apikey_test.go）。那还不够：库里是密文，不代表**响应
// 里**是密文——把明文塞回任何一个读接口是一行代码的事，而且它不会让任何一条已有的测试变红。
// 所以这里从 HTTP 那一侧再验一遍，断言是分层的：
//
//  1. 签发响应里明文出现**恰好一次**（不是两次：第二次多半意味着有人把它又填进了掩码或
//     apiKey 字段，那样它就会跟着列表页、日志、缓存到处走）；
//  2. 此后每一个读接口的响应体里都**没有那个值**，而且密钥列表的每一项上**根本没有 secret
//     这个键**（结构上不存在，不是「这次恰好是空的」）；
//  3. 库里那一列也不是明文（同一个请求，两条独立的路）。
//
// # 二、给用户看的中文句子漏一条会红
//
// admin_partner.go 的错误对照表上写着「漏一条测试会红」——就是下面第二个测试。它按 service
// 的哨兵错误逐个调 writeAdminPartnerError，把 (状态码, 错误码, 文案) 与期望对照。表里少了
// 一条时，那一条会掉进兜底：状态码变 500、文案变「服务暂时不可用」，于是这个测试红在
// 「一次没填名字的提交告诉用户『服务暂时不可用』」这句话上。

// ============================================================
// 桩
// ============================================================

// fakeGovernanceRepo 是 service.PartnerRepository 的桩：写方法把收到的参数原样记下来，读方法
// 回事先放好的行。它按**真的会落库的那份值**构造返回值（密文信封原样带回），这样读路径上
// 能泄漏的东西就是仓储交出来的东西——与本服务的真实装配一致。
type fakeGovernanceRepo struct {
	writes []repository.APIKeyWrite
	keys   []*model.APIKey

	partners []*repository.PartnerListRow
	callLogs []*repository.CallLogRow
}

func (f *fakeGovernanceRepo) ListPartners(context.Context, dto.PartnerQuery) ([]*repository.PartnerListRow, int, error) {
	return f.partners, len(f.partners), nil
}

func (f *fakeGovernanceRepo) FindPartner(context.Context, string) (*model.PartnerAccount, error) {
	return &model.PartnerAccount{ID: "p-1", Code: "fengxuan", Name: "风选"}, nil
}

func (f *fakeGovernanceRepo) CreatePartner(context.Context, repository.PartnerWrite, string) (*model.PartnerAccount, error) {
	return &model.PartnerAccount{ID: "p-1"}, nil
}

func (f *fakeGovernanceRepo) UpdatePartner(context.Context, string, repository.PartnerWrite) (*model.PartnerAccount, error) {
	return &model.PartnerAccount{ID: "p-1"}, nil
}

func (f *fakeGovernanceRepo) SetPartnerStatus(context.Context, string, string) (*model.PartnerAccount, error) {
	return &model.PartnerAccount{ID: "p-1"}, nil
}

func (f *fakeGovernanceRepo) ListAPIKeys(context.Context, string) ([]*model.APIKey, error) {
	return f.keys, nil
}

func (f *fakeGovernanceRepo) CreateAPIKey(_ context.Context, partnerID string, in repository.APIKeyWrite, _ string) (*model.APIKey, error) {
	f.writes = append(f.writes, in)
	key := &model.APIKey{
		ID: "k-1", PartnerID: partnerID, APIKey: in.APIKey, Secret: in.Secret,
		APIKeyMask: in.APIKeyMask, SecretMask: in.SecretMask, Name: in.Name,
		Status: model.StatusEnabled, ExpiresAt: in.ExpiresAt,
		IPWhitelist: in.IPWhitelist, RateLimitPerMinute: in.RateLimitPerMinute,
	}
	// 读接口接的就是这一行（库里只有这一份），所以列表页拿到的与签发时落库的是同一个对象。
	f.keys = append(f.keys, key)
	return key, nil
}

func (f *fakeGovernanceRepo) UpdateAPIKey(context.Context, string, string, repository.APIKeyWrite) (*model.APIKey, error) {
	return &model.APIKey{ID: "k-1"}, nil
}

func (f *fakeGovernanceRepo) SetAPIKeyStatus(context.Context, string, string, string) (*model.APIKey, error) {
	return &model.APIKey{ID: "k-1"}, nil
}

func (f *fakeGovernanceRepo) ListCallLogs(context.Context, dto.CallLogQuery) ([]*repository.CallLogRow, int, error) {
	return f.callLogs, len(f.callLogs), nil
}

// testKeyring 造一把测试用的主密钥。与 apikey_test.go 里的那一把是同一个值（32 字节），
// 但没有共用的可能——两个包各自持有一份，谁也不 import 谁的 _test.go。
func testKeyring(t *testing.T) *secret.Keyring {
	t.Helper()
	keyring, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	return keyring
}

// newGovernanceController 装配一棵不经过路由的控制器。
//
// 身份是**直接放进上下文**的（auth.WithIdentity），不是伪造一个令牌：路由那层（auth.Middleware
// → adminOnly → authz → RequirePermission）是 platform/auth 与 routes 的事，在这里再验一遍
// 只是把别人测过的东西抄一遍。这里要的是「handler 在拿到一个合法身份时把什么写回响应」。
func newGovernanceController(t *testing.T, repo *fakeGovernanceRepo) (*AdminPartnerController, *secret.Keyring) {
	t.Helper()
	keyring := testKeyring(t)
	return NewAdminPartnerController(service.NewAdminService(repo, keyring)), keyring
}

// adminRequest 造一个带后台身份、带 JSON body 的请求。
//
// path 直接写完整路径：控制器的分发是按 r.URL.Path 手写裁剪的（见 admin_partner.go 的
// Partners），所以这里不需要 gorilla/mux 也能走到目标分支——而那正是「路径解析」这件事唯一
// 值得测的形态。
func adminRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.WithIdentity(request.Context(), auth.Identity{
		UserID: "operator-1", Subject: "operator-1", Realm: auth.RealmPlatform,
	}))
	return request
}

// partnerPath 拼一条以合作方 id 开头的后台路径。
func partnerPath(partnerID, suffix string) string {
	return adminPartnersPath + "/" + partnerID + suffix
}

// ============================================================
// 一、明文只出现一次，读接口一个都没有
// ============================================================

// TestTheSigningSecretLeavesExactlyOnceAndNeverComesBack 是本文件的核心。
//
// 它是一份**跨四个请求**的断言，因为「明文只出一次」这句话本身就是关于时间顺序的：签发那一刻
// 出去，此后每一次读都拿不到。把四个请求写在一个测试里是有意的——分成四个测试的话，每一个都
// 只看到自己那一段，而「有人给列表加了一个 secret 字段」只会让第四个变红，看起来像一条孤立的
// 失败。
func TestTheSigningSecretLeavesExactlyOnceAndNeverComesBack(t *testing.T) {
	repo := &fakeGovernanceRepo{}
	ctrl, keyring := newGovernanceController(t, repo)
	partnerID := uuid.NewString()

	// ——1. 签发：这一份响应里必须有明文，而且只出现一次 ——
	issue := httptest.NewRecorder()
	ctrl.Partners(issue, adminRequest(http.MethodPost, partnerPath(partnerID, "/keys"), `{"name":"生产主用","rateLimitPerMinute":120}`))
	if issue.Code != http.StatusCreated {
		t.Fatalf("签发应当是 201，得到 %d，body=%s", issue.Code, issue.Body.String())
	}
	// 解进信封里的 data：响应体是 {"success":true,"data":{…}}（platform/api 的 Success/Created），
	// 直接解成 dto.APIKeyIssued 会得到一堆零值——而那正是「看着像空响应」的假象。
	var issueEnvelope struct {
		Data dto.APIKeyIssued `json:"data"`
	}
	if err := json.Unmarshal(issue.Body.Bytes(), &issueEnvelope); err != nil {
		t.Fatalf("签发响应解不出来: %v，body=%s", err, issue.Body.String())
	}
	issued := issueEnvelope.Data
	if issued.Secret == "" || issued.APIKey == "" {
		t.Fatalf("签发响应里应当同时给出 apiKey 与 secret，得到 %+v", issued.APIKeyItem)
	}
	if got := strings.Count(issue.Body.String(), issued.Secret); got != 1 {
		t.Fatalf("明文签名密钥在签发响应里出现了 %d 次，应当恰好 1 次（第二次多半是被填进了掩码或别的字段）", got)
	}
	// 掩码是「前 4 + … + 后 4」，与列表页显示的必须是同一个值。
	if want, err := partnerkey.Mask(issued.Secret); err != nil || issued.SecretMask != want {
		t.Fatalf("签发响应里的掩码与密钥对不上：%q / %v", issued.SecretMask, err)
	}

	// ——2. 库里那一列不是明文（与上面是两条独立的路：一条看响应，一条看写参数）——
	if len(repo.writes) != 1 {
		t.Fatalf("签发应当只写一次，写了 %d 次", len(repo.writes))
	}
	if strings.Contains(repo.writes[0].Secret.Ciphertext, issued.Secret) {
		t.Fatal("落库的 Secret 是明文——那一列在 psql 里谁都读得到")
	}
	if opened, err := keyring.Open(model.SecretSlot, repo.writes[0].Secret); err != nil || opened != issued.Secret {
		t.Fatalf("落库的密文应当能解回刚签发的明文: %v", err)
	}

	// ——3. 三个读接口：一个都不许把明文带回来 ——
	reads := []struct {
		name   string
		method string
		path   string
	}{
		{"密钥列表", http.MethodGet, partnerPath(partnerID, "/keys")},
		{"合作方详情", http.MethodGet, partnerPath(partnerID, "")},
		{"调用日志", http.MethodGet, adminCallLogsPath},
	}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if read.path == adminCallLogsPath {
				ctrl.CallLogs(rec, adminRequest(read.method, read.path, ""))
			} else {
				ctrl.Partners(rec, adminRequest(read.method, read.path, ""))
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("%s 应当是 200，得到 %d，body=%s", read.name, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), issued.Secret) {
				t.Fatalf("%s 的响应体里出现了明文签名密钥", read.name)
			}
			if strings.Contains(rec.Body.String(), repo.writes[0].Secret.Ciphertext) {
				t.Fatalf("%s 的响应体里出现了密文信封（它同样只在库里）", read.name)
			}
		})
	}

	// ——4. 密钥列表那一行上**没有 secret 这个键**（结构上不存在，不是「这次是空的」）——
	//
	// 这一条与上面那句「响应体里没有那个值」挡的不是同一件事：明文换成另一把密钥的值、或者
	// 将来有人加了一个「重置密钥」的读接口，句子级的断言都看不出来，而「这个响应里根本没有
	// 装着密钥的字段」看得出来。
	rec := httptest.NewRecorder()
	ctrl.Partners(rec, adminRequest(http.MethodGet, partnerPath(partnerID, "/keys"), ""))
	var envelope struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("密钥列表解不出来: %v", err)
	}
	if len(envelope.Data.Items) != 1 {
		t.Fatalf("列表里应当有一行，得到 %d 行", len(envelope.Data.Items))
	}
	for key := range envelope.Data.Items[0] {
		if strings.Contains(strings.ToLower(key), "secret") && key != "secretMask" {
			t.Fatalf("密钥列表的这一行上出现了 %q 字段——读路径上不该有任何装密钥本身的字段", key)
		}
	}
	if got, _ := envelope.Data.Items[0]["secretMask"].(string); got == "" {
		t.Fatal("列表里应当带掩码（运营靠它辨认是哪一把）")
	}
}

// TestReadsRejectNonUUIDPathSegmentsWithoutTouchingTheDatabase 钉住「路径形状不对在进 SQL 之前
// 就挡下来」。
//
// 这一条的生产意义见 admin_partner.go 的 partnerID：不是 uuid 的串会让 PostgreSQL 在参数编码
// 时报 22P02，于是「复制粘贴少了一截的 id」以 500 的面目出现。断言里带上仓储的写次数（一次
// 都没有）是为了让「先校验后落库」这件事有证据，而不是只看到一句 400。
func TestReadsRejectNonUUIDPathSegmentsWithoutTouchingTheDatabase(t *testing.T) {
	repo := &fakeGovernanceRepo{}
	ctrl, _ := newGovernanceController(t, repo)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		// 少一截的 id：合作方那一层就不是 uuid。
		{"合作方 id 少一截", http.MethodGet, partnerPath("0f8fad5b", "/keys"), ""},
		// 合作方 id 对、密钥那一层不是 uuid。用 PATCH 是因为这个分支先判方法（POST 会得到
		// 404，那挡的是「方法不对」，与这段要测的「id 形状不对」是两件事）。
		{"密钥 id 不是 uuid", http.MethodPatch, partnerPath(uuid.NewString(), "/keys/not-a-uuid/status"), `{"status":"disabled"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctrl.Partners(rec, adminRequest(tc.method, tc.path, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应当是 400，得到 %d，body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				ErrorCode string `json:"errorCode"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.ErrorCode != "INVALID_ARGUMENT" {
				t.Fatalf("应当是 INVALID_ARGUMENT，得到 %q", body.ErrorCode)
			}
			if len(repo.writes) != 0 {
				t.Fatal("路径不合法时不该走到仓储")
			}
		})
	}
}

// ============================================================
// 二、错误对照表是完整的
// ============================================================

// TestEveryGovernanceErrorHasAChineseSentence 逐个错误值验一遍对照表。
//
// 表驱动（对照表就在 admin_partner.go 里）的代价是「新加一条校验忘了加一行」，表现是用户看到
// 一句「服务暂时不可用，请稍后重试」的 500——而真相只是名字没填。这个测试把「漏一条」的定义
// 写死在这里：兜底响应是 msgGeneric + 500，凡是期望不是它的，都必须真的有一条自己的中文句子。
//
// 期望值里**没有** 500：这些错误全部是「这次请求/这份输入的问题」，一条都不该是 5xx。真出现
// 5xx 的那两条（ErrSecretUnavailable / ErrOperatorRequired，都是我们这边的装配或配置故障）单独
// 在下面那个测试里点出来。
func TestEveryGovernanceErrorHasAChineseSentence(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		msg    string
	}{
		{service.ErrCodeInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgCodeInvalid},
		{service.ErrNameRequired, http.StatusBadRequest, "INVALID_ARGUMENT", msgNameRequired},
		{service.ErrExpiresAtInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgExpiresAtInvalid},
		{service.ErrStatusInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgStatusInvalid},
		{service.ErrRateLimitInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgRateLimitInvalid},
		{service.ErrIPWhitelistInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgIPWhitelistInvalid},
		{service.ErrErrorCodeInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgErrorCodeInvalid},
		{service.ErrStatusCodeInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgStatusCodeInvalid},
		{repository.ErrPartnerNotFound, http.StatusNotFound, api.CodeNotFound, msgPartnerNotFound},
		{repository.ErrAPIKeyNotFound, http.StatusNotFound, api.CodeNotFound, msgKeyNotFound},
		{repository.ErrPartnerCodeTaken, http.StatusConflict, api.CodeConflict, msgPartnerCodeTaken},
		{repository.ErrAPIKeyTaken, http.StatusConflict, api.CodeConflict, msgKeyTaken},
		{repository.ErrPartnerCodeImmutable, http.StatusBadRequest, "INVALID_ARGUMENT", msgCodeImmutable},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			status, code, msg := respondTo(t, tc.err)
			if status != tc.status || code != tc.code || msg != tc.msg {
				t.Fatalf("对照表里这条不对：得到 (%d, %s, %q)，期望 (%d, %s, %q)",
					status, code, msg, tc.status, tc.code, tc.msg)
			}
		})
	}

	// 每一条文案都必须是中文（一眼能看出漏了翻译的只有「这句是英文」）。不检查「有没有中文
	// 句号」之类的形状：那是文风，不是漏配。
	for _, entry := range adminPartnerMessages {
		if entry.msg == msgGeneric {
			t.Fatalf("%v 掉进了兜底文案——它需要一个能指着具体字段说话的中文句子", entry.err)
		}
		if !strings.ContainsFunc(entry.msg, func(r rune) bool { return r >= 0x4E00 && r <= 0x9FFF }) {
			t.Fatalf("%v 的文案不是中文: %q", entry.err, entry.msg)
		}
	}
}

// TestOurOwnFailuresAreFiveHundredsNotSilentFourHundreds 钉住那两条「不是调用方的问题」的错误。
//
// ErrSecretUnavailable 是主密钥缺失或不对：回 400 会把我们这边的配置故障说成「你填错了」，
// 而看到那句的人会去改这次提交的内容。ErrOperatorRequired 走到这里意味着路由少挂了一层身份
// ——那是一个装配错误，回 401 会让运营反复重新登录。两条都必须是 500 且**进日志**（那句
// slog 在 writeAdminPartnerError 里，是这个测试唯一看不见的一半，所以写在这里提醒）。
func TestOurOwnFailuresAreFiveHundredsNotSilentFourHundreds(t *testing.T) {
	for _, err := range []error{service.ErrSecretUnavailable, service.ErrOperatorRequired} {
		status, code, msg := respondTo(t, err)
		if status != http.StatusInternalServerError || code != api.CodeInternal {
			t.Fatalf("%v 应当是 500/%s，得到 %d/%s", err, api.CodeInternal, status, code)
		}
		if msg == "" {
			t.Fatalf("%v 的响应里没有给用户看的句子", err)
		}
	}
}

// TestAnUnmappedErrorFallsBackToAGenericFiveHundred 钉住兜底那一支。
//
// 兜底是必须存在的（否则一个没人映射过的错误会变成一次 panic 或一个空 body），而它的代价是
// 「忘了映射」会静默地长成 500——所以它必须以 500 出现，且不能把内部错误直接发给调用方。
func TestAnUnmappedErrorFallsBackToAGenericFiveHundred(t *testing.T) {
	status, code, msg := respondTo(t, io.ErrUnexpectedEOF)
	if status != http.StatusInternalServerError || code != api.CodeInternal {
		t.Fatalf("兜底应当是 500/%s，得到 %d/%s", api.CodeInternal, status, code)
	}
	if msg != msgGeneric {
		t.Fatalf("兜底的话应当是 %q，得到 %q", msgGeneric, msg)
	}
	if strings.Contains(msg, "unexpected EOF") {
		t.Fatal("兜底把内部错误原样发给了调用方")
	}
}

// respondTo 调一次 writeAdminPartnerError，把三个响应要素取回来。
func respondTo(t *testing.T, err error) (status int, code, message string) {
	t.Helper()
	rec := httptest.NewRecorder()
	writeAdminPartnerError(rec, adminRequest(http.MethodGet, adminPartnersPath, ""), err, "test")
	var body struct {
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if decodeErr := json.Unmarshal(rec.Body.Bytes(), &body); decodeErr != nil {
		t.Fatalf("错误响应解不出来: %v", decodeErr)
	}
	return rec.Code, body.ErrorCode, body.ErrorMessage
}
