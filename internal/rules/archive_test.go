package rules

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The verbatim archiver is a fallback for ONE failure: a lock this build cannot
// PARSE. Everything else -- a path that is both the archive and the lock, a
// lock that cannot be read at all -- is a failure of the ARCHIVE, and answering
// it by copying bytes would report an archive that is not there.
func TestOnlyAnUnparseableLockIsReportedAsErrLockUnreadable(t *testing.T) {
	dir := t.TempDir()
	v1 := []byte(`{"schema_version":1,"repository":"v2fly/domain-list-community","commit":"` +
		strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) +
		`","list_sha256":"` + strings.Repeat("e", 64) + `","entry":"data/cn"}`)
	current := filepath.Join(dir, "source-lock.json")
	if err := os.WriteFile(current, v1, 0o640); err != nil {
		t.Fatal(err)
	}

	// The document failure: the fallback must be able to see this one.
	if err := Archive(filepath.Join(dir, "previous.json"), current); !errors.Is(err, ErrLockUnreadable) {
		t.Errorf("a lock this build cannot parse is not reported as ErrLockUnreadable, so a caller "+
			"cannot fall back to copying it: %v", err)
	}

	// The operation failures: a caller must NOT be able to fall back on these,
	// or a failed archive would be reported as a published one.
	same := filepath.Join(dir, "same.json")
	if err := Archive(same, same); errors.Is(err, ErrLockUnreadable) {
		t.Error("a path that is both the archive and the lock is reported as an unreadable lock, so a " +
			"caller would fall back to copying a file onto itself")
	}
	if err := Archive("", current); errors.Is(err, ErrLockUnreadable) {
		t.Error("an empty archive path is reported as an unreadable lock")
	}
	if err := Archive(filepath.Join(dir, "previous.json"), ""); errors.Is(err, ErrLockUnreadable) {
		t.Error("an empty lock path is reported as an unreadable lock")
	}

	// And a lock that PARSES but is not VALID is archived and re-encoded, not
	// reported: the split is about the document, not about whether the document
	// is correct. Archive does not call Validate, and refusing an invalid lock
	// here would mean a machine that published one could not roll back from it.
	//
	// The fixture has to actually fail Validate, or it proves nothing -- and the
	// obvious way to make one (a lock with a bad digest) is refused by
	// ParseSourceLock's caller rather than by Archive, so it would test nothing
	// either. `kinds` on a primary parses and is refused by validateRef, which is
	// the shape this wants.
	invalid := SourceLock{
		SchemaVersion: schemaVersion,
		Primary: SourceRef{
			Repository: RepositoryV2Fly,
			Commit:     strings.Repeat("a", 40),
			SHA256:     strings.Repeat("b", 64),
			Entry:      V2FlyEntry,
			Kinds:      []string{"full"}, // a supplement field on a primary
		},
		ListSHA256: strings.Repeat("e", 64),
	}
	if err := invalid.Validate(); err == nil {
		t.Fatal("the fixture is a valid lock, so it cannot show that validity is not what Archive keys on")
	}
	encoded, err := invalid.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSourceLock(encoded); err != nil {
		t.Fatalf("the fixture does not parse, so it is testing the other case: %v", err)
	}
	if err := os.WriteFile(current, encoded, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Archive(filepath.Join(dir, "previous.json"), current); err != nil {
		t.Errorf("a lock that parses but does not validate is not archived, so a machine that published "+
			"one could not roll back from it: %v", err)
	} else if errors.Is(err, ErrLockUnreadable) {
		t.Error("a lock that parses was reported as unreadable")
	}
}

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
