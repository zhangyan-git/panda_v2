package handler

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/platform/upload"
)

const (
	testBoundary = "pandauploadtestboundary"
	multipartCT  = "multipart/form-data; boundary=" + testBoundary
	uploadPath   = "/v1/admin/uploads/images"
)

// TestUploadImageRouteIsAlwaysRegistered 钉住「路由按配置条件注册」这个坑：
// 上传器没配好时接口必须仍在路由表里并返回 503，而不是 404。注册时少一条路由
// 的表现恰好是 404，而 404 的响应里没有任何一个字指向配置。
func TestUploadImageRouteIsAlwaysRegistered(t *testing.T) {
	stack := newUploadStack(t, NewAdminUploadHandler(nil, upload.Config{}.Validate()), []string{"admin:brands:manage"})
	w := stack.post(t, stack.token(t, ""), imageForm(t, "logo.png", png(64)))
	if w.Code == http.StatusNotFound {
		t.Fatal("route is missing: an unconfigured uploader must still answer, not 404")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want %d body=%s", w.Code, http.StatusServiceUnavailable, w.Body)
	}
	body := decodeResponse(t, w)
	if body.ErrorCode != "SERVICE_UNAVAILABLE" {
		t.Fatalf("errorCode=%q", body.ErrorCode)
	}
	// 缺哪个变量要点名，否则线上只能靠猜。
	for _, name := range []string{"OSS_ACCESS_KEY", "OSS_SECRET_KEY", "OSS_ENDPOINT", "OSS_BUCKET"} {
		if !strings.Contains(body.ErrorMessage, name) {
			t.Fatalf("errorMessage=%q must name %s", body.ErrorMessage, name)
		}
	}
}

// TestUploadImageAuthorization 走真实的中间件链（JWT → adminIdentity → 实时鉴权），
// 因此每一条断言都是页面真正会撞上的行为。
func TestUploadImageAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name        string
		anonymous   bool
		tenant      string
		permissions []string
		want        int
	}{
		{"anonymous", true, "", []string{"admin:brands:manage"}, http.StatusUnauthorized},
		{"tenant token", false, "merchant", []string{"admin:brands:manage"}, http.StatusForbidden},
		{"only view permissions", false, "", []string{"admin:brands:view", "admin:stores:view"}, http.StatusForbidden},
		{"brand manager", false, "", []string{"admin:brands:manage"}, http.StatusOK},
		{"store manager", false, "", []string{"admin:stores:manage"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := newUploadStack(t, nil, tc.permissions)
			token := ""
			if !tc.anonymous {
				token = stack.token(t, tc.tenant)
			}
			w := stack.post(t, token, imageForm(t, "logo.png", png(64)))
			if w.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestUploadImageReturnsAnAbsoluteURL 是这一轮唯一的端到端路径：真的 SDK 客户端、
// 真的内容嗅探、真的对象键，只有目的地是本机的假 OSS 端点，所以不需要任何凭据。
func TestUploadImageReturnsAnAbsoluteURL(t *testing.T) {
	stack := newUploadStack(t, nil, []string{"admin:brands:manage"})
	data := png(64)
	w := stack.post(t, stack.token(t, ""), imageForm(t, "logo.png", data))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", w.Code, w.Body)
	}
	var result upload.Result
	decodeResponse(t, w).into(t, &result)
	if parsed, err := url.Parse(result.URL); err != nil || !parsed.IsAbs() {
		t.Fatalf("url=%q err=%v: 前端会把它直接塞进 <img>，必须是绝对地址", result.URL, err)
	}
	sum := md5.Sum(data)
	digest := hex.EncodeToString(sum[:])
	wantKey := "panda-v2/images/" + digest[:2] + "/" + digest[2:4] + "/" + digest + ".png"
	if result.Key != wantKey || !strings.HasSuffix(result.URL, "/"+wantKey) {
		t.Fatalf("key=%q url=%q want key %q as the url suffix", result.Key, result.URL, wantKey)
	}
	if result.Name != "logo.png" || result.Size != int64(len(data)) || result.ContentType != "image/png" || result.MD5 != digest {
		t.Fatalf("result=%+v", result)
	}
	// 假端点收到的内容必须与客户端发的一模一样：它同时证明了 handler 从 multipart
	// 里取出的字节没有多也没有少。
	put := stack.oss.only(t)
	if put.method != http.MethodPut || !strings.HasSuffix(put.path, "/"+wantKey) {
		t.Fatalf("oss request = %s %s, want PUT .../%s", put.method, put.path, wantKey)
	}
	if put.contentType != "image/png" {
		t.Fatalf("stored contentType=%q, want the sniffed image/png", put.contentType)
	}
	if !bytes.Equal(put.body, data) {
		t.Fatalf("stored %d bytes, uploaded %d", len(put.body), len(data))
	}
}

func TestUploadImageLimits(t *testing.T) {
	const limit = 32
	for _, tc := range []struct {
		name        string
		form        *bytes.Buffer
		want        int
		wantMessage string
	}{
		{"at the limit", imageForm(t, "logo.png", png(limit)), http.StatusOK, ""},
		{"over the limit", imageForm(t, "logo.png", png(limit+1)), http.StatusRequestEntityTooLarge, "32B"},
		// 文件本身没过界，但整个请求体超过「上限 + 余量」：这是防磁盘写满那行的
		// 唯一可达路径——超出的部分在读进内存前就被切断，而不是溢写到 /tmp。
		{
			"oversized request body",
			imageFormWithField(t, "note", bytes.Repeat([]byte("x"), 1<<20), "logo.png", png(limit)),
			http.StatusRequestEntityTooLarge,
			"32B",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := newUploadStackWithLimit(t, limit, []string{"admin:brands:manage"})
			w := stack.post(t, stack.token(t, ""), tc.form)
			if w.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.want, w.Body)
			}
			if body := decodeResponse(t, w); tc.wantMessage != "" && !strings.Contains(body.ErrorMessage, tc.wantMessage) {
				t.Fatalf("errorMessage=%q must mention %q", body.ErrorMessage, tc.wantMessage)
			}
		})
	}
}

