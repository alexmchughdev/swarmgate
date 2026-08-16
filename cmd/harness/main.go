// Command harness drives evaluation scenarios against a running swarmgate instance.
package main

import (
	"fmt"
	"os"
)

// version is overwritten at build time via -ldflags "-X main.version=...";
// see the Makefile's release target.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		os.Exit(0)
	}
	os.Exit(dispatch(os.Args[1:]))
}
