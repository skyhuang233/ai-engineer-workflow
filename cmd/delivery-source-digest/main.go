package main

import (
	"context"
	"fmt"
	"os"

	"github.com/skyhuang233/workflow/internal/deliverysource"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: delivery-source-digest <bare-repository>")
		os.Exit(2)
	}
	digest, err := deliverysource.Digest(context.Background(), os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(digest)
}
