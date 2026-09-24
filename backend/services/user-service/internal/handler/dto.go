package handler

import (
	"time"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

type roleResponse struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
}

type permResponse struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Group       string `json:"group"`
	CreatedAt   string `json:"createdAt"`
}

func toRoleResponse(r *model.AdminRole) roleResponse {
	return roleResponse{
		ID:          r.ID,
		Code:        r.Code,
		Name:        r.Name,
		Description: r.Description,
		CreatedAt:   r.CreatedAt.Format(time.RFC3339),
	}
}

// miniappUserResponse 是小程序侧看到自己资料的全部字段。
//
// 刻意不含 status：能拿到 200 就说明账号是可用的，把这个字段回显出去只会让
// 前端多一个「什么情况下该显示什么」的分支，而答案永远是 active。
// 也不含 last_login_ip / login_count 这类运营审计信息——那是给自己人看的。
type miniappUserResponse struct {
	ID         string `json:"id"`
	Phone      string `json:"phone"`
	Nickname   string `json:"nickname"`
	AvatarURL  string `json:"avatarUrl"`
	Gender     string `json:"gender"`
	Birthday   string `json:"birthday"`
	RegionCode string `json:"regionCode"`
	RegionName string `json:"regionName"`
	CreatedAt  string `json:"createdAt"`
}

func toMiniappUserResponse(u *model.User) miniappUserResponse {
	birthday := ""
	// 生日可能没填（库里的 DATE 可空），空值和「没填」在 JSON 里都用空串表示，
	// 前端判空字符串即可，不用再区分 null。
	if u.Birthday != nil {
		birthday = u.Birthday.Format("2006-01-02")
	}
	return miniappUserResponse{
		ID:         u.ID,
		Phone:      u.Phone,
		Nickname:   u.Nickname,
		AvatarURL:  u.AvatarURL,
		Gender:     u.Gender,
		Birthday:   birthday,
		RegionCode: u.RegionCode,
		RegionName: u.RegionName,
		CreatedAt:  u.CreatedAt.Format(time.RFC3339),
	}
}

func toPermResponse(p *model.AdminPermission) permResponse {
	return permResponse{
		ID:          p.ID,
		Code:        p.Code,
		Name:        p.Name,
		Description: p.Description,
		Group:       p.PermGroup,
		CreatedAt:   p.CreatedAt.Format(time.RFC3339),
	}
}
