package main

import "github.com/panda-dev/panda-v2/backend/services/user-service/internal/app"

func main() {
	if err := app.Run("user-service"); err != nil {
		panic(err)
	}
}
