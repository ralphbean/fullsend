package cli

import (
	"io"
	"strings"
	"testing"
)

func TestK8sPoCRecoveryCommandRequiresRunID(t *testing.T) {
	for _, args := range [][]string{{"retry"}, {"dismiss"}, {"retry", "invalid"}, {"dismiss", "invalid"}} {
		cmd := newK8sPoCCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
}

func TestK8sPoCCommandsRequireConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, args := range [][]string{{"migrate"}, {"dispatch"}, {"status"}, {"retry", "1"}, {"dismiss", "1"}} {
		cmd := newK8sPoCCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
			t.Fatalf("%v: expected missing database configuration, got %v", args, err)
		}
	}
	cmd := newK8sPoCCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"poll-fixture", "--fixture", "missing.json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("poll-fixture accepted a missing fixture")
	}
}
