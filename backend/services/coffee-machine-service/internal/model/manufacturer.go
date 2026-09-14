package model

import "time"

// Manufacturer 是 manufacturers 表在列表查询里用到的列。
//
// 接入凭据（manufacturer_credentials）不在这个结构里，也不会进任何返回给前端的
// 载荷：密钥与令牌只在这张表被查询时按需取，混进厂商对象就等于给每个列表接口
// 都带上了密钥。
type Manufacturer struct {
	ID           string    `db:"id"`
	Code         string    `db:"code"`
	Name         string    `db:"name"`
	ContactName  string    `db:"contact_name"`
	ContactPhone string    `db:"contact_phone"`
	Status       string    `db:"status"`
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}
