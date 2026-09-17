package daemonctl

import (
	"reflect"
	"testing"
)

func TestDaemonEnvironmentExcludesInvocationConfig(t *testing.T) {
	input := []string{"PATH=/bin", "TREEMAN_CONFIG=relative.yaml", "TREEMAN_CONFIG_OTHER=keep", "HOME=/home/test", "TREEMAN_CONFIG="}
	want := []string{"PATH=/bin", "TREEMAN_CONFIG_OTHER=keep", "HOME=/home/test"}
	if got := daemonEnvironment(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %v, want %v", got, want)
	}
	if len(input) != 5 || input[1] != "TREEMAN_CONFIG=relative.yaml" {
		t.Fatal("parent environment mutated")
	}
}
