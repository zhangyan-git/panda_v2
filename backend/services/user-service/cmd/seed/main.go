package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL not set")
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
	permissions := []struct{ code, resource, action, group, name string }{
		{"admin:users:read", "admin_users", "read", "admin-users", "查看管理员"},
		{"admin:users:write", "admin_users", "write", "admin-users", "管理管理员"},
		{"admin:roles:read", "admin_roles", "read", "roles", "查看角色"},
		{"admin:roles:write", "admin_roles", "write", "roles", "管理角色"},
		{"admin:roles:delete", "admin_roles", "delete", "roles", "删除角色"},
		{"admin:permissions:read", "admin_permissions", "read", "permissions", "查看权限"},
		{"admin:permissions:write", "admin_permissions", "write", "permissions", "管理权限"},
		{"admin:permissions:delete", "admin_permissions", "delete", "permissions", "删除权限"},
		{"admin:bindings:read", "admin_bindings", "read", "bindings", "查看绑定"},
		{"admin:bindings:write", "admin_bindings", "write", "bindings", "管理绑定"},
	}
	var permIDs []string
	for _, p := range permissions {
		var permID string
		err = pool.QueryRow(ctx, `SELECT id FROM admin_permissions WHERE code = $1`, p.code).Scan(&permID)
		if err != nil {
			permID = uuid.NewString()
			_, err = pool.Exec(ctx, `
				INSERT INTO admin_permissions (id, code, resource, action, perm_group, name, description, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				permID, p.code, p.resource, p.action, p.group, p.name, "", now,
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
