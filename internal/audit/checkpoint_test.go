package audit

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestDeletionCheckpointPrunedOnlyAfterAck(t *testing.T) {
	s, err := Open(t.TempDir(), "test", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Observe("uid", "deleted", []Event{{Action: "organization.deleted"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(0); err != nil || s.Seen("uid") == "" {
		t.Fatal("premature pruning", err)
	}
	size, cap := s.StateSize()
	if size <= 0 || cap != 1<<20 {
		t.Fatal("state metrics")
	}
	s.max = 1 // Lowering the configured cap must not prevent draining old state.
	if err = s.Ack(1); err != nil || s.Seen("uid") != "" {
		t.Fatal("ack blocked or tombstone retained", err)
	}
}
func TestDirectorySyncFailureExits(t *testing.T) {
	if os.Getenv("AUDIT_TEST_FSYNC") == "1" {
		s, err := Open(os.Getenv("AUDIT_TEST_DIR"), "test", 1<<20)
		if err != nil {
			panic(err)
		}
		syncDirectory = func(string) error { return errors.New("injected fsync failure") }
		_ = s.Append([]Event{{Action: "test"}})
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDirectorySyncFailureExits$")
	cmd.Env = append(os.Environ(), "AUDIT_TEST_FSYNC=1", "AUDIT_TEST_DIR="+t.TempDir())
	if err := cmd.Run(); err == nil {
		t.Fatal("process survived directory fsync failure")
	}
}
