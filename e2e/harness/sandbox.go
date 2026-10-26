//go:build e2e

package harness

import (
	"fmt"
	"os"
)

func init() {
	if os.Getenv("TREEMAN_E2E_SANDBOX") != "1" {
		fmt.Fprintln(os.Stderr, "e2e tests run only inside the throwaway sandbox container; use `just test-e2e`")
		os.Exit(2)
	}
}