func TestUploadImageRejections(t *testing.T) {
	for _, tc := range []struct {
		name        string
		build       func(t *testing.T) *bytes.Buffer
		contentType string
		wantMessage string
	}{
		{
			"no file part",
			func(t *testing.T) *bytes.Buffer { return imageFormWithField(t, "note", []byte("x"), "", nil) },
			multipartCT,
			"缺少文件字段 file",
		},
		{
			"empty file",
			func(t *testing.T) *bytes.Buffer { return imageForm(t, "logo.png", nil) },
			multipartCT,
			"上传的文件为空",
		},
		{
			"disallowed extension",
			func(t *testing.T) *bytes.Buffer { return imageForm(t, "logo.svg", png(64)) },
			multipartCT,
			"仅支持",
		},
		{
			// 改名骗不过嗅探：能执行脚本的格式不该因为后缀是 .png 就进公开桶。
			"html pretending to be a png",
			func(t *testing.T) *bytes.Buffer {
				return imageForm(t, "logo.png", bytes.Repeat([]byte("<html><script>x</script>"), 8))
			},
			multipartCT,
			"文件内容不是受支持的图片格式",
		},
		{
			"not multipart",
			func(t *testing.T) *bytes.Buffer {
				return bytes.NewBufferString(`{"url":"https://example.test/logo.png"}`)
			},
			"application/json",
			"请求必须是 multipart/form-data",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := tc.build(t)
			stack := newUploadStack(t, nil, []string{"admin:brands:manage"})
			w := stack.postContentType(t, stack.token(t, ""), form, tc.contentType)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 body=%s", w.Code, w.Body)
			}
			body := decodeResponse(t, w)
			if body.ErrorCode != "INVALID_REQUEST" || !strings.Contains(body.ErrorMessage, tc.wantMessage) {
				t.Fatalf("response=%+v must contain %q", body, tc.wantMessage)
			}
			if put := stack.oss.requests(); len(put) != 0 {
				t.Fatalf("a rejected upload must not reach object storage: %+v", put)
			}
		})
	}
}

