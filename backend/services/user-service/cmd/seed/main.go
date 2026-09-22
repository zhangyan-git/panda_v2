package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	requireDevEnvironment()

	// 种子数据全部属于身份域，拆库后落在身份库上。USER_DATABASE_URL 是那个库，
	// 未配置时回落 DATABASE_URL（单库栈）。
	dbURL := os.Getenv("USER_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		log.Fatal("USER_DATABASE_URL (or DATABASE_URL) not set")
	}

	username := os.Getenv("DEV_ADMIN_USERNAME")
	password := os.Getenv("DEV_ADMIN_PASSWORD")
	if username == "" {
		username = "admin"
	}
	if password == "" {
		password = "admin123"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt: %v", err)
	}

	now := time.Now()

	// 1. upsert admin user, get back id
	var adminUserID string
	err = pool.QueryRow(ctx, `
		INSERT INTO admin_users (id, username, password_hash, name, email, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'active', $6, $7)
		ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash, status = 'active'
		RETURNING id`,
		uuid.NewString(), username, string(hash),
		"超级管理员", "admin@panda.dev",
		now, now,
	).Scan(&adminUserID)
	if err != nil {
		log.Fatalf("upsert admin user: %v", err)
	}
	fmt.Printf("✓ admin user '%s' id=%s\n", username, adminUserID)

	// 2. get or create super_admin role (code + name are the natural key)
	var roleID string
	err = pool.QueryRow(ctx, `SELECT id FROM admin_roles WHERE code = $1`, "super_admin").Scan(&roleID)
	if err != nil {
		roleID = uuid.NewString()
		_, err = pool.Exec(ctx, `
			INSERT INTO admin_roles (id, code, name, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			roleID, "super_admin", "超级管理员", "拥有全部权限", now, now,
		)
		if err != nil {
			log.Fatalf("insert role: %v", err)
		}
	}
	fmt.Printf("✓ role 'super_admin' id=%s\n", roleID)

	// 3. upsert permissions
	permissions := []struct{ code, group, name string }{
		{"admin:users:view", "用户管理", "查看管理员"},
		{"admin:users:manage", "用户管理", "管理管理员"},
		{"admin:roles:view", "角色管理", "查看角色"},
		{"admin:roles:manage", "角色管理", "管理角色"},
		{"admin:roles:delete", "角色管理", "删除角色"},
		{"admin:permissions:view", "权限管理", "查看权限"},
		{"admin:permissions:manage", "权限管理", "管理权限"},
		{"admin:permissions:delete", "权限管理", "删除权限"},
		{"admin:bindings:view", "绑定管理", "查看绑定"},
		{"admin:bindings:manage", "绑定管理", "管理绑定"},
	}
	var permIDs []string
	for _, p := range permissions {
		var permID string
		err = pool.QueryRow(ctx, `SELECT id FROM admin_permissions WHERE code = $1`, p.code).Scan(&permID)
		if err != nil {
			permID = uuid.NewString()
			_, err = pool.Exec(ctx, `
				INSERT INTO admin_permissions (id, code, perm_group, name, description, created_at)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				permID, p.code, p.group, p.name, "", now,
			)
			if err != nil {
				log.Fatalf("insert permission %s: %v", p.code, err)
			}
		}
		permIDs = append(permIDs, permID)
		fmt.Printf("✓ permission '%s' id=%s\n", p.code, permID)
	}

	// 4. bind permissions to role
	for _, permID := range permIDs {
		_, err = pool.Exec(ctx, `
			INSERT INTO admin_role_permissions (role_id, permission_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING`,
			roleID, permID,
		)
		if err != nil {
			log.Fatalf("bind permission to role: %v", err)
		}
	}
	fmt.Printf("✓ bound %d permissions to super_admin\n", len(permIDs))

	// 5. bind role to admin user
	_, err = pool.Exec(ctx, `
		INSERT INTO admin_user_role_bindings (admin_user_id, role_id, granted_at)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`,
		adminUserID, roleID, now,
	)
	if err != nil {
		log.Fatalf("bind role to user: %v", err)
	}
	fmt.Println("✓ bound super_admin role to admin user")
}

// requireDevEnvironment 是这个程序唯一的护栏，而它挡的是一件事：**把生产库的
// 超级管理员密码重置成 admin123**。
//
// 看代码看不出来危险：这里每一句都是 upsert 与 ON CONFLICT DO NOTHING，没有一句
// DELETE。但第一句就是 `ON CONFLICT (username) DO UPDATE SET password_hash =
// EXCLUDED.password_hash`，配上 DEV_ADMIN_PASSWORD 缺席时的那个默认值——在一个
// 生产库上跑一次，等于把超管口令改成一个人人皆知的字符串，还把 status 拉回 active。
// 而它连的是 USER_DATABASE_URL（或 DATABASE_URL）指向的任何一个库——这个程序自己
// 区分不出环境。
//
// 判据取 PANDA_ENV（不是「库地址里有没有 localhost」那种猜测）：它是**显式**写在
// 环境里的一句话，这个程序在别处也从不读 .env，所以拿到的一定是操作者当下导出的值。
// 没设、或者设成别的（production、staging、pre）一律拒绝——默认拒绝，不是默认放行。
func requireDevEnvironment() {
	env := strings.TrimSpace(os.Getenv("PANDA_ENV"))
	if env == "dev" {
		return
	}
	if env == "" {
		log.Fatal("PANDA_ENV is not set; this program rewrites the admin password and grants super_admin. " +
			"Set PANDA_ENV=dev explicitly if that is really what you mean")
	}
	log.Fatalf("refusing to seed: PANDA_ENV=%q. This program rewrites the admin password and grants super_admin, "+
		"so it only runs against a dev environment", env)
}
