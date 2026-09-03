package main

import "github.com/panda-dev/panda-v2/backend/services/settlement-service/internal/app"

func main() {
	if err := app.Run("settlement-service"); err != nil {
		panic(err)
	}
}
