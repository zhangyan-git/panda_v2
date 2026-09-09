package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	var id, username, hash, status string
	err = pool.QueryRow(context.Background(),
		`SELECT id, username, password_hash, status FROM admin_users WHERE username = 'admin' LIMIT 1`,
	).Scan(&id, &username, &hash, &status)
	if err != nil {
		log.Fatalf("query: %v", err)
	}

	fmt.Printf("id=%s username=%s status=%s\n", id, username, status)
	fmt.Printf("hash=%s\n", hash)

	for _, pw := range []string{"admin123", "admin"} {
		err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw))
		if err == nil {
			fmt.Printf("✓ password '%s' matches\n", pw)
		} else {
			fmt.Printf("✗ password '%s' does not match: %v\n", pw, err)
		}
	}
}
