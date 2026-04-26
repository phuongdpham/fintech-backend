package main

import (
	"context"
	"fmt"

	"github.com/phuongdpham/fintech/libs/go/logger"
)

func main() {
	ctx := context.Background()
	log := logger.New("gateway-svc")
	log.Info(ctx, "gateway-svc booting")
	fmt.Println("gateway-svc up")
}
