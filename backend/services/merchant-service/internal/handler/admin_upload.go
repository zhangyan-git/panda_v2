package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/upload"
)

const (
	// imageField 是 multipart 里文件字段的名字。与旧后端、与 AntD Upload 的
	// 默认字段名一致，前端因此不需要额外配置。
	imageField = "file"
	// multipartOverhead 是给 multipart 边界和头部留的余量。它不参与「文件够不够
	// 小」的判断——那由 upload.Upload 按解码后的字节数决定——只用来保证一个
	// 过大的请求体在读干净之前就被切断。
	multipartOverhead = 1 << 20
)

// errMissingFile 表示请求里没有 file 部分。
var errMissingFile = errors.New("缺少文件字段 file")

// AdminUploadHandler 是后台的图片上传接口。
type AdminUploadHandler struct {
	uploader *upload.Uploader
	// unavailable 是 uploader 缺席的原因，原样回给调用方：上传没配好时要能
	// 一眼看出缺的是哪个环境变量，而不是笼统的一句「未配置」。
	unavailable error
}

// NewAdminUploadHandler 组装上传接口。
//
// uploader 为 nil 不等于接口消失：路由照常注册，每个请求返回 503 并点名缺失的
// 配置。按配置条件注册会让同一份二进制在不同环境有不同路由表，网关转发过来
// 看到的是 404——看着像路由 bug，响应里却没有一个字指向配置。
func NewAdminUploadHandler(uploader *upload.Uploader, unavailable error) *AdminUploadHandler {
	if uploader == nil && unavailable == nil {
		unavailable = errors.New("upload: not configured")
	}
	return &AdminUploadHandler{uploader: uploader, unavailable: unavailable}
}

// UploadImage godoc
//
//	@Summary     上传图片到对象存储
//	@Description 接收 multipart/form-data（字段名 file），只接受图片，返回可直接渲染的绝对 URL。
//	@Tags        admin-uploads
//	@Accept      multipart/form-data
//	@Produce     json
//	@Security    BearerAuth
//	@Param       file formData file true "图片文件"
//	@Success     200 {object} api.Response{data=upload.Result}
//	@Failure     400 {object} api.Response
//	@Failure     413 {object} api.Response
//	@Failure     503 {object} api.Response
//	@Router      /v1/admin/uploads/images [post]
func (h *AdminUploadHandler) UploadImage(w http.ResponseWriter, r *http.Request) {
	if h.uploader == nil {
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "图片上传未配置："+h.unavailable.Error())
		return
	}
	maxSize := h.uploader.MaxFileSize()
	// kratos 不给请求体设任何上限，而 ParseMultipartForm 只在内存里限额、超出
	// 部分无限溢写到 /tmp。没有这一行，任何一个有效管理员（或一个被盗的 token）
	// 都能把容器磁盘写满。
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+multipartOverhead)

	reader, err := r.MultipartReader()
	if err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求必须是 multipart/form-data")
		return
	}
	data, name, err := readImagePart(reader, maxSize)
	if err != nil {
		h.writeUploadError(w, err)
		return
	}
	result, err := h.uploader.Upload(r.Context(), name, data)
	if err != nil {
		h.writeUploadError(w, err)
		return
	}
	api.Success(w, result)
}

// readImagePart 找到名为 file 的部分并读出内容，最多读 maxSize+1 字节。
//
// 用 MultipartReader 而不是 ParseMultipartForm：后者会把超过内存上限的部分
// 无限溢写到磁盘临时文件，还多出一个必须记得调用的 RemoveAll。
func readImagePart(reader *multipart.Reader, maxSize int64) ([]byte, string, error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, "", errMissingFile
		}
		if err != nil {
			return nil, "", err
		}
		if part.FormName() != imageField {
			_ = part.Close()
			continue
		}
		name := part.FileName()
		// 多读一个字节：正好等于上限和超过上限必须能分开，只有多这一个字节
		// 才分得开。这样上限只有一个定义处（upload.Uploader），这里只是
		// 防止读进一个不该被读进来的东西。
		data, err := io.ReadAll(io.LimitReader(part, maxSize+1))
		_ = part.Close()
		if err != nil {
			return nil, "", err
		}
		return data, name, nil
	}
}

// writeUploadError 把上传失败翻译成状态码。
func (h *AdminUploadHandler) writeUploadError(w http.ResponseWriter, err error) {
	var (
		sizeErr *upload.SizeError
		typeErr *upload.TypeError
		bodyErr *http.MaxBytesError
	)
	switch {
	case errors.As(err, &sizeErr):
		api.Error(w, http.StatusRequestEntityTooLarge, api.CodeInvalidRequest, sizeErr.Error())
	case errors.As(err, &bodyErr):
		// 整个请求体（含 multipart 边框）就超了，还没轮到判定文件本身。
		api.Error(w, http.StatusRequestEntityTooLarge, api.CodeInvalidRequest,
			fmt.Sprintf("图片不能超过 %s", upload.HumanBytes(h.uploader.MaxFileSize())))
	case errors.Is(err, errMissingFile):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case errors.Is(err, upload.ErrEmptyFile), errors.As(err, &typeErr):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case errors.Is(err, context.Canceled):
		// 客户端半路断开，响应已经没人收；写什么都行，但别把它记成服务端故障。
		api.Error(w, http.StatusRequestTimeout, api.CodeTimeout, "上传已取消")
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		// kratos 的整请求 deadline 到期在读 body 上表现为 i/o timeout。
		// 让它漏成通用的 500 会把「传得太慢」说成「服务器坏了」。
		api.Error(w, http.StatusRequestTimeout, api.CodeTimeout, "上传超时，请重试或改用更小的图片")
	default:
		// OSS 自己的失败。调用方无论哪种原因都只看到 503，所以原因必须在这里
		// 留下痕迹，否则线上出问题就只有手工复现这一条路。
		slog.Error("upload: writing the image to object storage failed", "error", err)
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "图片存储暂不可用")
	}
}

// isTimeout 判断读 body 时遇到的 i/o timeout。
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
