package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kpraveenkumar19/mini-redis/app"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:6379", "Address to listen on")
	dir := flag.String("dir", "", "RDB directory path")
	dbfilename := flag.String("dbfilename", "", "RDB file name")
	flag.Parse()

	if err := app.RunServer(*addr, *dir, *dbfilename); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
