package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

// 这个文件驱动的是真路由表：中间件链、mux 的路径变量、控制器都是生产上挂的那几个。
// 假掉的只有两处——问 user-service 要数据范围的那次 RPC（换成一个返回固定答案的函数），
// 以及最后那个仓储（记下交给查询的过滤器）。
//
// 于是这里能看见一件在别处看不见的事：一个商户 token 走到 SQL 之前，边界变成了什么。

const merchantDeviceID = "2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c"

// fakeDeviceRepo 记下交给查询的过滤器。商户域的设备列表没有门店字段，所以
// seenFilter.StoreIDs 是范围唯一能留下的痕迹。
type fakeDeviceRepo struct {
	repository.MasterDataRepository
	devices []*model.Device
	byID    *model.Device
	err     error

	seenFilter repository.DeviceFilter
	listCalls  int
}

func (f *fakeDeviceRepo) ListDevices(_ context.Context, filter repository.DeviceFilter) ([]*model.Device, int64, error) {
	f.listCalls++
	f.seenFilter = filter
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.devices, int64(len(f.devices)), nil
}

func (f *fakeDeviceRepo) GetDevice(_ context.Context, _ string) (*model.Device, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.byID == nil {
		// 与真仓储同一条出口：查不到设备就该是 404。
		return nil, repository.ErrDeviceNotFound
	}
	return f.byID, nil
}

// merchantAPI 把一次请求要经过的东西装在一起：令牌、假的范围解析、真路由表。
type merchantAPI struct {
	server *khttp.Server
	jwt    *auth.Service
	repo   *fakeDeviceRepo

	// grants 与 resolveErr 是那个假 user-service 的答案，测试中途可以改——用来验
	// 「同一枚令牌的下一次请求就收窄」。
	grants     authz.MerchantGrants
	resolveErr error
	calls      int
}

func newMerchantAPI(t *testing.T, repo *fakeDeviceRepo, grants authz.MerchantGrants) *merchantAPI {
	t.Helper()
	api := &merchantAPI{repo: repo, jwt: testJWT(t), grants: grants}
	resolve := func(context.Context, string) (authz.MerchantGrants, error) {
		api.calls++
		if api.resolveErr != nil {
			return authz.MerchantGrants{}, api.resolveErr
		}
		return api.grants, nil
	}
	// 与 cmd/main.go 的 merchantAuthorizer 逐字一致：认证 → 实时取数据范围。
	// 那两行写在 main 里，这里重写一遍，是为了让中间件顺序本身也受测。
	authorize := func(next http.Handler) http.Handler {
		return auth.Middleware(api.jwt)(authz.MerchantMiddleware(resolve, time.Second)(next))
	}
	server := khttp.NewServer()
	RegisterMerchant(runtime.NewHTTPRouter(server),
		controller.NewMerchantDeviceController(service.NewMerchantDeviceService(repo)), authorize)
	api.server = server
	return api
}

// token 签一枚商户 access token。
func (a *merchantAPI) token(t *testing.T, grant auth.Grant) string {
	t.Helper()
	if grant.Realm == "" {
		grant.Realm = auth.RealmMerchant
	}
	return testToken(t, a.jwt, grant)
}

// do 用一枚已经签好的令牌发一个请求。
func (a *merchantAPI) do(t *testing.T, token, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	a.server.ServeHTTP(w, r)
	return w
}

// call 现签一枚令牌再发一个请求。
func (a *merchantAPI) call(t *testing.T, grant auth.Grant, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return a.do(t, a.token(t, grant), method, path)
}

