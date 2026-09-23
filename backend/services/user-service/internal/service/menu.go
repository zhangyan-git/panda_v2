package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// 菜单管理业务错误
var (
	ErrMenuNameRequired = errors.New("菜单名称不能为空")
	ErrMenuHasChildren  = errors.New("存在子菜单，请先删除子菜单")
	ErrMenuParentCycle  = errors.New("不能将菜单移动到自己的子树下")
)

// MenuNode 菜单树节点，直接用于 API 响应
type MenuNode struct {
	ID       string      `json:"id"`
	ParentID string      `json:"parentId"`
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Icon     string      `json:"icon"`
	Sort     int         `json:"sort"`
	Children []*MenuNode `json:"children,omitempty"`
}

// AdminMenuService 后台菜单树与角色-菜单绑定
type AdminMenuService struct {
	menus    repository.MenuRepository
	roles    repository.AdminRoleRepository
	bindings repository.AdminBindingRepository
}

func NewAdminMenuService(
	menus repository.MenuRepository,
	roles repository.AdminRoleRepository,
	bindings repository.AdminBindingRepository,
) *AdminMenuService {
	return &AdminMenuService{menus: menus, roles: roles, bindings: bindings}
}

// ListTree 返回完整菜单树（管理端）
func (s *AdminMenuService) ListTree(ctx context.Context) ([]*MenuNode, error) {
	menus, err := s.menus.FindAll(ctx)
	if err != nil {
		return nil, err
	}
	return buildMenuTree(menus, nil), nil
}

// TreeForIdentity 返回当前身份可见的菜单树：
// 超级管理员看到全部；其他身份取角色绑定菜单的并集，并补齐父级目录。
//
// 是否超管只看库里的实时角色，不看 token：token 里的角色在过期前不会变，
// 用它就意味着「把某人踢出超管」后他仍能在最长 24 小时内看到全部入口。
func (s *AdminMenuService) TreeForIdentity(ctx context.Context, identity auth.Identity) ([]*MenuNode, error) {
	menus, err := s.menus.FindAll(ctx)
	if err != nil {
		return nil, err
	}
	roles, err := s.bindings.FindRolesByUser(ctx, identity.UserID)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]bool)
	for _, role := range roles {
		if role != nil && model.IsSuperRoleCode(role.Code) {
			return buildMenuTree(menus, nil), nil
		}
		menuIDs, err := s.menus.FindMenuIDsByRoleID(ctx, role.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range menuIDs {
			visible[id] = true
		}
	}
	if len(visible) == 0 {
		return []*MenuNode{}, nil
	}

	// 补齐父级目录，保证子菜单在树上可达
	byID := make(map[string]*model.Menu, len(menus))
	for _, m := range menus {
		byID[m.ID] = m
	}
	for id := range visible {
		for cur := byID[id]; cur != nil && cur.ParentID != ""; cur = byID[cur.ParentID] {
			if visible[cur.ParentID] {
				break
			}
			visible[cur.ParentID] = true
		}
	}

	filter := func(id string) bool { return visible[id] }
	return buildMenuTree(menus, filter), nil
}

