package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kpraveenkumar19/mini-redis/app"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "Server address")
	flag.Parse()

	if err := app.RunCLI(*addr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