// testJWT 造一个只服务于这个文件的签名器。密钥长度是 NewService 自己要求的下限，
// 值与生产无关——这些令牌只在进程内签、进程内验。
func testJWT(t *testing.T) *auth.Service {
	t.Helper()
	svc, err := auth.NewService([]byte(strings.Repeat("test", 8)), "test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// testToken 签一枚 access token。SignGrant 拒绝签一枚不说自己属于哪一方的令牌，所以
// 没写 realm 的夹具在这里默认成平台管理员——这个文件里每一条要用商户身份的用例都会
// 显式写出 realm，剩下的那些正是想验「非商户令牌进不来」。
func testToken(t *testing.T, svc *auth.Service, grant auth.Grant) string {
	t.Helper()
	if grant.Realm == "" {
		grant.Realm = auth.RealmPlatform
	}
	token, err := svc.SignAccessGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

// merchantGrant 是一个默认合法的商户账号：realm 对，tenant 与 user 都是商户 m1 的。
func merchantGrant() auth.Grant {
	return auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"}
}

func TestMerchantDeviceListIsBoundToTheResolvedScope(t *testing.T) {
	store := "s1"
	repo := &fakeDeviceRepo{devices: []*model.Device{{ID: merchantDeviceID, StoreID: &store}}}
	api := newMerchantAPI(t, repo, authz.MerchantGrants{
		MerchantID: "m1", ScopeType: auth.ScopeTypeBrand, ScopeID: "b1", StoreIDs: []string{"s1", "s2"},
	})

	w := api.call(t, merchantGrant(), http.MethodGet, "/v1/merchant/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(repo.seenFilter.StoreIDs, []string{"s1", "s2"}) {
		t.Fatalf("范围过滤=%v，want [s1 s2]", repo.seenFilter.StoreIDs)
	}
}

// 查询串里带门店那一条必须完全无效：边界只能来自中间件解析出来的范围，一个能被请求
// 加宽的边界等于没有边界。
func TestMerchantDeviceListIgnoresStoreIDsFromTheQuery(t *testing.T) {
	repo := &fakeDeviceRepo{}
	api := newMerchantAPI(t, repo, authz.MerchantGrants{
		MerchantID: "m1", ScopeType: auth.ScopeTypeStore, ScopeID: "s1", StoreIDs: []string{"s1"},
	})

	// 带的那个点位刻意不是 UUID：后台那条路径上 ?storeIds= 会在 validUUID 处 400，
	// 商户这条路径根本不该认这个参数，连校验都不该有。
	w := api.call(t, merchantGrant(), http.MethodGet, "/v1/merchant/devices?storeIds=s9&status=active&keyword=SN")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(repo.seenFilter.StoreIDs, []string{"s1"}) {
		t.Fatalf("范围过滤=%v，查询串把它加宽了", repo.seenFilter.StoreIDs)
	}
	// 展示类过滤照常生效：被忽略的只有门店那一项。
	if repo.seenFilter.Status != "active" || repo.seenFilter.Keyword != "SN" {
		t.Fatalf("过滤=%+v，状态与关键字应当照常生效", repo.seenFilter)
	}
}

// 一个点位都没授权的账号是正常账号，不是错误。它产生的过滤器必须是一个空集合，而不是
// 「不过滤」——后者会让它看见全平台的设备。
func TestMerchantDeviceListWithEmptyScopeFiltersEverything(t *testing.T) {
	repo := &fakeDeviceRepo{}
	api := newMerchantAPI(t, repo, authz.MerchantGrants{
		MerchantID: "m1", ScopeType: auth.ScopeTypeBrand, ScopeID: "b-empty",
	})

	if w := api.call(t, merchantGrant(), http.MethodGet, "/v1/merchant/devices"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if repo.seenFilter.StoreIDs == nil {
		t.Fatal("StoreIDs 是 nil —— 到了 SQL 那边会被读成「不过滤」，而不是「没有点位」")
	}
	if len(repo.seenFilter.StoreIDs) != 0 {
		t.Fatalf("StoreIDs=%v，want 空", repo.seenFilter.StoreIDs)
	}
}

func TestMerchantDeviceDetailHidesOutOfScopeDevices(t *testing.T) {
	inside, outside := "s1", "s9"
	for _, tt := range []struct {
		name    string
		scope   []string
		device  *model.Device
		repoErr error
		path    string
		want    int
	}{
		{
			name:   "范围之内",
			scope:  []string{"s1", "s2"},
			device: &model.Device{ID: merchantDeviceID, StoreID: &inside},
			path:   "/v1/merchant/devices/" + merchantDeviceID,
			want:   http.StatusOK,
		},
		{
			// 403 会承认这个 id 存在、只是别人的。
			name:   "别的商户的设备",
			scope:  []string{"s1", "s2"},
			device: &model.Device{ID: merchantDeviceID, StoreID: &outside},
			path:   "/v1/merchant/devices/" + merchantDeviceID,
			want:   http.StatusNotFound,
		},
		{
			// 没挂点位的设备不在任何账号能授权的集合里。
			name:   "没有点位归属的设备",
			scope:  []string{"s1"},
			device: &model.Device{ID: merchantDeviceID},
			path:   "/v1/merchant/devices/" + merchantDeviceID,
			want:   http.StatusNotFound,
		},
		{
			name:    "设备不存在",
			scope:   []string{"s1"},
			repoErr: repository.ErrDeviceNotFound,
			path:    "/v1/merchant/devices/" + merchantDeviceID,
			want:    http.StatusNotFound,
		},
		{
			name:  "id 不是 UUID",
			scope: []string{"s1"},
			path:  "/v1/merchant/devices/not-a-uuid",
			want:  http.StatusBadRequest,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeDeviceRepo{byID: tt.device, err: tt.repoErr}
			api := newMerchantAPI(t, repo, authz.MerchantGrants{
				MerchantID: "m1", ScopeType: auth.ScopeTypeBrand, ScopeID: "b1", StoreIDs: tt.scope,
			})
			w := api.call(t, merchantGrant(), http.MethodGet, tt.path)
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
		})
	}
}

// 商户域这一轮只有读。写方法不能掉进一个不看方法的处理器里。
func TestMerchantDeviceRoutesAreReadOnly(t *testing.T) {
	repo := &fakeDeviceRepo{}
	api := newMerchantAPI(t, repo, authz.MerchantGrants{
		MerchantID: "m1", ScopeType: auth.ScopeTypeMerchant, StoreIDs: []string{"s1"},
	})
	for _, path := range []string{"/v1/merchant/devices", "/v1/merchant/devices/" + merchantDeviceID} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if w := api.call(t, merchantGrant(), method, path); w.Code != http.StatusNotFound {
				t.Fatalf("%s %s status=%d want 404", method, path, w.Code)
			}
		}
	}
	if repo.listCalls != 0 {
		t.Fatalf("写方法进入了读查询 %d 次", repo.listCalls)
	}
}

func TestMerchantRoutesRejectNonMerchantCallers(t *testing.T) {
	for _, tt := range []struct {
		name    string
		grant   auth.Grant
		noToken bool
		want    int
	}{
		{
			// 管理员的令牌不是商户令牌，它的 subject 也没有商户账号行能解析出范围。
			name:  "平台令牌",
			grant: auth.Grant{Subject: "admin", UserID: "admin", Realm: auth.RealmPlatform},
			want:  http.StatusForbidden,
		},
		{
			// 没有 tenant 就没有边界可解析，也没有任何东西可以默认成边界。
			name:  "没有 tenant 的商户令牌",
			grant: auth.Grant{Subject: "a1", UserID: "a1", Realm: auth.RealmMerchant},
			want:  http.StatusForbidden,
		},
		{
			name:    "没有令牌",
			noToken: true,
			want:    http.StatusUnauthorized,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeDeviceRepo{}
			api := newMerchantAPI(t, repo, authz.MerchantGrants{
				MerchantID: "m1", ScopeType: auth.ScopeTypeMerchant, StoreIDs: []string{"s1"},
			})
			var w *httptest.ResponseRecorder
			if tt.noToken {
				w = api.do(t, "", http.MethodGet, "/v1/merchant/devices")
			} else {
				w = api.call(t, tt.grant, http.MethodGet, "/v1/merchant/devices")
			}
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if api.calls != 0 {
				t.Fatal("对一个只凭令牌就该被拒的请求，去问了身份服务")
			}
			if repo.listCalls != 0 {
				t.Fatal("对一次被拒的请求执行了查询")
			}
		})
	}
}

