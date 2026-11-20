// Command hello creates a sandbox, runs one command, prints its output, and
// deletes the sandbox.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	prized "github.com/ijaidev/prized-go"
)

func main() {
	options := []prized.Option{}
	if baseURL := os.Getenv("PRIZED_BASE_URL"); baseURL != "" {
		options = append(options, prized.WithBaseURL(baseURL))
	}
	client, err := prized.NewClient(options...)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sandbox, err := client.Sandboxes().Create(ctx, prized.CreateSandboxOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := sandbox.Delete(context.Background(), nil); err != nil {
			log.Printf("delete sandbox %s: %v", sandbox.ID(), err)
		}
	}()

	result, err := sandbox.Exec(ctx, `python3 -c "print(6 * 7)"`, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(result.Stdout)
}
