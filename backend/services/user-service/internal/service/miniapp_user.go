package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// 资料字段的长度与格式限制。
//
// 库里这些列都是 TEXT，写多长都不会报错——所以限制只能在这里。不设限的话，
// 一个 10 万字的昵称会跟着每一张订单、每一条评价一路带到商户端的列表里。
const (
	maxNicknameRunes = 32
	maxAvatarURLLen  = 512
	maxRegionRunes   = 32
)

// 资料更新相关的错误，handler 一律映射成 400 并把文案直接回给用户。
var (
	ErrNicknameInvalid = errors.New("昵称长度需在 1-32 个字符之间")
	ErrGenderInvalid   = errors.New("性别取值不合法")
	ErrBirthdayInvalid = errors.New("生日格式应为 YYYY-MM-DD")
	ErrAvatarInvalid   = errors.New("头像地址不合法")
	ErrRegionInvalid   = errors.New("地区信息过长")
)

// ProfileUpdate 是一次资料部分更新，nil 表示「本次不改这一项」。
//
// 全部用指针：昵称、地区都允许被清空，空串是合法的目标值，拿它当「没传」会让
// 用户再也改不掉自己填错的内容。
type ProfileUpdate struct {
	Nickname *string
	// AvatarURL 传空串表示清空头像。
	AvatarURL *string
	// Gender 取值同 model.ValidGender。
	Gender *string
	// Birthday 是 "2006-01-02"，传空串表示清空——日期的「清空」没有零值可表达，
	// 所以用空串当那个信号，和上面两个字段的规则一致。
	Birthday   *string
	RegionCode *string
	RegionName *string
}

// MiniappUserService 是小程序用户的个人资料与账号绑定。
type MiniappUserService struct {
	users    repository.UserRepository
	smsCodes repository.SMSCodeRepository
}

func NewMiniappUserService(users repository.UserRepository, smsCodes repository.SMSCodeRepository) *MiniappUserService {
	return &MiniappUserService{users: users, smsCodes: smsCodes}
}

// Profile 返回当前登录用户，并顺带确认账号仍然可用。
//
// 状态在这里再查一次而不是只靠中间件：access token 有 24 小时有效期，期间
// 账号可能已经被禁用或注销，只验签名的话这枚令牌还能用满一整天。
func (s *MiniappUserService) Profile(ctx context.Context, userID string) (*model.User, error) {
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := ensureConsumerActive(user); err != nil {
		return nil, err
	}
	return user, nil
}

// UpdateProfile 更新资料并返回更新后的用户。
//
// 先读一次当前资料再写：写完之后要回显最新的完整资料，而部分更新的响应里
// 不该有「没传的字段是空的」这种歧义。多一次往返换来响应不产生歧义。
func (s *MiniappUserService) UpdateProfile(ctx context.Context, userID string, upd ProfileUpdate) (*model.User, error) {
	repoUpd, err := validateProfileUpdate(upd)
	if err != nil {
		return nil, err
	}
	if err := s.users.UpdateProfile(ctx, userID, repoUpd); err != nil {
		return nil, err
	}
	return s.users.FindByID(ctx, userID)
}

// BindPhone 用一条 bind_phone 用途的验证码把手机号绑到当前账号上。
//
// 绑成功之后不动已有会话，这是想清楚的决定：手机号是登录标识，但它不是让
// 已有令牌继续有效的凭据（那是刷新令牌）。绑定只影响「以后谁能登进来」，
// 而要做到「用别人的账号绑自己的手机号」得先拿到那个账号的会话，那时候
// 撤销会话也救不了。反过来，改绑后立刻撤销所有会话会把用户当场踢下线——
// 他刚在界面里点了确认，还要立刻重新登录一次。
func (s *MiniappUserService) BindPhone(ctx context.Context, userID, phone, code string) (*model.User, error) {
	if !ValidCNMobile(phone) {
		return nil, ErrInvalidPhone
	}
	if strings.TrimSpace(code) == "" {
		return nil, ErrLoginCredentialMissing
	}
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := ensureConsumerActive(user); err != nil {
		return nil, err
	}
	// 已经是这个号了就没什么好绑的。提前返回而不是走一遍验证码校验：那会白白
	// 消耗掉用户刚收到的那条码，他还得再要一条才能做别的事。
	if user.Phone == phone {
		return user, nil
	}
	if err := verifyAndConsumeSMSCode(ctx, s.smsCodes, phone, model.SMSPurposeBindPhone, code); err != nil {
		return nil, err
	}
	// 手机号已被别的账号占用时 BindPhone 返回 model.ErrPhoneTaken，原样上抛：
	// 它的文案（「手机号已被其他账号绑定」）就是该给用户看的那句。
	if err := s.users.BindPhone(ctx, userID, phone); err != nil {
		return nil, err
	}
	return s.users.FindByID(ctx, userID)
}

// validateProfileUpdate 把 service 层的更新意图校验并翻译成 repository 的入参。
//
// 校验放在这里而不是 handler：字段规则是业务规则，而且换绑/导入等其他写入路径
// 将来也要走同一套判断，写在 HTTP 层就只有这一条路走得到。
func validateProfileUpdate(upd ProfileUpdate) (repository.UserProfileUpdate, error) {
	var out repository.UserProfileUpdate

	if upd.Nickname != nil {
		name := strings.TrimSpace(*upd.Nickname)
		// 全空格的昵称等于没有昵称，但又不是空串，显示出来是一片空白。
		if n := utf8.RuneCountInString(name); n == 0 || n > maxNicknameRunes {
			return out, ErrNicknameInvalid
		}
		out.Nickname = &name
	}
	if upd.AvatarURL != nil {
		url := strings.TrimSpace(*upd.AvatarURL)
		if len(url) > maxAvatarURLLen {
			return out, ErrAvatarInvalid
		}
		// 只收 http(s)：头像地址会被前端塞进 <image src>，放行 javascript:/data:
		// 这类 scheme 等于把一个可控的取值交给渲染层，那是 XSS 的入口。
		if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return out, ErrAvatarInvalid
		}
		out.AvatarURL = &url
	}
	if upd.Gender != nil {
		gender := strings.TrimSpace(*upd.Gender)
		if !model.ValidGender(gender) {
			return out, ErrGenderInvalid
		}
		out.Gender = &gender
	}
	if upd.Birthday != nil {
		raw := strings.TrimSpace(*upd.Birthday)
		if raw == "" {
			out.ClearBirthday = true
		} else {
			day, err := time.Parse("2006-01-02", raw)
			if err != nil {
				return out, ErrBirthdayInvalid
			}
			// 未来的生日没有意义，多半是输入错误。挡在这里，免得一个明显不对的
			// 数据进库之后要靠人去发现。
			if day.After(time.Now()) {
				return out, ErrBirthdayInvalid
			}
			out.Birthday = &day
		}
	}
	if upd.RegionCode != nil {
		code := strings.TrimSpace(*upd.RegionCode)
		if utf8.RuneCountInString(code) > maxRegionRunes {
			return out, ErrRegionInvalid
		}
		out.RegionCode = &code
	}
	if upd.RegionName != nil {
		name := strings.TrimSpace(*upd.RegionName)
		if utf8.RuneCountInString(name) > maxRegionRunes {
			return out, ErrRegionInvalid
		}
		out.RegionName = &name
	}
	return out, nil
}
