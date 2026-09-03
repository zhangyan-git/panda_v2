package main

import (
	"context"
	"log"

	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/app"
)

func main() {
	cfg, err := config.Load("user-service")
	if err != nil {
		log.Fatal(err)
	}
	db, err := database.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := app.RunWithRepository(cfg, db); err != nil {
		log.Fatal(err)
	}
}
