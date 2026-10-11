package rules

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchiveVerbatimKeepsALockThisBuildCannotRead pins the property the
// migration depends on: a schema version 1 lock is refused by everything else in
// this package, and the archive is the one place that has to keep it anyway.
//
// A record this build cannot read is still a record. Losing it would leave a
// migration with no way back at all.
func TestArchiveVerbatimKeepsALockThisBuildCannotRead(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "source-lock.json")
	previous := filepath.Join(dir, "source-lock.previous.json")
	v1 := []byte(`{"schema_version":1,"repository":"v2fly/domain-list-community","commit":"` +
		strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) +
		`","list_sha256":"` + strings.Repeat("e", 64) + `","entry":"data/cn"}`)
	if _, err := ParseSourceLock(v1); err == nil {
		t.Fatal("the fixture is readable as a v2 lock, so it proves nothing")
	}
	if err := os.WriteFile(current, v1, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := ArchiveVerbatim(previous, current); err != nil {
		t.Fatalf("ArchiveVerbatim refused a lock it was asked to keep: %v", err)
	}
	archived, err := os.ReadFile(previous)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}
	if !bytes.Equal(archived, v1) {
		t.Fatalf("the archive is\n%s\nwhich is not the lock that was published\n%s", archived, v1)
	}
	// The outgoing lock is untouched: archiving is a copy, and this one runs
	// before a publish that replaces the original.
	if onDisk, err := os.ReadFile(current); err != nil || !bytes.Equal(onDisk, v1) {
		t.Fatalf("the published lock changed: %q (%v)", onDisk, err)
	}
}

// A first install has no outgoing lock, and that is not a failure here either:
// it is the same answer Archive gives, and refusing would make installing from
// nothing impossible in order to protect a record that does not exist.
func TestArchiveVerbatimTreatsAMissingLockAsAFirstInstall(t *testing.T) {
	dir := t.TempDir()
	if err := ArchiveVerbatim(filepath.Join(dir, "previous.json"), filepath.Join(dir, "absent.json")); err != nil {
		t.Fatalf("ArchiveVerbatim on a machine with nothing published: %v", err)
	}
}

// A lock that cannot even be READ is a different thing from a lock that cannot
// be parsed: publishing over it would destroy the only copy, which is what the
// refusal is for.
func TestArchiveVerbatimRefusesWhenTheOutgoingLockCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "source-lock.json")
	if err := os.WriteFile(unreadable, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a file with no permission bits")
	}
	if err := ArchiveVerbatim(filepath.Join(dir, "previous.json"), unreadable); err == nil {
		t.Fatal("ArchiveVerbatim reported success for a lock it could not read")
	}
}

func TestArchiveRefusesTheSamePathsItAlwaysHas(t *testing.T) {
	// Archive and ArchiveVerbatim differ in whether they READ the lock, not in
	// which arguments they accept: an empty path, or one file that is both the
	// archive and the lock being archived, destroys the record in either.
	dir := t.TempDir()
	path := filepath.Join(dir, "source-lock.json")
	if err := os.WriteFile(path, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, archive := range []func(string, string) error{Archive, ArchiveVerbatim} {
		if err := archive("", path); err == nil {
			t.Errorf("%T accepted an empty archive path", archive)
		}
		if err := archive(path, ""); err == nil {
			t.Errorf("%T accepted an empty lock path", archive)
		}
		if err := archive(path, path); err == nil {
			t.Errorf("%T accepted one path as both the archive and the lock", archive)
		}
	}
}
