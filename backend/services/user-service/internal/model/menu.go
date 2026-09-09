package model

import "time"

// Menu 后台菜单节点（目录或菜单），树关系由 ParentID 表达
type Menu struct {
	ID        string    `db:"id"`
	ParentID  string    `db:"parent_id"` // 空字符串表示顶级
	Name      string    `db:"name"`
	Path      string    `db:"path"` // 前端路由路径；目录为空
	Icon      string    `db:"icon"`
	Sort      int       `db:"sort"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}
