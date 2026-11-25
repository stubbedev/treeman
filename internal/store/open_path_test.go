package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenUsesTheGivenPathVerbatim(t *testing.T) {
	for _, name := range []string{"plain", "has#hash", "has?query", "has%25percent"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), name, "treeman.db")
			st, err := Open(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("store must be created at %s: %v", p, err)
			}
		})
	}
}