// Create 新建菜单；parentID 为空表示顶级
func (s *AdminMenuService) Create(ctx context.Context, parentID, name, path, icon string, sort int) (*MenuNode, error) {
	if name == "" {
		return nil, ErrMenuNameRequired
	}
	if parentID != "" {
		if _, err := s.menus.FindByID(ctx, parentID); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	m := &model.Menu{
		ID:        uuid.NewString(),
		ParentID:  parentID,
		Name:      name,
		Path:      path,
		Icon:      icon,
		Sort:      sort,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.menus.Create(ctx, m); err != nil {
		return nil, err
	}
	return toMenuNode(m), nil
}

// Update 更新菜单，禁止把菜单挂到自己的子树下
func (s *AdminMenuService) Update(ctx context.Context, id, parentID, name, path, icon string, sort int) (*MenuNode, error) {
	if name == "" {
		return nil, ErrMenuNameRequired
	}
	m, err := s.menus.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if parentID != "" {
		if parentID == id {
			return nil, ErrMenuParentCycle
		}
		if _, err := s.menus.FindByID(ctx, parentID); err != nil {
			return nil, err
		}
		// parentID 不能是 id 的后代
		inSubtree, err := s.isDescendant(ctx, id, parentID)
		if err != nil {
			return nil, err
		}
		if inSubtree {
			return nil, ErrMenuParentCycle
		}
	}
	m.ParentID = parentID
	m.Name = name
	m.Path = path
	m.Icon = icon
	m.Sort = sort
	m.UpdatedAt = time.Now()
	if err := s.menus.Update(ctx, m); err != nil {
		return nil, err
	}
	return toMenuNode(m), nil
}

// Delete 删除菜单，存在子菜单时拒绝
func (s *AdminMenuService) Delete(ctx context.Context, id string) error {
	if _, err := s.menus.FindByID(ctx, id); err != nil {
		return err
	}
	hasChildren, err := s.menus.HasChildren(ctx, id)
	if err != nil {
		return err
	}
	if hasChildren {
		return ErrMenuHasChildren
	}
	return s.menus.Delete(ctx, id)
}

// RoleMenuIDs 返回角色实际绑定的菜单 ID。
// 超级管理员的侧栏全量可见由 TreeForIdentity 保证，这里只回显真实绑定，
// 避免「分配菜单」弹窗回显与保存结果不一致。
func (s *AdminMenuService) RoleMenuIDs(ctx context.Context, roleID string) ([]string, error) {
	if _, err := s.roles.FindByID(ctx, roleID); err != nil {
		return nil, err
	}
	ids, err := s.menus.FindMenuIDsByRoleID(ctx, roleID)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// AssignMenusToRole 全量覆盖角色的菜单绑定
func (s *AdminMenuService) AssignMenusToRole(ctx context.Context, roleID string, menuIDs []string) error {
	if _, err := s.roles.FindByID(ctx, roleID); err != nil {
		return err
	}
	return s.menus.AssignToRole(ctx, roleID, menuIDs)
}

// isDescendant 判断 candidateID 是否位于 rootID 的子树中。
//
// visited 是兜环的，不能省：admin_menus.parent_id 是自引用外键（见 migrations/identity），
// 只声明了 ON DELETE CASCADE，**不拦成环**，表上也没有 CHECK 或触发器。走 API 造不出环
// （本函数就是那道闸之一），但 SQL 直改能——手工修菜单在这套系统里是常规操作。
// 没有 visited 时，只要 rootID 落在环上，这里就永远转不完：那一次「修改菜单」会挂住不放，
// 占着 goroutine 和连接池连接，而且日志里什么都不留。
func (s *AdminMenuService) isDescendant(ctx context.Context, rootID, candidateID string) (bool, error) {
	menus, err := s.menus.FindAll(ctx)
	if err != nil {
		return false, err
	}
	childrenOf := make(map[string][]string)
	for _, m := range menus {
		if m.ParentID != "" {
			childrenOf[m.ParentID] = append(childrenOf[m.ParentID], m.ID)
		}
	}
	stack := append([]string(nil), childrenOf[rootID]...)
	visited := make(map[string]bool, len(menus))
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == candidateID {
			return true, nil
		}
		// 命中判断在 visited 之前：被压过两次的节点照样要比一次，只是不再展开第二遍，
		// 于是「可达」的语义与成环前完全一致。
		if visited[cur] {
			continue
		}
		visited[cur] = true
		stack = append(stack, childrenOf[cur]...)
	}
	return false, nil
}

// buildMenuTree 按 parentID 组树；filter 非空时只保留通过的节点
func buildMenuTree(menus []*model.Menu, filter func(id string) bool) []*MenuNode {
	nodes := make(map[string]*MenuNode, len(menus))
	for _, m := range menus {
		if filter != nil && !filter(m.ID) {
			continue
		}
		nodes[m.ID] = toMenuNode(m)
	}
	var roots []*MenuNode
	for _, m := range menus {
		node, ok := nodes[m.ID]
		if !ok {
			continue
		}
		if parent, ok := nodes[m.ParentID]; ok && m.ParentID != "" {
			parent.Children = append(parent.Children, node)
		} else {
			roots = append(roots, node)
		}
	}
	sortMenuNodes(roots)
	if roots == nil {
		roots = []*MenuNode{}
	}
	return roots
}

func sortMenuNodes(nodes []*MenuNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Sort != nodes[j].Sort {
			return nodes[i].Sort < nodes[j].Sort
		}
		return nodes[i].Name < nodes[j].Name
	})
	for _, n := range nodes {
		if len(n.Children) > 0 {
			sortMenuNodes(n.Children)
		}
	}
}

func toMenuNode(m *model.Menu) *MenuNode {
	return &MenuNode{
		ID:       m.ID,
		ParentID: m.ParentID,
		Name:     m.Name,
		Path:     m.Path,
		Icon:     m.Icon,
		Sort:     m.Sort,
	}
}
