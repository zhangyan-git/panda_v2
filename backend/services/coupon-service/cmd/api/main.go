package main

import "github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/app"

func main() {
	if err := app.Run("coupon-service"); err != nil {
		panic(err)
	}
}
