package main

import (
	"fmt"
	"os"

	"server/internal/httpapi"
)

func main() {
	spec, err := httpapi.OpenAPISpec()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
