package main

import (
	"context"
	"fmt"

	"github.com/phuongdpham/fintech/libs/go/logger"
)

func main() {
	ctx := context.Background()
	log := logger.New("compliance-cli")
	log.Info(ctx, "compliance-cli starting")
	fmt.Println("compliance-cli up")
}
