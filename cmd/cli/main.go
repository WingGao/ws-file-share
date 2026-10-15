package main

import (
	"fmt"
	"os"

	"wsdownloader/internal/command"
)

func main() {
	if err := command.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "WS下载器 CLI:", err)
		os.Exit(1)
	}
}
