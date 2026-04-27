package main

import (
	"os"

	"github.com/mordilloSan/indexer/internal/cli"
)

func main() {
	cli.Main(os.Args[1:])
}
