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

func IsReservedRoleCode(code string) bool {
	return code == "super_admin" || code == "超级管理员"
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
