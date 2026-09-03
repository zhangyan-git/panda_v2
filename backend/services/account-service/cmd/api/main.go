package main

import "github.com/panda-dev/panda-v2/backend/services/account-service/internal/app"

func main() {
	if err := app.Run("account-service"); err != nil {
		panic(err)
	}
}