// TestUploadImageReportsStorageFailure 让假端点回 500：OSS 侧失败对调用方一律是
// 503（「存储暂不可用」），而不是漏成 500——500 会把一次下游故障说成服务端 bug。
func TestUploadImageReportsStorageFailure(t *testing.T) {
	stack := newUploadStack(t, nil, []string{"admin:brands:manage"})
	stack.oss.fail(t, http.StatusInternalServerError)
	w := stack.post(t, stack.token(t, ""), imageForm(t, "logo.png", png(64)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", w.Code, w.Body)
	}
	if body := decodeResponse(t, w); body.ErrorCode != "SERVICE_UNAVAILABLE" || body.ErrorMessage != "图片存储暂不可用" {
		t.Fatalf("response=%+v", body)
	}
}

// uploadStack 是被测的那一套东西：真实 JWT 中间件、真实实时鉴权链、真实路由表。
type uploadStack struct {
	router http.Handler
	jwt    *auth.Service
	oss    *fakeOSS
}

// newUploadStack 组装一套完整的栈。up 为 nil 表示用一个指向假端点的真上传器。
func newUploadStack(t *testing.T, up *AdminUploadHandler, permissions []string) *uploadStack {
	t.Helper()
	return newUploadStackWithLimit(t, 1024, permissions, up)
}

func newUploadStackWithLimit(t *testing.T, maxSize int64, permissions []string, up ...*AdminUploadHandler) *uploadStack {
	t.Helper()
	oss := newFakeOSS(t)
	if len(up) == 0 || up[0] == nil {
		up = []*AdminUploadHandler{NewAdminUploadHandler(testUploader(t, oss.URL, maxSize), nil)}
	}
	jwt := testJWT(t)
	authorizer, _ := liveAccess(t, []string{"editor"}, permissions)
	s := runtime.NewHTTPRouter(khttp.NewServer())
	Register(s, &AdminMerchantHandler{}, &AdminBrandHandler{}, &AdminStoreHandler{}, up[0], jwt, authorizer)
	return &uploadStack{router: s, jwt: jwt, oss: oss}
}

// token 为空租户签发一个后台管理员令牌。权限不写进令牌：实时的那一次查询才是
// 唯一来源，令牌里写什么都改变不了鉴权结果。
func (s *uploadStack) token(t *testing.T, tenant string) string {
	t.Helper()
	return testToken(t, s.jwt, auth.Grant{Subject: "admin", UserID: "admin", Tenant: tenant})
}

func (s *uploadStack) post(t *testing.T, token string, body *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	return s.postContentType(t, token, body, multipartCT)
}

func (s *uploadStack) postContentType(t *testing.T, token string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, uploadPath, body)
	r.Header.Set("Content-Type", contentType)
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, r)
	return w
}

// testUploader 组装一个真实的上传器：真的 SDK 客户端、真的校验、真的对象键，
// 只有写入的目的地是本机假端点。它认得出 IP 形式的 endpoint 并改用 path style
// （/bucket/key），所以凭据不必是真的——签名在本地算，没人验。
func testUploader(t *testing.T, endpoint string, maxSize int64) *upload.Uploader {
	t.Helper()
	up, err := upload.New(upload.Config{
		AccessKey:   "test-access-key",
		SecretKey:   "test-secret-key",
		Endpoint:    endpoint,
		Bucket:      "test-bucket",
		MaxFileSize: maxSize,
		UseMD5:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// fakeOSS 是一个本机的假 OSS 端点，记录收到的每一次写入。
type fakeOSS struct {
	*httptest.Server

	mu       sync.Mutex
	received []ossRequest
	status   int
}

type ossRequest struct {
	method, path, contentType string
	body                      []byte
}

func newFakeOSS(t *testing.T) *fakeOSS {
	t.Helper()
	f := &fakeOSS{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the OSS request body: %v", err)
		}
		f.mu.Lock()
		f.received = append(f.received, ossRequest{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body})
		status := f.status
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOSS) fail(t *testing.T, status int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeOSS) requests() []ossRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ossRequest(nil), f.received...)
}

func (f *fakeOSS) only(t *testing.T) ossRequest {
	t.Helper()
	received := f.requests()
	if len(received) != 1 {
		t.Fatalf("object storage received %d requests, want exactly 1: %+v", len(received), received)
	}
	return received[0]
}

// imageForm 拼一个只含 file 部分的 multipart 请求体。filename 为空表示不写 file
// 部分，用来测「缺少文件字段」。data 为 nil 写出一个 0 字节的文件。
func imageForm(t *testing.T, filename string, data []byte) *bytes.Buffer {
	t.Helper()
	return imageFormWithField(t, "", nil, filename, data)
}

// imageFormWithField 先在 file 之前放一个普通字段，用来把整个请求体顶过上限。
func imageFormWithField(t *testing.T, field string, value []byte, filename string, data []byte) *bytes.Buffer {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.SetBoundary(testBoundary); err != nil {
		t.Fatal(err)
	}
	if field != "" {
		if err := w.WriteField(field, string(value)); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &body
}

// png 造一段够嗅探器认出 PNG 的字节。8 字节签名是 DetectContentType 判定的全部
// 依据，后面补零凑长度。
func png(size int) []byte {
	data := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if size < len(data) {
		size = len(data)
	}
	return append(data, make([]byte, size-len(data))...)
}

type responseBody struct {
	Success      bool            `json:"success"`
	Data         json.RawMessage `json:"data"`
	ErrorCode    string          `json:"errorCode"`
	ErrorMessage string          `json:"errorMessage"`
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) responseBody {
	t.Helper()
	var body responseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v; body=%s", err, w.Body)
	}
	return body
}

func (b responseBody) into(t *testing.T, target any) {
	t.Helper()
	if err := json.Unmarshal(b.Data, target); err != nil {
		t.Fatalf("invalid data: %v; data=%s", err, b.Data)
	}
}
