package main

import (
	"fmt"
	"github.com/awked-com/infra/ci/worker"
	"os"
	"syscall"
)

func main() {
	syscall.Umask(0077)
	if e := worker.SourceMain(); e != nil {
		fmt.Fprintln(os.Stderr, "Source retrieval failed.")
		os.Exit(1)
	}
}