// 取不到的边界永远不被替换成一个猜出来的边界：不回落到令牌里的，也不降级成空集或全量。
func TestMerchantRoutesFailClosedWhenTheScopeCannotBeResolved(t *testing.T) {
	for _, tt := range []struct {
		name   string
		grants authz.MerchantGrants
		err    error
		want   int
	}{
		{"账号被停用", authz.MerchantGrants{}, authz.ErrForbidden, http.StatusForbidden},
		{"令牌不再被接受", authz.MerchantGrants{}, authz.ErrUnauthenticated, http.StatusUnauthorized},
		{"身份服务不可用", authz.MerchantGrants{}, errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable},
		{
			// 答案说的是另一个商户：两边必有一边是错的，无法分辨，所以都不信。
			name:   "答案说的是别的商户",
			grants: authz.MerchantGrants{MerchantID: "m2", ScopeType: auth.ScopeTypeMerchant},
			want:   http.StatusServiceUnavailable,
		},
		{
			name:   "答案没有商户",
			grants: authz.MerchantGrants{ScopeType: auth.ScopeTypeMerchant},
			want:   http.StatusServiceUnavailable,
		},
		{
			// 认不出来的档位是拒绝而不是放宽：展开是照着这个值算的，这个值认不出来
			// 就意味着那组点位无法解释。
			name:   "答案带了一个不认识的档位",
			grants: authz.MerchantGrants{MerchantID: "m1", ScopeType: "region"},
			want:   http.StatusServiceUnavailable,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeDeviceRepo{devices: []*model.Device{{ID: merchantDeviceID}}}
			api := newMerchantAPI(t, repo, tt.grants)
			api.resolveErr = tt.err
			w := api.call(t, merchantGrant(), http.MethodGet, "/v1/merchant/devices")
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if repo.listCalls != 0 {
				t.Fatal("边界没定下来，查询却跑了")
			}
		})
	}
}

