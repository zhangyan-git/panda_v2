package app

import "github.com/panda-dev/panda-v2/backend/platform/server"

func Run(service string) error {
	return server.Run(service)
}
