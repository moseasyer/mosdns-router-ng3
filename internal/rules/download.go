package rules

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	// RepositoryChina and RepositoryV2Fly are the two sources this gateway has
	// reviewed. Which of them a published list derives from is the lock's
	// decision and not a constant's: the primary is whichever the lock names, a
	// supplement can only be the v2fly one, and a fallback publication names
	// v2fly as the primary with no supplement at all.
	//
	// Neither is a parameter a caller may set to any value. Both entries of this
	// allowlist were reviewed by hand and anything outside it is refused,
	// because what a lock records is which source was reviewed -- and the
	// repository is a path under a fixed host, so an unchecked name is also a
	// way to aim the download somewhere else.
	RepositoryChina = "felixonmars/dnsmasq-china-list"
	RepositoryV2Fly = "v2fly/domain-list-community"

	// V2FlyEntry is the list inside the v2fly repository this gateway publishes:
	// the China domain set. It is one file.
	V2FlyEntry = "data/cn"

	// schemaVersion is the shape of a written lock. A lock carrying any other
	// version is refused rather than interpreted.
	schemaVersion = 2

	// apiBaseURL and archiveBaseURL are fixed GitHub endpoints. Neither the
	// repository nor the host is caller-controlled, and the repository that is
	// concatenated into them has been through KnownRepository first.
	apiBaseURL     = "https://api.github.com"
	archiveBaseURL = "https://codeload.github.com"

	// maxArchiveFileBytes bounds one file inside a source archive, and
	// maxArchiveBytes bounds the archive body and its total uncompressed
	// content. A reviewed source archive is a few hundred kilobytes; these are
	// envelopes a hostile origin cannot make the pin buffer past.
	maxArchiveFileBytes = 4 << 20
	maxArchiveBytes     = 64 << 20

	// maxArchiveFiles bounds the entries of a source archive, so an archive of
	// tiny entries cannot make extraction do unbounded work.
	maxArchiveFiles = 32768

	// maxCommitDocumentBytes bounds the commit document this package reads one
	// field out of. A commit with a long file list is a few kilobytes.
	maxCommitDocumentBytes = 1 << 20
)

// V2FlySupplementKinds are the v2fly rule kinds merged into a primary source
// when the primary is the dnsmasq list.
//
// Only those two, because they are the ones that source cannot say at all:
// `server=/<domain>/` means the domain and everything under it, which is what a
// `domain:` rule already means, so the syntax has no room for an exact match or
// for a regular expression. A kind the primary does carry would contribute a
// second spelling of a rule the published list already holds.
//
// It is a slice, so nothing may put it into a SourceRef directly: see
// SupplementKinds, which is the only way this package hands the set out.
var V2FlySupplementKinds = []string{"full", "regexp"}

// SupplementKinds returns a copy of the kinds a v2fly supplement may contribute.
//
// The copy is the same defence entriesFor makes, for the same reason. A lock
// that shared the backing array of a package-level slice would hand its holder a
// way to rewrite that slice in place, and the next lock built from it would
// carry whatever the previous holder wrote there. The lock is the document a
// later reader trusts, so it must not alias a mutable value.
//
// It is exported because the command that builds a supplement has to name its
// kinds somewhere, and the alternative -- the exported slice above -- hands out
// the array this function exists to keep private. Every lock in this package and
// every lock the command writes goes through here.
func SupplementKinds() []string {
	return append([]string(nil), V2FlySupplementKinds...)
}

// KnownRepository reports whether this gateway has reviewed name as a source.
//
// It is the one gate every repository name passes through before it reaches a
// URL, so it is deliberately an equality test against two constants rather than
// a shape test. A shape test would accept every repository that looks like the
// two of them, and a lock that records which source was reviewed is worth
// nothing if a well-formed name is enough to get into one.
func KnownRepository(name string) bool {
	return name == RepositoryChina || name == RepositoryV2Fly
}

