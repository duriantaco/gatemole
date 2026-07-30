package main

import (
	"os"

	"github.com/duriantaco/gatemole/internal/gatemole"
)

func main() {
	os.Exit(gatemole.Main(os.Args[1:], os.Stdout, os.Stderr))
}
