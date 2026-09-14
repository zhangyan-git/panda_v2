package model

import (
	"errors"
	"strings"
)

var (
	ErrInvalidRoleCode  = errors.New("角色代码不能为空、包含首尾空白或逗号、引号、换行等 CSV 特殊字符")
	ErrReservedRoleCode = errors.New("保留角色代码不可创建或修改")
	ErrRoleCodeConflict = errors.New("角色代码已存在或存在同名平台授权规则，请使用其他代码")
)

// 超级管理员的角色码。两种写法在数据里都出现过：「超级管理员」是早期形式。
// 这枚角色码也是平台唯一「绕过权限模型」的标记，所以它既被保留（不允许创建或
// 改名）又被鉴权用于放行判断，两处必须是同一份定义。
const (
	SuperRoleCode       = "super_admin"
	legacySuperRoleCode = "超级管理员"
)

// IsSuperRoleCode reports whether the code marks a super administrator, who
// passes every permission check.
func IsSuperRoleCode(code string) bool {
	return code == SuperRoleCode || code == legacySuperRoleCode
}

func IsReservedRoleCode(code string) bool {
	return IsSuperRoleCode(code)
}

// ValidateRoleCode preserves the current CSV adapter's exact authorization identifier.
func ValidateRoleCode(code string) error {
	if code == "" || strings.TrimSpace(code) != code || strings.ContainsAny(code, ",\"\r\n\x00") {
		return ErrInvalidRoleCode
	}
	return nil
}

func ValidateRoleCodeChange(oldCode, code string) error {
	if err := ValidateRoleCode(code); err != nil {
		return err
	}
	if oldCode != code && (IsReservedRoleCode(oldCode) || IsReservedRoleCode(code)) {
		return ErrReservedRoleCode
	}
	return nil
}