// SourceRef is one reviewed upstream source.
//
// Entry and Entries are two spellings of the same question -- which files of the
// archive are converted -- because the two sources answer it differently. The
// dnsmasq repository's list is three named files and one string cannot say that;
// the v2fly repository's is one. Which spelling belongs to which repository is
// checked rather than inferred, because a lock that named both, or named the
// wrong one, would be a pin of something nobody read.
//
// Kinds narrows what a SUPPLEMENT contributes, and a primary names none: both
// are checked, and a doc comment saying so is not what enforces it.
type SourceRef struct {
	Repository string   `json:"repository"`
	Commit     string   `json:"commit"`
	SHA256     string   `json:"sha256"`
	Entry      string   `json:"entry,omitempty"`
	Entries    []string `json:"entries,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
}

// SourceLock records the reviewed upstream sources of a published list, so a
// later update can be compared against them and a reader can tell exactly which
// bytes produced the list it is running.
//
// The list derives from primary, and optionally from supplement: the dnsmasq
// source cannot express a `full:` or a `regexp:` rule, so those v2fly rules are
// merged into the same published file and the lock has to be able to say so.
// When the primary IS v2fly -- a fallback publication -- supplement is absent
// and one document shape covers both roles. That is enforced rather than
// described: v2fly already carries every kind a supplement could contribute, so
// naming one beside it would be a document untrue about how the list was built.
type SourceLock struct {
	// SchemaVersion is the shape of this document.
	SchemaVersion int `json:"schema_version"`
	// Primary is the source the published list is converted from.
	Primary SourceRef `json:"primary"`
	// Supplement is the second source, when there is one. Absent rather than
	// empty when the list comes from a single archive.
	Supplement *SourceRef `json:"supplement,omitempty"`
	// ListSHA256 is the digest of the published list bytes this lock describes.
	ListSHA256 string `json:"list_sha256"`
}

// Encode renders the lock as the exact bytes a lock file holds. The field order
// is the struct's, so the same lock always renders the same document.
func (l SourceLock) Encode() ([]byte, error) {
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(l); err != nil {
		return nil, fmt.Errorf("encode source lock: %w", err)
	}
	return rendered.Bytes(), nil
}

// ParseSourceLock decodes a lock document. An unknown field, a second document
// and any malformed JSON are refused: a lock is the record of what was reviewed,
// so a field this build does not understand must not be quietly dropped while
// the document still reads as a complete pin.
//
// A document of another schema version is refused too, and with the command that
// produces one this build can read. That is a separate failure from an unknown
// field on purpose: the first is a lock written before the list had two sources,
// and the operator's next step is to re-pin rather than to edit the document.
func ParseSourceLock(data []byte) (SourceLock, error) {
	// The version is read first, from a document decoded for nothing else. A v1
	// lock's fields are not this build's fields -- `repository` and `entry` are
	// where `primary` is now -- so decoding it strictly would fail on an unknown
	// field and report a syntax problem, when what the operator has is a lock of
	// a shape this build does not read, which is a different thing with a
	// different command that fixes it.
	var version struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&version); err != nil {
		return SourceLock{}, fmt.Errorf("decode source lock: %w", err)
	}
	if version.SchemaVersion != schemaVersion {
		return SourceLock{}, fmt.Errorf("source lock is schema version %d and this build reads version %d. %s",
			version.SchemaVersion, schemaVersion, whyVersionIsRefused(version.SchemaVersion))
	}

	lock := SourceLock{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return SourceLock{}, fmt.Errorf("decode source lock: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return SourceLock{}, errors.New("decode source lock: trailing data after the document")
	}
	return lock, nil
}

// whyVersionIsRefused explains a version this build does not read, and says
// what to do about it.
//
// The advice differs by DIRECTION, which is why this is not one sentence: an
// older lock is behind this build and re-pinning moves it forward, while a newer
// one is ahead of it, and re-pinning with this build would replace a document
// that records things this one cannot with one that knows less. So a v1 lock is
// named as v1 and told the command that fixes it; anything else is told the
// version it actually is and told not to re-pin it away.
//
// The command names `--pin-source` because the plain form cannot be followed: a
// machine on a v1 lock has no readable lock to say which repository to pin, so
// `--pin-remote` alone refuses the same document this message is about. A
// refusal that names a command which cannot succeed sends the operator to run
// something and read a refusal, which is worse than naming no command at all.
func whyVersionIsRefused(version int) string {
	if version == 1 {
		return "A v1 lock names one repository and one entry; this list is published from two, " +
			"so reading the old document as the new one would make a lock that reads complete " +
			"mean something it does not say. Name the source and re-pin with " +
			"`mosdns-cdnctl update-lists --pin-remote HEAD --pin-source <repository>`, where the " +
			"repository is one of " + RepositoryV2Fly + " or " + RepositoryChina + " -- the flag is " +
			"required here because a v1 lock cannot say which repository this machine was running"
	}
	return "This build cannot account for a lock of another shape, and re-pinning with it would " +
		"replace a document that knows things this one does not. Upgrade the gateway instead of " +
		"re-pinning over it"
}

// Validate reports whether a published lock is complete. It is used when a lock
// on disk is about to be replaced or compared, never as an input filter.
func (l SourceLock) Validate() error {
	if err := l.validatePinnedSource(); err != nil {
		return err
	}
	if !isLowerHex(l.ListSHA256, 64) {
		return fmt.Errorf("list_sha256 %q is not a 64 character lower-case hex digest", l.ListSHA256)
	}
	return nil
}

// validatePinnedSource checks everything a download needs to trust an archive
// except the list digest: a lock that has not been converted yet has no list
// digest, and the download is what produces it.
//
// A supplement is checked for shape here and refused for substance by Download,
// which is the one place that knows whether it can read two archives.
func (l SourceLock) validatePinnedSource() error {
	if err := l.validateRef(&l.Primary); err != nil {
		return fmt.Errorf("primary: %w", err)
	}
	if len(l.Primary.Kinds) != 0 {
		// `kinds` is a supplement's field, so on the primary it is a claim about
		// the conversion rather than a description of the source -- and one that
		// nothing here could check, because the primary contributes every rule
		// its repository holds.
		return errors.New("primary: kinds names what a SUPPLEMENT contributes; the primary contributes everything its repository holds")
	}
	if l.Supplement != nil {
		// A supplement carries what the primary CANNOT. The dnsmasq source
		// cannot express a `full:` or a `regexp:` rule, so the v2fly rules of
		// those kinds are merged in beside it. The v2fly source is the opposite
		// case: as the primary it already carries every kind there is, so a
		// supplement beside it re-reads its own repository and the document
		// attributes rules to a second source that already held them -- and no
		// value of `kinds` could make that true.
		//
		// Checked here rather than in Download, because the fault is in the
		// document: a lock like this is untruthful however it is read, including
		// by Validate on a pair already on disk. One shape covers both roles --
		// the fallback publication names no supplement at all.
		if l.Primary.Repository != RepositoryChina {
			return fmt.Errorf("supplement: primary %q already carries every rule kind a supplement contributes; a v2fly primary is the fallback publication and has nothing to merge", l.Primary.Repository)
		}
		if l.Supplement.Repository != RepositoryV2Fly {
			return fmt.Errorf("supplement: repository %q is not the only source allowed as a supplement", l.Supplement.Repository)
		}
		if err := l.validateRef(l.Supplement); err != nil {
			return fmt.Errorf("supplement: %w", err)
		}
		if err := validateSupplementKinds(l.Supplement.Kinds); err != nil {
			return fmt.Errorf("supplement: %w", err)
		}
	}
	return nil
}

// validateSupplementKinds reports whether kinds are the ones a supplement may
// contribute.
//
// It is an allowlist rather than "some kinds are fine" because the field says
// what went into the published list from a second source. A kind the primary
// already carries makes the document untrue about how the list was built: it
// attributes `domain:` rules to v2fly when the dnsmasq source held every one of
// them, so a later reader comparing the two sources would count the same domains
// twice and read the agreement as coverage. A kind this build does not know is
// worse still: it is a rule form no converter here will ever emit, so the lock
// promises a contribution nothing can produce.
//
// Whether the primary already carries a kind is out of this function's reach --
// it depends on which repository the primary is -- so the pairing that makes the
// question unanswerable is refused in validatePinnedSource rather than asked
// here.
func validateSupplementKinds(kinds []string) error {
	if len(kinds) == 0 {
		return errors.New("a supplement records which rule kinds it contributed")
	}
	for _, kind := range kinds {
		if !slices.Contains(V2FlySupplementKinds, kind) {
			return fmt.Errorf("kind %q is not one of the kinds a supplement contributes %v", kind, V2FlySupplementKinds)
		}
	}
	return nil
}

// validateRef checks everything a source ref must satisfy apart from what it
// contributes, which only means something on one of the two roles: the primary
// contributes everything its repository holds and names no kinds, while a
// supplement contributes the kinds it names and is checked by its caller.
//
// The schema version is checked here rather than once per document because a v2
// document with a v1 version is a document this build cannot account for
// whatever else it says, and a lock with no sources at all still has to fail for
// that reason first.
func (l SourceLock) validateRef(ref *SourceRef) error {
	if l.SchemaVersion != schemaVersion {
		return fmt.Errorf("schema_version %d is not the supported version %d", l.SchemaVersion, schemaVersion)
	}
	if !KnownRepository(ref.Repository) {
		return fmt.Errorf("repository %q is not one of the reviewed sources %q and %q", ref.Repository, RepositoryChina, RepositoryV2Fly)
	}
	if !isLowerHex(ref.Commit, 40) {
		return fmt.Errorf("commit %q is not a 40 character lower-case hex commit id", ref.Commit)
	}
	if !isLowerHex(ref.SHA256, 64) {
		return fmt.Errorf("sha256 %q is not a 64 character lower-case hex digest", ref.SHA256)
	}
	switch ref.Repository {
	case RepositoryChina:
		if len(ref.Entries) == 0 {
			return errors.New("entries is empty: the dnsmasq source is three named files")
		}
		if ref.Entry != "" {
			return errors.New("entry and entries are both set on the dnsmasq source, which uses entries")
		}
		// The three files are the reviewed set rather than a hint. Upstream adds
		// files -- `bogus-nxdomain.china.conf` among them -- and a lock naming a
		// fourth would have this build convert a list no review ever covered,
		// under a lock that claims it was reviewed.
		if !sameEntries(ref.Entries, ChinaConfEntries) {
			return fmt.Errorf("entries %v are not the reviewed set %v", ref.Entries, ChinaConfEntries)
		}
	case RepositoryV2Fly:
		if ref.Entry != V2FlyEntry {
			return fmt.Errorf("entry %q is not the reviewed list %q", ref.Entry, V2FlyEntry)
		}
		if len(ref.Entries) != 0 {
			return errors.New("the v2fly source has one entry and it is named by entry, not entries")
		}
	}
	return nil
}

// sameEntries reports whether a ref names exactly the reviewed files, in any
// order. Order carries no meaning -- the converter merges the three results
// into one list -- but a file that is not in the reviewed set is a refusal
// rather than a reordering.
func sameEntries(named, reviewed []string) bool {
	if len(named) != len(reviewed) {
		return false
	}
	counts := make(map[string]int, len(named))
	for _, entry := range named {
		counts[entry]++
	}
	for _, entry := range reviewed {
		counts[entry]--
		if counts[entry] < 0 {
			return false
		}
	}
	return true
}

// entriesFor returns the entry set a repository's source is converted from, or
// nil for a repository that names one file by name instead.
//
// The set is copied: it comes from a package-level slice, and a lock that
// shared its backing array with it could be written by a caller mutating the
// constant out from under the next validation.
func entriesFor(repository string) []string {
	if repository == RepositoryChina {
		return append([]string(nil), ChinaConfEntries...)
	}
	return nil
}

// entryFor returns the single converted list of a repository that has one, and
// the empty string for the repository whose list is three named files.
func entryFor(repository string) string {
	if repository == RepositoryV2Fly {
		return V2FlyEntry
	}
	return ""
}

// ResolveCommit resolves the commit the repository publishes for ref, and returns a
// lock pinned to it with no digests: it has read a commit, not an archive.
//
// The repository is a parameter and is gated by KnownRepository rather than
// being a constant, because a lock records which source was reviewed and a
// build with one hardcoded source could not record a fallback publication at
// all. The gate is the same allowlist either way, so this widens which reviewed
// source may be pinned, not who may be pinned.
//
// An empty ref or the literal HEAD is the repository's current default-branch commit,
// read through the fixed GitHub API endpoint -- one request, so a check that finds the
// locked commit unchanged never downloads an archive at all.
//
// A full 40-character commit is that commit, and costs NO request. That is not only
// cheaper: it is what makes a rollback possible on a machine that can reach the archive
// host but not the API, which is a machine that cannot answer "what is HEAD today" and
// so cannot be told where to go back to. The commit names itself, and the archive
// fetch that follows is what refuses it if the repository has never published it.
//
// Anything else -- a branch, a tag, an abbreviated commit, HEAD~1 -- is refused here.
// A ref names a moving target or a form this package does not verify, and the lock
// records a commit: accepting one would put something in the lock that is not the
// thing the lock says it is.
func ResolveCommit(ctx context.Context, client *http.Client, repository, ref string) (SourceLock, error) {
	if !KnownRepository(repository) {
		return SourceLock{}, fmt.Errorf("repository %q is not one of the reviewed sources %q and %q", repository, RepositoryChina, RepositoryV2Fly)
	}
	if client == nil {
		return SourceLock{}, errors.New("an HTTP client is required to resolve the remote commit")
	}
	if named, ok := normalizeCommitRef(ref); ok {
		return SourceLock{
			SchemaVersion: schemaVersion,
			Primary: SourceRef{
				Repository: repository,
				Commit:     named,
				Entry:      entryFor(repository),
				Entries:    entriesFor(repository),
			},
		}, nil
	}
	endpoint := apiBaseURL + "/repos/" + repository + "/commits/HEAD"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SourceLock{}, fmt.Errorf("%s: build request: %w", endpoint, err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "mosdns-router")

	response, err := client.Do(request)
	if err != nil {
		return SourceLock{}, fmt.Errorf("%s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return SourceLock{}, fmt.Errorf("%s: unexpected status %d", endpoint, response.StatusCode)
	}
	if response.ContentLength > maxCommitDocumentBytes {
		return SourceLock{}, fmt.Errorf("%s: commit document declares %d bytes, which is larger than the %d byte limit", endpoint, response.ContentLength, maxCommitDocumentBytes)
	}
	document, err := io.ReadAll(io.LimitReader(response.Body, maxCommitDocumentBytes+1))
	if err != nil {
		return SourceLock{}, fmt.Errorf("%s: read commit document: %w", endpoint, err)
	}
	if len(document) > maxCommitDocumentBytes {
		return SourceLock{}, fmt.Errorf("%s: commit document is larger than the %d byte limit", endpoint, maxCommitDocumentBytes)
	}

	// The document carries the whole commit; only the sha is read from it.
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(document, &commit); err != nil {
		return SourceLock{}, fmt.Errorf("%s: decode commit document: %w", endpoint, err)
	}
	if !isLowerHex(commit.SHA, 40) {
		return SourceLock{}, fmt.Errorf("%s: commit document has no 40 character lower-case hex sha, got %q", endpoint, commit.SHA)
	}
	return SourceLock{
		SchemaVersion: schemaVersion,
		Primary: SourceRef{
			Repository: repository,
			Commit:     commit.SHA,
			Entry:      entryFor(repository),
			Entries:    entriesFor(repository),
		},
	}, nil
}

// ResolvePinned resolves ref and downloads that commit's source archive to compute
// the archive digest, which is what a lock has to record before the archive can be
// verified. The returned lock has no list digest: nothing has been converted yet.
//
// The digest is recorded HERE rather than left to Download so that it is the digest of
// the archive this run read, not of one a later run reads. A lock whose digest is
// computed during the download it is verifying is a lock that verifies nothing: the
// archive it is checked against is whatever arrived.
func ResolvePinned(ctx context.Context, client *http.Client, repository, ref string) (SourceLock, error) {
	resolved, err := ResolveCommit(ctx, client, repository, ref)
	if err != nil {
		return SourceLock{}, err
	}
	archive, err := fetchArchive(ctx, client, &resolved.Primary)
	if err != nil {
		return SourceLock{}, err
	}
	resolved.Primary.SHA256 = digestHex(archive)
	return resolved, nil
}

// ResolveHEAD resolves the repository's current default-branch commit and its archive
// digest. It is ResolvePinned with no ref, and it is named separately because the two
// callers mean different things by it: the drift check is asking "has upstream moved
// on from what this machine pinned", and a pin naming a commit is asking for that
// commit. Only the first is a question about now.
func ResolveHEAD(ctx context.Context, client *http.Client, repository string) (SourceLock, error) {
	return ResolvePinned(ctx, client, repository, "")
}

// normalizeCommitRef reports the commit ref names, and whether it named one at all.
//
// It repeats the shape ParseSourceLock requires of a commit rather than exporting that
// check, because that one answers a different question -- "is this string usable as a
// commit in a lock" -- and a ref that has passed it has not been validated as a ref.
func normalizeCommitRef(ref string) (string, bool) {
	trimmed := strings.TrimSpace(ref)
	if len(trimmed) != 40 {
		return "", false
	}
	for index := 0; index < len(trimmed); index++ {
		if !strings.ContainsRune("0123456789abcdef", rune(trimmed[index])) {
			return "", false
		}
	}
	return trimmed, true
}

// Download fetches the source archives a fully pinned lock names, verifies each
// digest before a byte of it is extracted, converts the pinned entries, merges
// the supplement when the lock names one, and returns the lock extended with the
// digest of the exact list bytes it produced. A failure at any step returns no
// lock and no list, so a caller cannot mistake a partial update for a pin.
//
// The repository, the archive root and the converter all come from the lock
// rather than from a package constant. A download that asked for the default
// repository would verify the pinned digest against an archive from somewhere
// else entirely, and the two would agree only while the default happened to be
// the source the lock named.
//
// The two sources are verified and converted SEPARATELY, each against its own
// ref, and merged only once both have been read: a digest is a claim about one
// archive, and a union built before either was checked would publish a list no
// single pin describes.
//
// `list_sha256` covers the merged bytes, because that is what ships. It is
// therefore not any one archive's digest, and two runs that read the same two
// commits publish the same bytes: the merge is sorted and deduplicated, so the
// list is a function of the two sets rather than of the order they arrived in.
func Download(ctx context.Context, client *http.Client, lock SourceLock) (SourceLock, []byte, error) {
	if err := lock.validatePinnedSource(); err != nil {
		return SourceLock{}, nil, fmt.Errorf("source lock: %w", err)
	}
	if client == nil {
		return SourceLock{}, nil, errors.New("an HTTP client is required to download the source archive")
	}

	fsys, err := fetchVerifiedArchive(ctx, client, &lock.Primary)
	if err != nil {
		return SourceLock{}, nil, err
	}
	expressions, comments, err := convertSource(fsys, lock.Primary)
	if err != nil {
		return SourceLock{}, nil, fmt.Errorf("convert %s at %s: %w", lock.Primary.Repository, lock.Primary.Commit, err)
	}

	if lock.Supplement != nil {
		supplementFS, err := fetchVerifiedArchive(ctx, client, lock.Supplement)
		if err != nil {
			return SourceLock{}, nil, err
		}
		// The kinds come from the lock, not from here. They are what the lock
		// says went into the published list, and this build refuses any other
		// set before a byte is fetched -- so reading them back out of
		// V2FlySupplementKinds here would publish what the document says rather
		// than what it claims, and the two drifting apart is exactly the
		// untrue-document failure validateSupplementKinds exists to prevent.
		contribution, err := ConvertFSKinds(supplementFS, lock.Supplement.Entry, lock.Supplement.Kinds)
		if err != nil {
			return SourceLock{}, nil, fmt.Errorf("convert supplement %s at %s: %w", lock.Supplement.Repository, lock.Supplement.Commit, err)
		}
		expressions = mergeExpressions(expressions, contribution)
	}

	list := renderList(expressions, comments)
	pinned := lock
	pinned.ListSHA256 = digestHex(list)
	return pinned, list, nil
}

// fetchVerifiedArchive downloads one source's archive and refuses it unless its
// digest is the one that source's own ref records. The digest is verified
// BEFORE extraction, so an archive that does not match is never unpacked -- which
// is the same guarantee for the supplement that it has always been for the
// primary, and the reason the two cannot share one code path that checked only
// the lock's primary.
func fetchVerifiedArchive(ctx context.Context, client *http.Client, ref *SourceRef) (fs.FS, error) {
	archive, err := fetchArchive(ctx, client, ref)
	if err != nil {
		return nil, err
	}
	if actual := digestHex(archive); actual != ref.SHA256 {
		return nil, fmt.Errorf("%s archive sha256 %s does not match the pinned %s", ref.Repository, actual, ref.SHA256)
	}
	return extractArchive(archive, ref.Repository, ref.Commit)
}

// convertSource converts the entries a reviewed source names, in the shape that
// source has, and returns the upstream's own record of what it disabled
// alongside them.
//
// It dispatches on the repository rather than on the entry, because the two
// reviewed sources are different formats and nothing else in a lock says which
// parser a file is written for. Dispatching on the entry would mean letting a
// lock field choose the code that reads it, and a lock is the one file this
// package refuses to take anything on trust.
func convertSource(fsys fs.FS, ref SourceRef) (expressions, comments []string, err error) {
	switch ref.Repository {
	case RepositoryChina:
		result, err := ConvertChinaConfFS(fsys, ref.Entries)
		if err != nil {
			return nil, nil, err
		}
		// The comments come back beside the expressions rather than in front of
		// them because they are not rules: renderList writes them as a block
		// above the list, which keeps the whole document sorted without the
		// expressions having to be sorted around them.
		//
		// They are in the list rather than beside it because the list is the only
		// artifact that ships: a lock has no field for them, and dropping them
		// would drop the record of which domains upstream turned off and why.
		return result.Expressions, result.Comments, nil
	case RepositoryV2Fly:
		// No filter: a primary contributes everything its repository holds. Only
		// a supplement narrows what it contributes, and a v2fly primary is the
		// fallback publication, where there is nothing to merge it with.
		converted, err := ConvertFS(fsys, ref.Entry)
		if err != nil {
			return nil, nil, err
		}
		return converted, nil, nil
	}
	return nil, nil, fmt.Errorf("repository %q is not a reviewed source this build can convert", ref.Repository)
}

// renderList renders converted expressions and the upstream's disabled-entry
// reasons as the exact bytes a list file holds.
//
// The comments come first so that a reader opening the file sees what upstream
// turned off before seeing what is on. They sort before every expression ('#'
// precedes any letter), so the document stays sorted whole, and they are inert
// to the matcher: MOSDNS truncates every line at the first '#', leaving
// nothing on the line, and skips what is left empty.
//
// A comment line is not a rule. `validateRenderedRule` -- the gate the
// committed list is held to -- does not accept a '#' at all, so a list carrying
// comments is refused by that gate until it is taught the shape. That is a
// known conflict, not a contradiction here.
func renderList(expressions, comments []string) []byte {
	var rendered bytes.Buffer
	for _, comment := range comments {
		rendered.WriteString(comment)
		rendered.WriteByte('\n')
	}
	if len(comments) > 0 && len(expressions) > 0 {
		rendered.WriteByte('\n')
	}
	for _, expression := range expressions {
		rendered.WriteString(expression)
		rendered.WriteByte('\n')
	}
	return rendered.Bytes()
}

// mergeExpressions unions two converted sets and renders them sorted, so the
// published bytes are a function of the two SETS and not of the order the
// archives were converted in.
//
// Both arguments are already sorted and deduplicated by their own converter,
// which is why the dedupe here is about the overlap between them: the two
// repositories state many of the same domains, and the committed-list gate --
// firstDuplicate in committed_test.go, which refuses a list that repeats a
// line -- is what that overlap would otherwise fail. That gate is why this is
// deduped rather than merely appended; the matcher is not the reason, and a
// duplicate rule would not change what the router answers either way.
func mergeExpressions(first, second []string) []string {
	seen := make(map[string]bool, len(first)+len(second))
	merged := make([]string, 0, len(first)+len(second))
	for _, set := range [][]string{first, second} {
		for _, expression := range set {
			if seen[expression] {
				continue
			}
			seen[expression] = true
			merged = append(merged, expression)
		}
	}
	sort.Strings(merged)
	return merged
}

// fetchArchive downloads the source archive of one ref's commit from the fixed
// codeload endpoint, enforcing the download bound before and while reading.
//
// The repository comes from the ref and is checked against the allowlist HERE,
// before it is concatenated into the URL rather than by the caller: this is the
// one function every archive fetch goes through, and a repository name is a
// path segment under a host this package chose. A caller that had already
// validated the lock would make this check redundant, which is exactly why a
// redundant check in the place a string becomes a URL is the one worth keeping.
//
// It takes the ref rather than the lock BECAUSE there are two of them. Reading
// lock.Primary here would check the primary's repository and then build the
// supplement's URL out of whatever the caller passed, so the second source
// would reach the network through a gate that never looked at it.
func fetchArchive(ctx context.Context, client *http.Client, ref *SourceRef) ([]byte, error) {
	if !KnownRepository(ref.Repository) {
		return nil, fmt.Errorf("repository %q is not one of the reviewed sources %q and %q", ref.Repository, RepositoryChina, RepositoryV2Fly)
	}
	endpoint := archiveBaseURL + "/" + ref.Repository + "/tar.gz/" + ref.Commit
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", endpoint, err)
	}
	request.Header.Set("Accept", "application/gzip")
	request.Header.Set("User-Agent", "mosdns-router")

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: unexpected status %d", endpoint, response.StatusCode)
	}
	if response.ContentLength > maxArchiveBytes {
		return nil, fmt.Errorf("%s: archive declares %d bytes, which is larger than the %d byte limit", endpoint, response.ContentLength, maxArchiveBytes)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read archive: %w", endpoint, err)
	}
	if len(archive) > maxArchiveBytes {
		return nil, fmt.Errorf("%s: archive is larger than the %d byte limit", endpoint, maxArchiveBytes)
	}
	return archive, nil
}

// extractArchive unpacks a source archive into a read-only in-memory
// filesystem rooted at the archive's single top-level directory, which is the
// one named after the repository and the pinned commit. Anything that is not a
// regular file, any path that could reach outside the archive, and any archive
// above the size or entry bounds are refused: an archive is untrusted input.
func extractArchive(archive []byte, repository, commit string) (fs.FS, error) {
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open source archive: %w", err)
	}
	defer func() { _ = reader.Close() }()

	files := make(map[string][]byte)
	tarReader := tar.NewReader(reader)
	root := archiveRoot(repository, commit)
	prefix := root + "/"
	var total int64
	for entries := 0; ; entries++ {
		if entries >= maxArchiveFiles {
			return nil, fmt.Errorf("source archive holds more than %d entries", maxArchiveFiles)
		}
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read source archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			// A GitHub source archive opens with a PAX global header that names
			// itself and carries no file of the repository.
			continue
		}
		name := header.Name
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
			if name == "" {
				continue
			}
		}
		if !fs.ValidPath(name) || name != path.Clean(name) {
			return nil, fmt.Errorf("unsafe archive path %q", header.Name)
		}
		if name == root {
			// The archive's own top-level directory, which holds the repository.
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			return nil, fmt.Errorf("archive entry %q is not inside the %q top-level directory", header.Name, root)
		}
		relative := strings.TrimPrefix(name, prefix)
		if relative == "" {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if header.Size > maxArchiveFileBytes {
				return nil, fmt.Errorf("archive entry %q is %d bytes, larger than the %d byte limit", header.Name, header.Size, maxArchiveFileBytes)
			}
			total += header.Size
			if total > maxArchiveBytes {
				return nil, fmt.Errorf("archive holds more than the %d bytes of uncompressed content limit", maxArchiveBytes)
			}
			contents, err := io.ReadAll(io.LimitReader(tarReader, header.Size))
			if err != nil {
				return nil, fmt.Errorf("read archive entry %q: %w", header.Name, err)
			}
			files[relative] = contents
		default:
			return nil, fmt.Errorf("unsupported archive entry type %d for %q", header.Typeflag, header.Name)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("source archive holds no files under the %q top-level directory", root)
	}
	return &archiveFS{files: files}, nil
}

// archiveRoot is the single top-level directory a GitHub source archive for a
// commit is named after: the repository's own name, then the commit. Requiring
// it proves the archive is the repository at the pinned commit, so a substituted
// or repacked archive is refused.
//
// It is derived from the reviewed repository rather than written out per
// repository, because a hardcoded root is how a two-source download ends up
// extracting the dnsmasq archive under v2fly's directory name and refusing it
// for being the wrong shape. The value comes from an allowlisted constant, so
// deriving it does not widen what may be looked for.
func archiveRoot(repository, commit string) string {
	return path.Base(repository) + "-" + commit
}

// archiveFS is a read-only filesystem over the files a source archive held.
type archiveFS struct {
	files map[string][]byte
}

func (a *archiveFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	contents, ok := a.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &archiveFile{name: name, Reader: bytes.NewReader(contents)}, nil
}

// archiveFile is one file of an archiveFS.
type archiveFile struct {
	name string
	*bytes.Reader
}

func (f *archiveFile) Stat() (fs.FileInfo, error) {
	return archiveFileInfo{name: f.name, size: f.Size()}, nil
}

func (f *archiveFile) Close() error { return nil }

// archiveFileInfo describes an archiveFile to a caller that stats it.
type archiveFileInfo struct {
	name string
	size int64
}

func (i archiveFileInfo) Name() string       { return path.Base(i.name) }
func (i archiveFileInfo) Size() int64        { return i.size }
func (i archiveFileInfo) Mode() fs.FileMode  { return 0o444 }
func (i archiveFileInfo) ModTime() time.Time { return time.Time{} }
func (i archiveFileInfo) IsDir() bool        { return false }
func (i archiveFileInfo) Sys() any           { return nil }

// digestHex returns the lowercase hex SHA-256 of b, which is the shape a lock
// records and a pinned download requires.
func digestHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// isLowerHex reports whether value is exactly length lower-case hex characters.
// Upper case, short, long and non-hex values are all refused: a digest or commit
// id is compared as bytes, so a different spelling of the same value would
// silently never match.
func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}