// 范围每个请求重取一次。收窄之后，同一枚还没过期的令牌的下一次请求必须立刻跟着收窄——
// 这正是它不签进 24 小时 access token 的全部理由。
func TestMerchantScopeIsResolvedFreshEveryRequest(t *testing.T) {
	repo := &fakeDeviceRepo{}
	api := newMerchantAPI(t, repo, authz.MerchantGrants{
		MerchantID: "m1", ScopeType: auth.ScopeTypeMerchant, StoreIDs: []string{"s1", "s2"},
	})
	// 同一枚令牌打两次。令牌没过期、内容也没变，变的只有数据范围。
	token := api.token(t, merchantGrant())

	if w := api.do(t, token, http.MethodGet, "/v1/merchant/devices"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(repo.seenFilter.StoreIDs, []string{"s1", "s2"}) {
		t.Fatalf("第一次请求的范围=%v", repo.seenFilter.StoreIDs)
	}

	api.grants = authz.MerchantGrants{
		MerchantID: "m1", ScopeType: auth.ScopeTypeStore, ScopeID: "s1", StoreIDs: []string{"s1"},
	}
	if w := api.do(t, token, http.MethodGet, "/v1/merchant/devices"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(repo.seenFilter.StoreIDs, []string{"s1"}) {
		t.Fatalf("范围=%v —— 它被复用了，而不是重取的", repo.seenFilter.StoreIDs)
	}
	if api.calls != 2 {
		t.Fatalf("解析次数=%d want 2", api.calls)
	}
}

// 路由表本身：同一条路径注册两次的话，gorilla/mux 会让先注册的那条吃掉所有方法，
// 表现出来是 GET 能用、另一条静默回 404。上面那些用例只打 GET，看不出来。
//
// 授了权的那个中间件在进处理器之前就短路，所以控制器可以带 nil 服务，整个用例不碰数据库。
func TestMerchantRoutesMapPathsToHandlers(t *testing.T) {
	probe := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	}
	server := khttp.NewServer()
	RegisterMerchant(runtime.NewHTTPRouter(server), controller.NewMerchantDeviceController(nil), probe)

	for _, path := range []string{"/v1/merchant/devices", "/v1/merchant/devices/" + merchantDeviceID} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Fatalf("GET %s status=%d want 418", path, w.Code)
		}
	}
	// 没挂上来的路径仍然是 404。饮品就是其中一条：商户端不做饮品，这条断言把它钉住。
	miss := httptest.NewRecorder()
	server.ServeHTTP(miss, httptest.NewRequest(http.MethodGet, "/v1/merchant/drinks", nil))
	if miss.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", miss.Code)
	}
}

// 漏传鉴权器是装配错误，必须失败关闭。这条挡的是「将来有人把 RegisterMerchant 的第三个
// 参数写成 nil，于是整片商户读接口变成匿名可读」。
func TestMerchantRoutesFailClosedWithoutAnAuthorizer(t *testing.T) {
	server := khttp.NewServer()
	RegisterMerchant(runtime.NewHTTPRouter(server), controller.NewMerchantDeviceController(nil), nil)

	r := httptest.NewRequest(http.MethodGet, "/v1/merchant/devices", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", w.Code, w.Body)
	}
}
