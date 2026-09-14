package service

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// 后台用户管理相关的错误，handler 据此选状态码。
var (
	// ErrUserNotFound 目标账号不存在。
	ErrUserNotFound = errors.New("用户不存在")
	// ErrUserStatusInvalid 目标状态不是后台可设置的值。
	ErrUserStatusInvalid = errors.New("status 只能为 active 或 disabled")
	// ErrUserStatusFilterInvalid 筛选条件不是合法的状态取值。
	//
	// 和上面那个分开：筛选时 deleted 是合法的（要能查已注销的账号），设置时不是
	// （注销是用户自己的决定，后台不给改）。共用一个错误值的话，筛选写错时提示
	// 「只能为 active 或 disabled」，而调用方明明看到 deleted 能筛出结果。
	ErrUserStatusFilterInvalid = errors.New("status 只能是 active、disabled 或 deleted")
	// ErrUserKeywordTooLong 搜索词过长。
	ErrUserKeywordTooLong = errors.New("搜索关键词过长")
)

// 搜索关键词的长度上限。手机号 11 位、昵称 32 字，超过这个长度的输入不可能是
// 有效的搜索词，而它会被拼成 LIKE 模式串发到库里。
const maxUserKeywordRunes = 64

// 详情页带回的登录记录条数。够回答「他最近是不是一直在失败」，又不至于让一个
// 正常用户的历史把响应撑大——完整历史仍然要查库。
const recentLoginEventLimit = 20

// 后台列表里允许按状态筛选的取值：加空串表示「不限」。
func validUserStatusFilter(status string) bool {
	switch status {
	case "", model.UserStatusActive, model.UserStatusDisabled, model.UserStatusDeleted:
		return true
	default:
		return false
	}
}

// MiniappUserDetail 是后台详情页需要的全部内容。
//
// 多出来的三样不是装饰：后台看用户十有八九是因为他登不上，而「绑了几个微信、
// 现在还有几个会话活着、最近几次登录成没成功」正是回答这个问题的全部材料。
// 少了它们，客服只能来问开发，而这些数据本来就在库里。
type MiniappUserDetail struct {
	User             *model.User
	WechatIdentities []*model.UserWechatIdentity
	// ActiveSessions 未撤销且未过期的会话数。
	ActiveSessions int64
	LoginEvents    []*model.UserLoginEvent
}

// AdminMiniappUserService 是后台对小程序用户的管理。
type AdminMiniappUserService struct {
	users      repository.UserRepository
	sessions   repository.UserSessionRepository
	adminUsers repository.AdminMiniappUserRepository
}

func NewAdminMiniappUserService(
	users repository.UserRepository,
	sessions repository.UserSessionRepository,
	adminUsers repository.AdminMiniappUserRepository,
) *AdminMiniappUserService {
	return &AdminMiniappUserService{users: users, sessions: sessions, adminUsers: adminUsers}
}

// List 返回一页用户及其总数。入参校验放在这里而不是 handler：状态取值和关键词
// 长度是「什么算合法查询」的规则，将来后台之外（比如导出的定时任务）复用同一套。
func (s *AdminMiniappUserService) List(
	ctx context.Context, page, pageSize int, status, keyword string,
) ([]*model.User, int64, error) {
	status = strings.TrimSpace(status)
	if !validUserStatusFilter(status) {
		return nil, 0, ErrUserStatusFilterInvalid
	}
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > maxUserKeywordRunes {
		return nil, 0, ErrUserKeywordTooLong
	}
	filter := repository.MiniappUserFilter{Keyword: keyword, Status: status}
	users, err := s.adminUsers.FindPage(ctx, filter, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.adminUsers.Count(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// Detail 组装详情页数据。
//
// 账号本身查不到就直接返回，后面三项不再查：它们都以 user_id 为条件，账号不
// 存在时只会得到三个空结果，白白三次往返。
func (s *AdminMiniappUserService) Detail(ctx context.Context, id string) (*MiniappUserDetail, error) {
	user, err := s.users.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	identities, err := s.users.ListWechatIdentitiesByUser(ctx, id)
	if err != nil {
		return nil, err
	}
	sessions, err := s.sessions.CountActiveByUser(ctx, id)
	if err != nil {
		return nil, err
	}
	events, err := s.users.ListLoginEventsByUser(ctx, id, recentLoginEventLimit)
	if err != nil {
		return nil, err
	}
	return &MiniappUserDetail{
		User:             user,
		WechatIdentities: identities,
		ActiveSessions:   sessions,
		LoginEvents:      events,
	}, nil
}

// UpdateStatus 改账号状态，返回被连带撤销的会话数。
//
// 状态是否合法在这里判，能不能改（比如已注销）在仓库的事务里判——后者要看那一行
// 此刻的取值，只有读的时候才作数。
func (s *AdminMiniappUserService) UpdateStatus(ctx context.Context, id, status string) (int64, error) {
	if !model.AdminSettableStatus(status) {
		return 0, ErrUserStatusInvalid
	}
	revoked, err := s.adminUsers.UpdateStatus(ctx, id, status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrUserNotFound
	case err != nil:
		return 0, err
	}
	return revoked, nil
}
