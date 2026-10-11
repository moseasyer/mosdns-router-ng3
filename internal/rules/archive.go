package rules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Archive copies the currently published lock to previousLockPath.
//
// It is written BEFORE a publication and replaced BY it, and that order is the whole
// reason it exists. The moment the new lock is renamed into place the old one is
// gone, and with it the only record of what this machine was running; an archive
// taken after the publication records the commit that has just been accepted, which
// is not a rollback target for anybody. Taken before, a publication that fails leaves
// the archive naming a commit that really was current -- which is the one an operator
// recovering from a failed unattended refresh needs.
//
// The archive is written the way a lock is published -- staged file, fsync, rename,
// fsync of the directory -- rather than by a plain write. An archive truncated by a
// power cut is an archive that parses to nothing, and an unparseable rollback target
// is worse than a missing one because the refusal names a file the operator believes
// exists.
func Archive(previousLockPath, currentLockPath string) error {
	if previousLockPath == "" || currentLockPath == "" {
		return fmt.Errorf("rules: an archive path and a current lock path are both required")
	}
	if previousLockPath == currentLockPath {
		return fmt.Errorf("rules: %s is both the archive and the lock it archives, so archiving would destroy it", currentLockPath)
	}
	encoded, err := os.ReadFile(currentLockPath)
	if err != nil {
		// A lock that is not there is nothing to archive, and that is the FIRST pin
		// rather than a failure: refusing would make installing from nothing
		// impossible in order to protect a record that does not exist. Anything other
		// than absence is a refusal, because it means an outgoing pin this package
		// cannot copy -- and publishing over it destroys the only copy there is.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rules: read the lock being archived: %w", err)
	}
	current, err := ParseSourceLock(encoded)
	if err != nil {
		// The one failure that is about the DOCUMENT rather than the filesystem or
		// the operation, and the only one a caller may work around: the lock is a
		// real file this build cannot read, and copying it needs neither the parse
		// nor anything that could have failed here. Everything after this point is
		// a failure of the archive itself, and a caller that fell back to copying
		// on those would be reporting success for an archive that is not there.
		//
		// Joined rather than replaced, so the CAUSE survives alongside the
		// sentinel: a caller that copies the bytes wants to tell a schema version
		// 1 document from a truncated file, and that distinction is the whole
		// difference between a migration and something gone wrong.
		return fmt.Errorf("rules: %s is not a lock this package can archive: %w",
			currentLockPath, errors.Join(err, ErrLockUnreadable))
	}
	// Re-encoded rather than the bytes copied. A lock that was hand-edited and still
	// parses would otherwise be archived verbatim, and the archive would be the one
	// file in the pair that is not what this package writes.
	canonical, err := current.Encode()
	if err != nil {
		return fmt.Errorf("rules: encode the lock being archived: %w", err)
	}
	return writeLockAtomically(previousLockPath, canonical, defaultFileOps())
}

// ErrLockUnreadable is Archive's one failure that is about the lock as a
// DOCUMENT: the file is there and readable, and it is not a lock this build can
// parse.
//
// It is named so that a caller can tell it from a failure of the archive -- a
// disk that is full, a directory that cannot be synced, a path that is both the
// archive and the lock. Only this one may be answered by copying the bytes
// instead, because only this one leaves a record that already exists.
var ErrLockUnreadable = errors.New("the published lock is not a lock this package can read")

// ArchiveVerbatim copies the currently published lock to previousLockPath
// WITHOUT reading it as a lock: the bytes on disk are what is archived, and a
// lock this build cannot parse is still the only record of what the machine was
// running.
//
// It exists for one caller, and the reason is a deadlock rather than a
// convenience. A published schema version 1 lock is refused by ParseSourceLock,
// and that refusal names `update-lists --pin-remote` as the way out. Archive
// refuses a lock it cannot parse too -- correctly, because an archive that
// parses to nothing is worse than no archive, since the refusal then names a
// file the operator believes exists. Together they mean the command the refusal
// points at cannot run, and the only remaining recovery is a human editing a
// file under /var/lib.
//
// Archive keeps refusing; this is the alternative an operator reaches for
// deliberately, having named the source to publish. The copy is written the way
// Archive writes one -- staged, fsync, rename, fsync of the directory -- so a
// power cut leaves no half-written rollback target, and a missing lock is still
// a first install rather than an error.
func ArchiveVerbatim(previousLockPath, currentLockPath string) error {
	if previousLockPath == "" || currentLockPath == "" {
		return errors.New("rules: an archive path and a current lock path are both required")
	}
	if previousLockPath == currentLockPath {
		return fmt.Errorf("rules: %s is both the archive and the lock it archives, so archiving would destroy it", currentLockPath)
	}
	encoded, err := os.ReadFile(currentLockPath)
	if err != nil {
		// Absence is the first pin and archives nothing, which is the same
		// answer Archive gives. Anything else means the outgoing pin cannot be
		// copied at all, and publishing over it would destroy the only copy.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rules: read the lock being archived: %w", err)
	}
	return writeLockAtomically(previousLockPath, encoded, defaultFileOps())
}

// ReadPrevious reads the archived pin.
//
// A missing archive is an error rather than an empty lock, and that is deliberate: a
// rollback that silently continued with nothing to roll back to would install a commit
// nobody reviewed, which is the outcome the archive exists to make impossible. The
// caller that does not need it -- --pin-remote with an explicit commit -- never calls
// this.
func ReadPrevious(previousLockPath string) (SourceLock, error) {
	if previousLockPath == "" {
		return SourceLock{}, fmt.Errorf("rules: an archive path is required")
	}
	encoded, err := os.ReadFile(previousLockPath)
	if err != nil {
		return SourceLock{}, fmt.Errorf("rules: read the archived pin: %w", err)
	}
	previous, err := ParseSourceLock(encoded)
	if err != nil {
		return SourceLock{}, fmt.Errorf("rules: %s is not an archived pin this package can read: %w", previousLockPath, err)
	}
	return previous, nil
}

// writeLockAtomically stages a lock beside its target and renames it into place, the
// same way a published lock is written.
//
// It is separate from the publication machinery rather than folded into it because a
// publication restores what it replaced on failure and this writes a second file that
// has nothing to do with the pair: sharing the backup path would make an archive
// failure roll a working lock back to its predecessor, which is a way to lose two
// commits instead of one.
func writeLockAtomically(target string, contents []byte, ops fileOps) (err error) {
	staged, err := stageFile(target, contents, ops)
	if err != nil {
		return err
	}
	defer staged.discard(&err, ops)
	if err := ops.rename(staged.path, target); err != nil {
		return fmt.Errorf("rules: publish %s: %w", target, err)
	}
	if err := ops.syncDir(filepath.Dir(target)); err != nil {
		return fmt.Errorf("rules: sync %s: %w", filepath.Dir(target), err)
	}
	return nil
}
