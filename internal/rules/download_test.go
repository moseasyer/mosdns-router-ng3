package rules

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The archive fixtures below are built in memory, byte for byte, so no test in
// this file reads GitHub. The API fixture is the one field this package reads
// from a commit document, wrapped in the surrounding keys GitHub sends, so a
// decoder that reached for anything else would fail here rather than in
// production.

const (
	// fixtureCommit is the 40-hex commit the fake remote serves. It is a made-up
	// but well-formed commit id, not a real one.
	fixtureCommit = "0123456789abcdef0123456789abcdef01234567"
	// fixtureArchiveTop is the single top-level directory a GitHub source
	// archive carries. It is named after the repository and the commit, so the
	// second reviewed source has its own root rather than sharing this one.
	fixtureArchiveTop = "domain-list-community-" + fixtureCommit
	// fixtureChinaArchiveTop is the same for the dnsmasq source.
	fixtureChinaArchiveTop = "dnsmasq-china-list-" + fixtureCommit
	// fixtureList is the China list the fixture archive carries. It states one
	// rule per supported form, a bare domain, an include, an attribute that is
	// kept, and the attribute upstream casts out of the cn lists.
	fixtureList = "domain:example.cn @cn\nfull:exact.cn\ninclude:shared\ncast-out.cn @!cn\nads.cn @ads\n"
	// fixtureChinaList is the China list the dnsmasq fixture archive carries.
	// The address is 203.0.113.7 and NOT the address upstream really uses, so a
	// converter that published the address rather than dropping it could not
	// pass by having published the right value. The second line is an entry
	// upstream disabled, whose reason has to survive as a comment.
	fixtureChinaList = "server=/one.cn/203.0.113.7\n#server=/two.cn/203.0.113.7 # Disabled: upstream moved it\n"
	// fixtureShared is the list the fixture archive's China list includes. Its
	// plain copy of example.cn is the same MOSDNS expression as the attributed
	// one in the China list, and must collapse onto it.
	fixtureShared = "regexp:^static[0-9]+\\.example\\.cn$\nexample.cn\nkeyword:测试\n"
)

// wantFixtureExpressions is the converted fixture list, sorted and with the
// `@!cn` rule cast out, derived by hand from the two fixture files above.
var wantFixtureExpressions = []string{
	"domain:ads.cn",
	"domain:example.cn",
	"full:exact.cn",
	"keyword:测试",
	"regexp:^static[0-9]+\\.example\\.cn$",
}

// archiveEntry is one entry of a test archive.
type archiveEntry struct {
	name       string
	body       string
	typeflag   byte
	linkname   string
	size       int64 // overrides the body length when non-zero
	paxRecords map[string]string
}

// buildArchive returns a gzipped tar archive holding the given entries.
func buildArchive(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		size := entry.size
		if size == 0 {
			size = int64(len(entry.body))
		}
		if entry.typeflag == tar.TypeXGlobalHeader {
			// A PAX global header may carry nothing but its records.
			if err := tarWriter.WriteHeader(&tar.Header{
				Name:       entry.name,
				Typeflag:   tar.TypeXGlobalHeader,
				PAXRecords: entry.paxRecords,
			}); err != nil {
				t.Fatalf("write archive global header %q: %v", entry.name, err)
			}
			continue
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o644,
			Size:     size,
			Typeflag: entry.typeflag,
			Linkname: entry.linkname,
		}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write archive header %q: %v", entry.name, err)
		}
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeXGlobalHeader {
			// A directory and a PAX global header carry no list, but they are real
			// entries: a global header names itself and is not a list file, and a
			// directory names its contents.
			continue
		}
		// The declared size may exceed the body, which is how a fixture claims a
		// file larger than the bound the converter enforces.
		source := io.Reader(bytes.NewReader([]byte(entry.body)))
		if int64(len(entry.body)) < size {
			source = io.LimitReader(zeroReader{}, size)
		}
		if _, err := io.CopyN(tarWriter, source, size); err != nil {
			t.Fatalf("write archive body %q: %v", entry.name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return compressed.Bytes()
}

// fixtureArchive is the archive every happy-path test in this file pins.
func fixtureArchive(t *testing.T) []byte {
	t.Helper()
	return buildArchive(t, []archiveEntry{
		{name: fixtureArchiveTop + "/data/cn", body: fixtureList},
		{name: fixtureArchiveTop + "/data/shared", body: fixtureShared},
		{name: fixtureArchiveTop + "/README.md", body: "# fixture\n"},
	})
}

// fakeRemote is a GitHub-shaped HTTP origin: a commit endpoint and a source
// archive endpoint, both served from one test server. Every request is
// recorded so a test can assert that a path was never fetched.
type fakeRemote struct {
	server *httptest.Server
	client *http.Client
	// sources is what this origin can answer for, keyed by repository.
	//
	// It is keyed by repository, and each source names its own head and its own
	// archives, rather than being one archive served at one commit: an origin
	// that answers for exactly one repository cannot observe a request for
	// another one at all -- the request 404s and the test passes either way --
	// which is how a fixture ends up modelling a machine that has only ever had
	// one source.
	sources map[string]fakeSource

	// commitBody, when set (rawBody true), is served verbatim by the commit
	// endpoint instead of a commit document.
	commitBody string
	rawBody    bool
	// declaredLength overrides the Content-Length the archive endpoint sends.
	declaredLength int64
	// streamBytes, when non-zero, makes the archive endpoint stream that many
	// zero bytes instead of the archive, so a test can exercise the read bound.
	streamBytes int64
	status      int
	requests    []string
}

// fakeSource is one repository the origin will answer for.
type fakeSource struct {
	// head is what the repository's commit endpoint answers.
	head string
	// archives maps a 40-character commit to that repository's archive bytes.
	archives map[string][]byte
}

// newFakeRemote serves one repository's archive, which is all most tests in
// this file need.
func newFakeRemote(t *testing.T, commit string, archive []byte) *fakeRemote {
	t.Helper()
	return newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryV2Fly: {head: commit, archives: map[string][]byte{commit: archive}},
	})
}

// newFakeRemoteSources serves the named repositories from one origin, and 404s
// every path naming a repository it holds nothing for.
func newFakeRemoteSources(t *testing.T, sources map[string]fakeSource) *fakeRemote {
	t.Helper()
	remote := &fakeRemote{sources: make(map[string]fakeSource, len(sources)), status: http.StatusOK}
	for repository, source := range sources {
		remote.sources[repository] = source
	}
	remote.server = httptest.NewServer(http.HandlerFunc(remote.serve))
	t.Cleanup(remote.server.Close)
	remote.client = hostRewritingClient(t, remote.server.URL)
	return remote
}

func (r *fakeRemote) serve(writer http.ResponseWriter, request *http.Request) {
	r.requests = append(r.requests, request.URL.Path)
	if r.status != 0 && r.status != http.StatusOK {
		writer.WriteHeader(r.status)
		return
	}
	switch {
	case strings.HasSuffix(request.URL.Path, "/commits/HEAD"):
		source, known := r.sourceFor(request.URL.Path)
		if !known {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if r.rawBody {
			_, _ = io.WriteString(writer, r.commitBody)
			return
		}
		// The surrounding keys are the ones GitHub sends around the single field
		// this package reads.
		_, _ = io.WriteString(writer, fmt.Sprintf(`{"sha":%q,"node_id":"C_1","commit":{"committer":{"date":"2026-09-25T01:39:17Z"}},"parents":[]}`, source.head))
	default:
		archive, known := r.archiveFor(request.URL.Path)
		if !known {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/gzip")
		if r.streamBytes != 0 && r.declaredLength == 0 {
			// No declared length, so only the reader bound can stop this body.
			_, _ = io.CopyN(writer, zeroReader{}, r.streamBytes)
			return
		}
		length := int64(len(archive))
		if r.declaredLength != 0 {
			length = r.declaredLength
		}
		writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if r.streamBytes != 0 {
			_, _ = io.CopyN(writer, zeroReader{}, r.streamBytes)
			return
		}
		_, _ = writer.Write(archive)
	}
}

// sourceFor returns the repository a path names and what this origin holds for
// it, and whether it holds anything for that repository at all.
func (r *fakeRemote) sourceFor(requestPath string) (fakeSource, bool) {
	repository := repositoryOfPath(requestPath)
	if repository == "" {
		return fakeSource{}, false
	}
	source, known := r.sources[repository]
	return source, known
}

// archivePrefix is the codeload path a repository's source archives live under.
func archivePrefix(repository string) string {
	return "/" + repository + "/tar.gz/"
}

// repositoryOfPath returns the `owner/name` a codeload or API path names, or "".
//
// A repository name has two segments, and the API prefixes the pair with
// `/repos`, so the pair is taken from the front after that prefix is dropped.
// Reading one segment would yield `felixonmars` for every request and answer
// with one repository's commit whatever was asked for.
func repositoryOfPath(requestPath string) string {
	trimmed := strings.TrimPrefix(requestPath, "/")
	trimmed = strings.TrimPrefix(trimmed, "repos/")
	owner, rest, found := strings.Cut(trimmed, "/")
	if !found {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return ""
	}
	return owner + "/" + name
}

// archiveFor returns the archive a codeload path names, and whether this origin
// holds one for that repository at that commit.
func (r *fakeRemote) archiveFor(requestPath string) ([]byte, bool) {
	source, known := r.sourceFor(requestPath)
	if !known {
		return nil, false
	}
	commit, isArchive := strings.CutPrefix(requestPath, archivePrefix(repositoryOfPath(requestPath)))
	if !isArchive {
		return nil, false
	}
	archive, held := source.archives[commit]
	return archive, held
}

// served reports whether path was requested from this origin.
func (r *fakeRemote) served(path string) bool {
	return slices.Contains(r.requests, path)
}

// hostRewritingClient returns a client that sends every request to the test
// server, so production code under test keeps building and using its real
// api.github.com and codeload.github.com URLs.
func hostRewritingClient(t *testing.T, serverURL string) *http.Client {
	t.Helper()
	target, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	transport := &http.Transport{}
	return &http.Client{Transport: rewriteHostTransport{base: target, inner: transport}}
}

type rewriteHostTransport struct {
	base  *url.URL
	inner *http.Transport
}

func (r rewriteHostTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	next := request.Clone(request.Context())
	next.URL.Scheme = r.base.Scheme
	next.URL.Host = r.base.Host
	return r.inner.RoundTrip(next)
}

// sameLock reports whether two locks are the same document. A lock holds two
// slices now, so it cannot be compared with ==, and a test that compared only
// the fields it cared about would stop noticing a lock that came back carrying
// an entry set it did not have.
func sameLock(got, want SourceLock) bool {
	return got.SchemaVersion == want.SchemaVersion &&
		sameRef(got.Primary, want.Primary) &&
		(got.Supplement == nil) == (want.Supplement == nil) &&
		(got.Supplement == nil || sameRef(*got.Supplement, *want.Supplement)) &&
		got.ListSHA256 == want.ListSHA256
}

func sameRef(got, want SourceRef) bool {
	return got.Repository == want.Repository &&
		got.Commit == want.Commit &&
		got.SHA256 == want.SHA256 &&
		got.Entry == want.Entry &&
		slices.Equal(got.Entries, want.Entries) &&
		slices.Equal(got.Kinds, want.Kinds)
}

// digest returns the lowercase hex SHA-256 of b, which is the shape a lock
// records and a pinned download requires.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pinnedLock is a complete lock for the fixture archive: the digest is computed
// here from the archive the fake remote serves, exactly as a pin does.
func pinnedLock(t *testing.T, archive []byte) SourceLock {
	t.Helper()
	return SourceLock{
		SchemaVersion: schemaVersion,
		Primary: SourceRef{
			Repository: RepositoryV2Fly,
			Commit:     fixtureCommit,
			SHA256:     digest(archive),
			Entry:      V2FlyEntry,
		},
	}
}

// chinaArchive is the dnsmasq source archive the fake remote serves: the three
// files the pin names, under the top-level directory that repository's own
// source archive is named after.
func chinaArchive(t *testing.T) []byte {
	t.Helper()
	return buildArchive(t, []archiveEntry{
		{name: fixtureChinaArchiveTop + "/accelerated-domains.china.conf", body: fixtureChinaList},
		{name: fixtureChinaArchiveTop + "/google.china.conf", body: "server=/three.cn/203.0.113.7\n"},
		{name: fixtureChinaArchiveTop + "/apple.china.conf", body: "server=/four.cn/203.0.113.7\n"},
	})
}

// sourceArchiveFiles builds the source archive one repository publishes at one
// commit, holding several files. Arguments after the commit are path/body
// pairs.
//
// The top-level directory is derived from the repository and the commit by the
// production helper, because that is the name extractArchive looks for and a
// fixture rooted anywhere else would be refused for the wrong reason. The
// dnsmasq primary needs three files in one archive, which is what this is for:
// chinaArchive and fixtureArchive are fixed at fixtureCommit, while a two-source
// download pins each repository at its own commit.
func sourceArchiveFiles(t *testing.T, repository, commit string, pathAndBody ...string) []byte {
	t.Helper()
	if len(pathAndBody)%2 != 0 {
		t.Fatal("sourceArchiveFiles takes path/body pairs")
	}
	root := archiveRoot(repository, commit)
	entries := make([]archiveEntry, 0, len(pathAndBody)/2)
	for index := 0; index < len(pathAndBody); index += 2 {
		entries = append(entries, archiveEntry{
			name: root + "/" + pathAndBody[index],
			body: pathAndBody[index+1],
		})
	}
	return buildArchive(t, entries)
}

// chinaPinnedLock is a complete lock for the dnsmasq fixture archive: the
// reviewed source IS the primary and there is no supplement.
//
// It is NOT the fallback publication's shape. That one names v2fly as the
// primary -- see TestASourceWithNoSupplementPublishesOnlyItsOwnRules -- because
// a v2fly primary has nothing a supplement could add. This shape is the
// single-source dnsmasq publication, which is a third thing: before the union
// it was all this build could publish, and it is still a legal lock.
func chinaPinnedLock(t *testing.T, archive []byte) SourceLock {
	t.Helper()
	return SourceLock{
		SchemaVersion: schemaVersion,
		Primary: SourceRef{
			Repository: RepositoryChina,
			Commit:     fixtureCommit,
			SHA256:     digest(archive),
			Entries:    ChinaConfEntries,
		},
	}
}

func TestDownloadRecordsThePinnedSourceAndConvertsTheEntry(t *testing.T) {
	// The lock a pin writes is the audit record: which reviewed commit, which
	// archive digest, which entry, and the digest of the exact list the router
	// then reads. A Download that returned a lock missing or reordering those
	// would publish a list nothing can be checked against later.
	archive := fixtureArchive(t)
	remote := newFakeRemote(t, fixtureCommit, archive)

	got, list, err := Download(context.Background(), remote.client, pinnedLock(t, archive))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	wantLock := pinnedLock(t, archive)
	wantLock.ListSHA256 = digest(list)
	if !sameLock(got, wantLock) {
		t.Fatalf("Download lock = %+v, want %+v", got, wantLock)
	}
	if got.SchemaVersion != schemaVersion || got.Primary.Repository != RepositoryV2Fly || got.Primary.Entry != V2FlyEntry {
		t.Fatalf("Download lock lost its pinned identity: %+v", got)
	}

	wantList := ""
	for _, expression := range wantFixtureExpressions {
		wantList += expression + "\n"
	}
	if string(list) != wantList {
		t.Fatalf("Download list =\n%q\nwant\n%q", list, wantList)
	}
	if got.ListSHA256 != digest([]byte(wantList)) {
		t.Fatalf("ListSHA256 = %q, want the digest of the returned list %q", got.ListSHA256, digest([]byte(wantList)))
	}
	if want := "/v2fly/domain-list-community/tar.gz/" + fixtureCommit; !remote.served(want) {
		t.Fatalf("Download fetched %v, want the archive at %q", remote.requests, want)
	}
}

func TestDownloadAcceptsTheShapeAGitHubSourceArchiveActuallyHas(t *testing.T) {
	// A GitHub source archive opens with a PAX global header that names itself,
	// carries explicit directory entries with a trailing slash, and then the
	// files. An extractor that insisted every entry was a list file would refuse
	// every real archive, which is what the one-time pin against the reviewed
	// commit ran into.
	archive := buildArchive(t, []archiveEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, paxRecords: map[string]string{"comment": "fixture"}},
		{name: fixtureArchiveTop + "/", typeflag: tar.TypeDir},
		{name: fixtureArchiveTop + "/data/", typeflag: tar.TypeDir},
		{name: fixtureArchiveTop + "/data/cn", body: fixtureList},
		{name: fixtureArchiveTop + "/data/shared", body: fixtureShared},
		{name: fixtureArchiveTop + "/LICENSE", body: "MIT\n"},
	})
	remote := newFakeRemote(t, fixtureCommit, archive)

	_, list, err := Download(context.Background(), remote.client, pinnedLock(t, archive))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	want := ""
	for _, expression := range wantFixtureExpressions {
		want += expression + "\n"
	}
	if string(list) != want {
		t.Fatalf("Download list =\n%q\nwant\n%q", list, want)
	}
}

func TestDownloadRejectsAChecksumMismatchBeforeConversion(t *testing.T) {
	// The archive in the fixture would fail conversion if it were read at all,
	// so the reported error proves the digest is checked first: a Download that
	// extracted before verifying would report the unknown directive instead.
	archive := buildArchive(t, []archiveEntry{
		{name: fixtureArchiveTop + "/data/cn", body: "ip4:1.2.3.4\n"},
	})
	remote := newFakeRemote(t, fixtureCommit, archive)

	lock := pinnedLock(t, archive)
	lock.Primary.SHA256 = digest([]byte("a different archive"))
	lockReturned, list, err := Download(context.Background(), remote.client, lock)
	if err == nil {
		t.Fatalf("Download accepted a mismatched archive and returned %d bytes", len(list))
	}
	if list != nil {
		t.Fatalf("Download returned list bytes on failure: %q", list)
	}
	if !sameLock(lockReturned, SourceLock{}) {
		t.Fatalf("Download returned a lock on failure: %+v", lockReturned)
	}
	if !strings.Contains(err.Error(), "archive sha256") || !strings.Contains(err.Error(), lock.Primary.SHA256) {
		t.Fatalf("error = %v, want it to name the expected archive digest", err)
	}
	if strings.Contains(err.Error(), "ip4") {
		t.Fatalf("error came from conversion, so the digest was not checked first: %v", err)
	}
}

func TestDownloadRefusesALockThatIsNotFullyPinned(t *testing.T) {
	// Every field of a lock is a security decision, not metadata. A lock that
	// names another repository, another list, a short commit, an upper-case
	// digest or a schema version this build does not know must be refused
	// before a byte is fetched, or the gateway would convert someone else's
	// list and record it as reviewed.
	archive := fixtureArchive(t)
	china := chinaArchive(t)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryV2Fly: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: archive}},
		RepositoryChina: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: china}},
	})
	valid := pinnedLock(t, archive)
	validChina := chinaPinnedLock(t, china)
	tests := []struct {
		name string
		// base is the reviewed lock a mutation starts from. The zero value is
		// the v2fly primary, so a case that does not name one reads as the
		// ordinary lock broken one way.
		base   SourceLock
		mutate func(*SourceLock)
	}{
		{name: "empty commit", mutate: func(l *SourceLock) { l.Primary.Commit = "" }},
		{name: "short commit", mutate: func(l *SourceLock) { l.Primary.Commit = "0123456789abcdef" }},
		{name: "non hex commit", mutate: func(l *SourceLock) { l.Primary.Commit = strings.Repeat("z", 40) }},
		{name: "upper case commit", mutate: func(l *SourceLock) { l.Primary.Commit = strings.ToUpper(fixtureCommit) }},
		{name: "commit with a ref", mutate: func(l *SourceLock) { l.Primary.Commit = fixtureCommit + "/data/cn" }},
		{name: "empty archive digest", mutate: func(l *SourceLock) { l.Primary.SHA256 = "" }},
		{name: "short archive digest", mutate: func(l *SourceLock) { l.Primary.SHA256 = valid.Primary.SHA256[:63] }},
		{name: "upper case archive digest", mutate: func(l *SourceLock) { l.Primary.SHA256 = strings.ToUpper(valid.Primary.SHA256) }},
		{name: "other repository", mutate: func(l *SourceLock) { l.Primary.Repository = "attacker/domain-list" }},
		{name: "empty repository", mutate: func(l *SourceLock) { l.Primary.Repository = "" }},
		{name: "other entry", mutate: func(l *SourceLock) { l.Primary.Entry = "data/geolocation-!cn" }},
		{name: "absolute entry", mutate: func(l *SourceLock) { l.Primary.Entry = "/data/cn" }},
		{name: "empty entry", mutate: func(l *SourceLock) { l.Primary.Entry = "" }},
		{name: "unknown schema", mutate: func(l *SourceLock) { l.SchemaVersion = schemaVersion + 1 }},
		{name: "missing schema", mutate: func(l *SourceLock) { l.SchemaVersion = 0 }},
		{
			name: "v2fly source naming entries",
			mutate: func(l *SourceLock) {
				l.Primary.Entries = []string{V2FlyEntry}
			},
		},
		// The dnsmasq source is three named files, so both shapes a lock can
		// carry are wrong on it: an entry it does not use, and entries it left
		// out. The pin is the fixed set, so a lock that names a fourth file is
		// refused too rather than converted as if it had been reviewed.
		{
			name: "dnsmasq source naming a single entry",
			base: validChina,
			mutate: func(l *SourceLock) {
				l.Primary.Entry = V2FlyEntry
			},
		},
		{
			name:   "dnsmasq source with no entries",
			base:   validChina,
			mutate: func(l *SourceLock) { l.Primary.Entries = nil },
		},
		{
			name: "dnsmasq source naming a file outside the reviewed three",
			base: validChina,
			mutate: func(l *SourceLock) {
				l.Primary.Entries = []string{"bogus-nxdomain.china.conf"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lock := valid
			if test.base.Primary.Repository != "" {
				lock = test.base
			}
			test.mutate(&lock)
			if _, _, err := Download(context.Background(), remote.client, lock); err == nil {
				t.Fatal("Download accepted a lock that is not fully pinned")
			}
			if len(remote.requests) != 0 {
				t.Fatalf("Download fetched %v before refusing the lock", remote.requests)
			}
		})
	}
}

func TestDownloadRejectsAnArchiveThatCouldEscapeOrFlood(t *testing.T) {
	// A source archive is untrusted input. Anything that would write outside the
	// archive root, present a link instead of a list, or make the converter do
	// unbounded work is refused. The last case streams more than the download
	// bound, so it is the one test in this file that moves real bytes.
	tests := []struct {
		name    string
		entries []archiveEntry
		wantSub string
	}{
		{
			name:    "parent traversal",
			entries: []archiveEntry{{name: "../escape", body: "domain:escape.cn\n"}},
			wantSub: "unsafe archive path",
		},
		{
			name:    "absolute path",
			entries: []archiveEntry{{name: "/etc/passwd", body: "root\n"}},
			wantSub: "unsafe archive path",
		},
		{
			name:    "nested traversal",
			entries: []archiveEntry{{name: fixtureArchiveTop + "/data/../../escape", body: "domain:escape.cn\n"}},
			wantSub: "unsafe archive path",
		},
		{
			name:    "symlink",
			entries: []archiveEntry{{name: fixtureArchiveTop + "/data/cn", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
			wantSub: "unsupported archive entry type",
		},
		{
			name:    "hard link",
			entries: []archiveEntry{{name: fixtureArchiveTop + "/data/cn", typeflag: tar.TypeLink, linkname: fixtureArchiveTop + "/data/shared"}},
			wantSub: "unsupported archive entry type",
		},
		{
			name:    "character device",
			entries: []archiveEntry{{name: fixtureArchiveTop + "/data/cn", typeflag: tar.TypeChar}},
			wantSub: "unsupported archive entry type",
		},
		{
			name:    "fifo",
			entries: []archiveEntry{{name: fixtureArchiveTop + "/data/cn", typeflag: tar.TypeFifo}},
			wantSub: "unsupported archive entry type",
		},
		{
			name: "file above the size bound",
			entries: []archiveEntry{
				{name: fixtureArchiveTop + "/data/cn", body: fixtureList},
				{name: fixtureArchiveTop + "/data/big", size: 4<<20 + 1},
			},
			wantSub: "larger than the 4194304 byte",
		},
		{
			name: "entry count above the bound",
			entries: func() []archiveEntry {
				entries := []archiveEntry{{name: fixtureArchiveTop + "/data/cn", body: fixtureList}}
				for i := 0; i <= maxArchiveFiles; i++ {
					entries = append(entries, archiveEntry{name: fmt.Sprintf("%s/data/pad%d", fixtureArchiveTop, i)})
				}
				return entries
			}(),
			wantSub: "more than 32768 entries",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := buildArchive(t, test.entries)
			remote := newFakeRemote(t, fixtureCommit, archive)
			_, list, err := Download(context.Background(), remote.client, pinnedLock(t, archive))
			if err == nil {
				t.Fatalf("Download accepted the archive and returned %q", list)
			}
			if !strings.Contains(err.Error(), test.wantSub) {
				t.Fatalf("error = %v, want it to mention %q", err, test.wantSub)
			}
		})
	}
}

func TestDownloadRejectsAnArchiveWithoutExactlyOneTopLevelDirectory(t *testing.T) {
	// The entry path is resolved inside the archive, so the conversion is only
	// meaningful if every file sits under the one directory the source archive is
	// named after. An archive that mixes roots, or that is bare, is refused
	// rather than partially converted.
	tests := []struct {
		name    string
		entries []archiveEntry
	}{
		{
			name: "no top level directory",
			entries: []archiveEntry{
				{name: "data/cn", body: fixtureList},
				{name: "data/shared", body: fixtureShared},
			},
		},
		{
			name: "two top level directories",
			entries: []archiveEntry{
				{name: fixtureArchiveTop + "/data/cn", body: fixtureList},
				{name: "other-root/data/shared", body: fixtureShared},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := buildArchive(t, test.entries)
			remote := newFakeRemote(t, fixtureCommit, archive)
			_, list, err := Download(context.Background(), remote.client, pinnedLock(t, archive))
			if err == nil {
				t.Fatalf("Download accepted the archive and returned %q", list)
			}
			if !strings.Contains(err.Error(), "top-level directory") {
				t.Fatalf("error = %v, want it to mention the top-level directory", err)
			}
		})
	}
}

func TestDownloadReportsAMissingEntryInsteadOfAnEmptyList(t *testing.T) {
	// A pinned commit that no longer carries the entry would otherwise convert to
	// an empty China set, which the router would read as "nothing is Chinese".
	archive := buildArchive(t, []archiveEntry{{name: fixtureArchiveTop + "/README.md", body: "# fixture\n"}})
	remote := newFakeRemote(t, fixtureCommit, archive)

	_, list, err := Download(context.Background(), remote.client, pinnedLock(t, archive))
	if err == nil {
		t.Fatalf("Download accepted an archive without the entry and returned %q", list)
	}
	if !strings.Contains(err.Error(), V2FlyEntry) {
		t.Fatalf("error = %v, want it to name the entry %q", err, V2FlyEntry)
	}
}

func TestResolveCommitReturnsTheReviewedDefaultBranchCommit(t *testing.T) {
	// A pin starts from the commit the repository publishes for its default
	// branch, so the request path and the 40-hex requirement are the contract.
	archive := fixtureArchive(t)
	remote := newFakeRemote(t, fixtureCommit, archive)

	lock, err := ResolveCommit(context.Background(), remote.client, RepositoryV2Fly, "")
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if lock.Primary.Commit != fixtureCommit {
		t.Fatalf("Commit = %q, want %q", lock.Primary.Commit, fixtureCommit)
	}
	if lock.Primary.Repository != RepositoryV2Fly || lock.Primary.Entry != V2FlyEntry || lock.SchemaVersion != schemaVersion {
		t.Fatalf("resolved lock lost its pinned identity: %+v", lock)
	}
	if lock.Primary.SHA256 != "" || lock.ListSHA256 != "" || lock.Supplement != nil {
		t.Fatalf("ResolveCommit claimed a source or a digest it has not read: %+v", lock)
	}
	if want := "/repos/v2fly/domain-list-community/commits/HEAD"; !remote.served(want) {
		t.Fatalf("ResolveCommit fetched %v, want %q", remote.requests, want)
	}
	if len(remote.requests) != 1 {
		t.Fatalf("ResolveCommit fetched %v, want only the commit document", remote.requests)
	}
}

func TestResolveCommitRefusesAResponseThatIsNotAFortyHexCommit(t *testing.T) {
	// A commit that is not a full 40-hex id cannot address a source archive, so
	// accepting one would either fail later or, worse, be recorded in a lock as
	// if it had been reviewed.
	archive := fixtureArchive(t)
	tests := []struct {
		name   string
		commit string
	}{
		{name: "empty", commit: ""},
		{name: "abbreviated", commit: "0123456"},
		{name: "39 hex", commit: fixtureCommit[:39]},
		{name: "41 hex", commit: fixtureCommit + "0"},
		{name: "not hex", commit: strings.Repeat("z", 40)},
		{name: "upper case hex", commit: strings.ToUpper(fixtureCommit)},
		{name: "a ref", commit: "refs/heads/master"},
		{name: "a tag", commit: "v1.0.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := newFakeRemote(t, test.commit, archive)
			lock, err := ResolveCommit(context.Background(), remote.client, RepositoryV2Fly, "")
			if err == nil {
				t.Fatalf("ResolveCommit accepted %q and returned %+v", test.commit, lock)
			}
			if len(remote.requests) != 1 {
				t.Fatalf("ResolveCommit fetched %v, want only the commit document", remote.requests)
			}
		})
	}
}

func TestResolveCommitRefusesARemoteFailure(t *testing.T) {
	// An outage, a rate limit and a body that is not a commit document are all
	// ways the remote can fail to name a commit. Each must be an error, and none
	// may be turned into a pin.
	archive := fixtureArchive(t)
	tests := []struct {
		name       string
		commit     string
		commitBody string
		rawBody    bool
		status     int
		wantSub    string
	}{
		{name: "server error", commit: fixtureCommit, status: http.StatusInternalServerError, wantSub: "unexpected status 500"},
		{name: "not found", commit: fixtureCommit, status: http.StatusNotFound, wantSub: "unexpected status 404"},
		{name: "rate limited", commit: fixtureCommit, status: http.StatusForbidden, wantSub: "unexpected status 403"},
		{name: "empty body", commit: fixtureCommit, rawBody: true, wantSub: "decode commit"},
		{name: "not a document", commit: fixtureCommit, commitBody: "not json at all", rawBody: true, wantSub: "decode commit"},
		{name: "no sha field", commit: fixtureCommit, commitBody: `{"node_id":"C_1"}`, rawBody: true, wantSub: "has no 40 character lower-case hex sha"},
		{name: "an html error page", commit: fixtureCommit, commitBody: "<html>rate limited</html>", rawBody: true, wantSub: "decode commit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := newFakeRemote(t, test.commit, archive)
			remote.status = test.status
			remote.commitBody = test.commitBody
			remote.rawBody = test.rawBody
			if _, err := ResolveCommit(context.Background(), remote.client, RepositoryV2Fly, ""); err == nil {
				t.Fatal("ResolveCommit accepted a failing remote")
			} else if !strings.Contains(err.Error(), test.wantSub) {
				t.Fatalf("error = %v, want it to mention %q", err, test.wantSub)
			}
			if len(remote.requests) != 1 {
				t.Fatalf("ResolveCommit fetched %v, want only the commit document", remote.requests)
			}
		})
	}
}

func TestResolveHEADRecordsTheArchiveDigestOfTheResolvedCommit(t *testing.T) {
	// A pin cannot record a digest it has not read, so ResolveHEAD resolves the
	// commit and then downloads that commit's archive to compute the digest that
	// the written lock carries. The lock it returns must not claim a list digest
	// it has not produced either.
	archive := fixtureArchive(t)
	remote := newFakeRemote(t, fixtureCommit, archive)

	lock, err := ResolveHEAD(context.Background(), remote.client, RepositoryV2Fly)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	want := pinnedLock(t, archive)
	if !sameLock(lock, want) {
		t.Fatalf("ResolveHEAD = %+v, want %+v", lock, want)
	}
	if wantArchivePath := "/v2fly/domain-list-community/tar.gz/" + fixtureCommit; !remote.served(wantArchivePath) {
		t.Fatalf("ResolveHEAD fetched %v, want the archive at %q", remote.requests, wantArchivePath)
	}
}

func TestResolveHEADRefusesAnotherRepository(t *testing.T) {
	// There are two reviewed sources for this gateway and the list is what makes
	// them the only two. Accepting any other caller-supplied repository would let
	// a pin record a list nobody reviewed as if it had been, and the repository
	// goes into a URL, so a name like a path traversal would also be a way out of
	// the endpoint this package means to reach.
	remote := newFakeRemote(t, fixtureCommit, fixtureArchive(t))

	for _, repository := range []string{
		"attacker/domain-list",
		"v2fly/domain-list-community/data",
		"../v2fly/domain-list-community",
		"V2Fly/domain-list-community",
		"felixonmars/dnsmasq-china-list ",
		"felixonmars/dnsmasq-china-list/../v2fly/domain-list-community",
		" ",
	} {
		if _, err := ResolveHEAD(context.Background(), remote.client, repository); err == nil {
			t.Errorf("ResolveHEAD accepted repository %q", repository)
		}
		if _, err := ResolveCommit(context.Background(), remote.client, repository, ""); err == nil {
			t.Errorf("ResolveCommit accepted repository %q", repository)
		}
	}
	if len(remote.requests) != 0 {
		t.Fatalf("an unsupported repository was fetched before being refused: %v", remote.requests)
	}
}

func TestDownloadRefusesADownloadLargerThanTheArchiveBound(t *testing.T) {
	// The archive bound exists so a hostile or broken origin cannot make the pin
	// buffer an unbounded body. The first case is refused on the declared length
	// alone; the second is an origin that understates its length and has to be
	// stopped by the reader.
	tests := []struct {
		name           string
		declaredLength int64
		streamBytes    int64
	}{
		{name: "declared length above the bound", declaredLength: maxArchiveBytes + 1, streamBytes: 1024},
		{name: "undeclared length above the bound", streamBytes: maxArchiveBytes + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := newFakeRemote(t, fixtureCommit, nil)
			remote.declaredLength = test.declaredLength
			remote.streamBytes = test.streamBytes

			lock := pinnedLock(t, []byte("unused"))
			_, list, err := Download(context.Background(), remote.client, lock)
			if err == nil {
				t.Fatalf("Download accepted an oversized archive and returned %q", list)
			}
			if !strings.Contains(err.Error(), "67108864") {
				t.Fatalf("error = %v, want it to name the 67108864 byte bound", err)
			}
		})
	}
}

func TestDownloadRefusesACancelledRequest(t *testing.T) {
	// A cancelled update has to stop, not to convert a partial archive into a
	// list. A pin that ignored cancellation would also keep a stale pin running
	// after the operator asked for it to stop.
	archive := fixtureArchive(t)
	remote := newFakeRemote(t, fixtureCommit, archive)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := Download(ctx, remote.client, pinnedLock(t, archive)); err == nil {
		t.Fatal("Download ignored a cancelled context")
	}
	if _, err := ResolveCommit(ctx, remote.client, RepositoryV2Fly, ""); err == nil {
		t.Fatal("ResolveCommit ignored a cancelled context")
	}
	if _, err := ResolveHEAD(ctx, remote.client, RepositoryV2Fly); err == nil {
		t.Fatal("ResolveHEAD ignored a cancelled context")
	}
}

func TestSourceLockJSONIsDeterministicAndStrict(t *testing.T) {
	// The lock file is the audit record, so the same lock must always render the
	// same bytes, and a lock that carries an unknown field must be refused rather
	// than partially read: a future field this build does not understand could
	// otherwise be dropped while the lock still claims to pin the source.
	archive := fixtureArchive(t)
	lock := pinnedLock(t, archive)
	lock.ListSHA256 = digest([]byte("domain:example.cn\n"))

	first, err := lock.Encode()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	second, err := lock.Encode()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("MarshalJSON is not deterministic:\n%s\n%s", first, second)
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Fatalf("lock JSON does not end in a newline: %q", first)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(first, &fields); err != nil {
		t.Fatalf("decode lock JSON: %v", err)
	}
	want := []string{"schema_version", "primary", "list_sha256"}
	if len(fields) != len(want) {
		t.Fatalf("lock JSON has fields %v, want exactly %v", fields, want)
	}
	for _, name := range want {
		if _, ok := fields[name]; !ok {
			t.Errorf("lock JSON is missing %q: %s", name, first)
		}
	}
	// The field order is fixed by the struct, so a reader sees the same shape.
	if bytes.Index(first, []byte(`"primary"`)) > bytes.Index(first, []byte(`"list_sha256"`)) {
		t.Errorf("lock JSON is not in its documented order: %s", first)
	}
	if bytes.Contains(first, []byte(`"supplement"`)) {
		t.Errorf("a lock with no supplement named one: %s", first)
	}

	decoded, err := ParseSourceLock(first)
	if err != nil {
		t.Fatalf("ParseSourceLock: %v", err)
	}
	if !sameLock(decoded, lock) {
		t.Fatalf("ParseSourceLock = %+v, want %+v", decoded, lock)
	}
	if _, err := ParseSourceLock([]byte(string(first) + "\n{}")); err == nil {
		t.Error("ParseSourceLock accepted a second document")
	}
	if _, err := ParseSourceLock([]byte(`{"schema_version":2,"primary":{"repository":"v2fly/domain-list-community","commit":"` + fixtureCommit + `","sha256":"` + digest([]byte("x")) + `","entry":"data/cn"},"list_sha256":"` + digest([]byte("x")) + `","future":"value"}`)); err == nil {
		t.Error("ParseSourceLock accepted an unknown field")
	}
	if _, err := ParseSourceLock([]byte("not json")); err == nil {
		t.Error("ParseSourceLock accepted a non-JSON document")
	}
	if _, err := ParseSourceLock(nil); err == nil {
		t.Error("ParseSourceLock accepted an empty document")
	}
}

func TestAV2LockRoundTripsThroughItsOwnEncoding(t *testing.T) {
	lock := SourceLock{
		SchemaVersion: 2,
		Primary: SourceRef{
			Repository: RepositoryChina,
			Commit:     strings.Repeat("a", 40),
			SHA256:     strings.Repeat("b", 64),
			Entries:    ChinaConfEntries,
		},
		Supplement: &SourceRef{
			Repository: RepositoryV2Fly,
			Commit:     strings.Repeat("c", 40),
			SHA256:     strings.Repeat("d", 64),
			Entry:      "data/cn",
			Kinds:      []string{"full", "regexp"},
		},
		ListSHA256: strings.Repeat("e", 64),
	}
	if err := lock.Validate(); err != nil {
		t.Fatalf("a complete v2 lock is refused: %v", err)
	}
	encoded, err := lock.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := ParseSourceLock(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// sameLock, not a spot check on two fields: this document has six, and a
	// round trip that dropped a third of them is the failure being looked for.
	if !sameLock(decoded, lock) {
		t.Fatalf("the round trip lost something: decoded %+v, want %+v", decoded, lock)
	}
}

func TestTheFallbackShapeIsTheSameShapeWithNoSupplement(t *testing.T) {
	// v2fly as the PRIMARY is how a fallback publication records itself, so
	// one document shape has to cover both roles.
	lock := SourceLock{
		SchemaVersion: 2,
		Primary: SourceRef{
			Repository: RepositoryV2Fly,
			Commit:     strings.Repeat("a", 40),
			SHA256:     strings.Repeat("b", 64),
			Entry:      "data/cn",
		},
		ListSHA256: strings.Repeat("e", 64),
	}
	if err := lock.Validate(); err != nil {
		t.Fatalf("a v2 lock describing v2fly as the primary is refused: %v", err)
	}
}

func TestAV1LockIsRefusedAndTheRefusalNamesTheWayOut(t *testing.T) {
	v1 := []byte(`{"schema_version":1,"repository":"v2fly/domain-list-community","commit":"` +
		strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) +
		`","list_sha256":"` + strings.Repeat("e", 64) + `","entry":"data/cn"}`)
	_, err := ParseSourceLock(v1)
	if err == nil {
		t.Fatal("a v1 lock was accepted, and reading it as v2 would make a document mean something it does not say")
	}
	if !strings.Contains(err.Error(), "update-lists --pin-remote") {
		t.Fatalf("the refusal does not say how to get a v2 lock: %v", err)
	}
}

func TestALockFromTheFutureIsNotToldToRePinItAway(t *testing.T) {
	// The advice differs by direction. A v1 lock is behind this build and
	// re-pinning moves it forward; a lock from the future is AHEAD of it, and
	// re-pinning with this build would replace a document that records sources
	// this one cannot name with one that knows less. Telling an operator to
	// re-pin over it would be advice that destroys the thing it is offered as a
	// fix for.
	for _, version := range []string{"3", "99"} {
		t.Run("schema version "+version, func(t *testing.T) {
			document := []byte(`{"schema_version":` + version + `,"primary":{"repository":"v2fly/domain-list-community","commit":"` +
				strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) + `","entry":"data/cn"},"list_sha256":"` +
				strings.Repeat("e", 64) + `"}`)
			_, err := ParseSourceLock(document)
			if err == nil {
				t.Fatalf("a schema version %s lock was accepted by a build that reads version %d", version, schemaVersion)
			}
			message := err.Error()
			if !strings.Contains(message, "schema version "+version) {
				t.Errorf("the refusal does not name the version it found: %v", err)
			}
			if strings.Contains(message, "A v1 lock") {
				t.Errorf("a version %s document is told it is a v1 lock: %v", version, err)
			}
			if strings.Contains(message, "--pin-remote") {
				t.Errorf("a document from the future is told to re-pin over itself: %v", err)
			}
		})
	}
}

func TestAnUnknownFieldInAV2LockIsRefused(t *testing.T) {
	document := []byte(`{"schema_version":2,"primary":{"repository":"felixonmars/dnsmasq-china-list","commit":"` +
		strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) +
		`","entries":["accelerated-domains.china.conf"]},"list_sha256":"` +
		strings.Repeat("e", 64) + `","future_field":true}`)
	if _, err := ParseSourceLock(document); err == nil {
		t.Fatal("a field this build does not understand was dropped while the document still read as a complete pin")
	}
}

func TestThePrimaryArchiveIsFetchedFromTheRepositoryTheLockNames(t *testing.T) {
	// This is the one-source dnsmasq publication -- no supplement -- so it is
	// the case where the repository in the lock, the top-level directory the
	// archive is named after, and the converter that reads it all differ from
	// the v2fly default. A fetch that kept using the package default would
	// verify the primary's digest against the other repository's archive; an
	// extractor that kept using the default root would refuse a real one.
	//
	// It is not the fallback publication: that names v2fly as the primary and is
	// TestASourceWithNoSupplementPublishesOnlyItsOwnRules.
	archive := chinaArchive(t)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryChina: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: archive}},
		RepositoryV2Fly: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: fixtureArchive(t)}},
	})

	got, list, err := Download(context.Background(), remote.client, chinaPinnedLock(t, archive))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if want := "/" + RepositoryChina + "/tar.gz/" + fixtureCommit; !remote.served(want) {
		t.Fatalf("Download fetched %v, want the archive at %q", remote.requests, want)
	}
	// No supplement was named, so nothing may be fetched from the repository
	// that could have supplied one.
	if other := "/" + RepositoryV2Fly + "/tar.gz/" + fixtureCommit; remote.served(other) {
		t.Fatalf("Download fetched %q although the lock names no supplement", other)
	}
	// The upstream address is 203.0.113.7 in every fixture line, so a list
	// carrying one is a converter that published what the pin says it drops.
	want := "# disabled upstream: upstream moved it\n" +
		"\n" +
		"domain:four.cn\ndomain:one.cn\ndomain:three.cn\n"
	if string(list) != want {
		t.Fatalf("Download list =\n%q\nwant\n%q", list, want)
	}
	wantLock := chinaPinnedLock(t, archive)
	wantLock.ListSHA256 = digest(list)
	if !sameLock(got, wantLock) {
		t.Fatalf("Download lock = %+v, want %+v", got, wantLock)
	}
}

func TestDownloadUnionsThePrimaryAndTheSupplement(t *testing.T) {
	// The published list is the union of two repositories: the dnsmasq source's
	// domains plus the 70 v2fly rules its syntax cannot say. The two archives
	// come from different repositories at different commits, and the digest in
	// the returned lock has to cover what was actually published -- the union,
	// not one archive and not the primary.
	//
	// The primary carries a DISABLED entry and a prose header, because that is
	// the shape Task 6 will commit and it is the only source of comments in the
	// published list: the supplement contributes rules, never comments. A union
	// that dropped the comments, moved them into the supplement branch, or
	// re-sorted them together with the merged rules would publish a different
	// list from the same two commits, which is the one thing list_sha256 exists
	// to make impossible.
	primaryCommit := strings.Repeat("a", 40)
	supplementCommit := strings.Repeat("b", 40)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryChina: {head: primaryCommit, archives: map[string][]byte{
			primaryCommit: sourceArchiveFiles(t, RepositoryChina, primaryCommit,
				"accelerated-domains.china.conf", strings.Join([]string{
					// The two lines upstream's files really open with: prose, then
					// rules, then an entry upstream disabled with its reason.
					"# Generated by the upstream project; do not edit by hand",
					"server=/one.cn/203.0.113.7",
					"server=/two.cn/203.0.113.7",
					"#server=/three.cn/203.0.113.7 # Disabled: upstream moved it",
					"",
				}, "\n"),
				"google.china.conf", "server=/dl.google.com/203.0.113.7\n",
				"apple.china.conf", "server=/a1.mzstatic.com/203.0.113.7\n"),
		}},
		RepositoryV2Fly: {head: supplementCommit, archives: map[string][]byte{
			supplementCommit: sourceArchiveFiles(t, RepositoryV2Fly, supplementCommit,
				"data/cn", "include:shared\ndomain:one.cn\nfull:exact.cn\nregexp:^edge\\.cn$\n",
				"data/shared", "full:included.cn\ndomain:included-sub.cn\n"),
		}},
	})

	lock := twoSourceLock(t, remote, primaryCommit, supplementCommit)
	published, list, err := Download(context.Background(), remote.client, lock)
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	// Sorted by expression, so the bytes are a function of the two SETS and not
	// of which archive was converted first: every `domain:` rule comes from the
	// dnsmasq source, and the two kinds it cannot say come from v2fly even
	// though the included list states them and data/cn does not. The comment
	// block and its blank separator come from the primary alone, above them.
	want := "# disabled upstream: upstream moved it\n" +
		"\n" +
		"domain:a1.mzstatic.com\ndomain:dl.google.com\ndomain:one.cn\ndomain:two.cn\n" +
		"full:exact.cn\nfull:included.cn\nregexp:^edge\\.cn$\n"
	if string(list) != want {
		t.Fatalf("the published list is\n%q\nwant\n%q", list, want)
	}
	// The three facts the exact bytes above cannot explain on their own.
	if got := strings.Count(string(list), "disabled upstream"); got != 1 {
		t.Errorf("the list carries %d disabled-upstream lines, want the primary's one:\n%s", got, list)
	}
	if strings.Contains(string(list), "do not edit by hand") {
		t.Errorf("a prose header became a published comment, which is not upstream's record of anything:\n%s", list)
	}
	// The address upstream wrote is dropped everywhere, comment lines included:
	// the reason is free prose and a prose line may quote one.
	if strings.Contains(string(list), "203.0.113.7") {
		t.Errorf("the published list carries an upstream address:\n%s", list)
	}
	if strings.Contains(string(list), "domain:three.cn") {
		t.Errorf("the entry upstream disabled was published as a rule:\n%s", list)
	}
	if published.ListSHA256 != digest(list) {
		t.Fatal("the lock's list digest does not cover the bytes it published")
	}
	// The lock comes back as it went in plus the list digest: a union that
	// quietly dropped the supplement would leave a document that names one
	// source and a list that came from two.
	wantLock := lock
	wantLock.ListSHA256 = digest(list)
	if !sameLock(published, wantLock) {
		t.Fatalf("published lock = %+v, want %+v", published, wantLock)
	}
}

func TestEachSourceIsFetchedFromTheRepositoryItsOwnRefNames(t *testing.T) {
	// Without this, a fetch that always asked for one repository would still
	// pass the union test whenever the two archives happened to agree, and the
	// repository each archive is verified against is the whole of the security
	// claim here: the lock says which bytes are the reviewed ones.
	primaryCommit := strings.Repeat("a", 40)
	supplementCommit := strings.Repeat("b", 40)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryChina: {head: primaryCommit, archives: map[string][]byte{
			primaryCommit: sourceArchiveFiles(t, RepositoryChina, primaryCommit,
				"accelerated-domains.china.conf", "server=/one.cn/203.0.113.7\n",
				"google.china.conf", "server=/g.cn/203.0.113.7\n",
				"apple.china.conf", "server=/a.cn/203.0.113.7\n"),
		}},
		RepositoryV2Fly: {head: supplementCommit, archives: map[string][]byte{
			supplementCommit: sourceArchiveFiles(t, RepositoryV2Fly, supplementCommit,
				"data/cn", "domain:one.cn\nfull:exact.cn\n"),
		}},
	})

	if _, _, err := Download(context.Background(), remote.client,
		twoSourceLock(t, remote, primaryCommit, supplementCommit)); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !remote.served(archivePrefix(RepositoryChina) + primaryCommit) {
		t.Fatalf("the primary archive was not fetched from %s: %v", RepositoryChina, remote.requests)
	}
	if !remote.served(archivePrefix(RepositoryV2Fly) + supplementCommit) {
		t.Fatalf("the supplement archive was not fetched from %s: %v", RepositoryV2Fly, remote.requests)
	}
}

func TestASourceWithNoSupplementPublishesOnlyItsOwnRules(t *testing.T) {
	// The fallback publication takes this shape: v2fly as the primary and no
	// supplement, which is why one lock shape has to cover both roles. Every
	// rule kind it holds is published, because a primary contributes everything
	// its repository holds -- `domain:` is not filtered out of it.
	commit := strings.Repeat("b", 40)
	archive := sourceArchiveFiles(t, RepositoryV2Fly, commit,
		"data/cn", "domain:only.cn\nfull:exact.cn\nregexp:^edge\\.cn$\n")
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryV2Fly: {head: commit, archives: map[string][]byte{commit: archive}},
	})
	pinned, err := ResolvePinned(context.Background(), remote.client, RepositoryV2Fly, commit)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	_, list, err := Download(context.Background(), remote.client,
		SourceLock{SchemaVersion: schemaVersion, Primary: pinned.Primary})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	want := "domain:only.cn\nfull:exact.cn\nregexp:^edge\\.cn$\n"
	if string(list) != want {
		t.Fatalf("the published list is\n%q\nwant\n%q", list, want)
	}
}

func TestEachArchivesDigestIsVerifiedBeforeItIsExtracted(t *testing.T) {
	// Each archive is verified against the digest its OWN ref records, and it is
	// verified before it is unpacked. The fixture archives here are rooted in the
	// wrong repository AND hold a rule no converter here can express, so an
	// implementation that unpacked first would report the archive's root or its
	// unknown directive instead of a digest -- which is the whole difference
	// between "these are not the reviewed bytes" and "these bytes could not be
	// read".
	//
	// It is asked of both sources because a second source is a second archive,
	// and the primary's guarantee is not automatically the supplement's.
	primaryCommit := strings.Repeat("a", 40)
	supplementCommit := strings.Repeat("b", 40)
	// A readable archive for each source, and one that is rooted in the OTHER
	// repository's directory -- which is what a substituted archive looks like,
	// and what extractArchive refuses on its own. Only the source under test
	// gets the unusable one, so the other one converts and the refusal can only
	// be about the archive that was checked.
	readableChina := sourceArchiveFiles(t, RepositoryChina, primaryCommit,
		"accelerated-domains.china.conf", "server=/one.cn/203.0.113.7\n",
		"google.china.conf", "server=/g.cn/203.0.113.7\n",
		"apple.china.conf", "server=/a.cn/203.0.113.7\n")
	readableV2Fly := sourceArchiveFiles(t, RepositoryV2Fly, supplementCommit,
		"data/cn", "domain:one.cn\nfull:exact.cn\n")
	unusableChina := sourceArchiveFiles(t, RepositoryV2Fly, primaryCommit,
		"accelerated-domains.china.conf", "ip4:1.2.3.4\n",
		"google.china.conf", "server=/g.cn/203.0.113.7\n",
		"apple.china.conf", "server=/a.cn/203.0.113.7\n")
	unusableV2Fly := sourceArchiveFiles(t, RepositoryChina, supplementCommit,
		"data/cn", "ip4:1.2.3.4\n")

	tests := []struct {
		name string
		// china and v2fly are what the origin serves for each repository.
		china []byte
		v2fly []byte
		// which says whose digest was replaced, and hands back the one the
		// refusal has to name.
		which func(*SourceLock) string
	}{
		{
			name:  "the primary's",
			china: unusableChina,
			v2fly: readableV2Fly,
			which: func(l *SourceLock) string {
				l.Primary.SHA256 = digest([]byte("a different primary archive"))
				return l.Primary.SHA256
			},
		},
		{
			name:  "a supplement's",
			china: readableChina,
			v2fly: unusableV2Fly,
			which: func(l *SourceLock) string {
				l.Supplement.SHA256 = digest([]byte("a different supplement archive"))
				return l.Supplement.SHA256
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// One commit per repository, and it is the one that repository is
			// pinned at: an origin that also held the other repository's commit
			// would answer a fetch for the wrong one, which is the laxness the
			// per-repository fixture exists to remove.
			remote := newFakeRemoteSources(t, map[string]fakeSource{
				RepositoryChina: {head: primaryCommit, archives: map[string][]byte{
					primaryCommit: test.china,
				}},
				RepositoryV2Fly: {head: supplementCommit, archives: map[string][]byte{
					supplementCommit: test.v2fly,
				}},
			})
			lock := twoSourceLock(t, remote, primaryCommit, supplementCommit)
			wantDigest := test.which(&lock)

			lockReturned, list, err := Download(context.Background(), remote.client, lock)
			if err == nil {
				t.Fatalf("Download accepted a mismatched archive and returned %q", list)
			}
			if list != nil || !sameLock(lockReturned, SourceLock{}) {
				t.Fatalf("Download returned something on failure: lock %+v, list %q", lockReturned, list)
			}
			if !strings.Contains(err.Error(), "archive sha256") || !strings.Contains(err.Error(), wantDigest) {
				t.Fatalf("error = %v, want it to name %s pinned archive digest %s", test.name, wantDigest, err)
			}
			if strings.Contains(err.Error(), "top-level directory") {
				t.Fatalf("error came from extraction, so the digest was not checked first: %v", err)
			}
			if strings.Contains(err.Error(), "ip4") {
				t.Fatalf("error came from conversion, so the archive was not checked first: %v", err)
			}
		})
	}
}

func TestThePublishedBytesDoNotDependOnTheOrderTheTwoSourcesWereConvertedIn(t *testing.T) {
	// The digest in the lock is a promise about bytes, and those bytes have to
	// be a function of the two sets rather than of the order the archives were
	// read in: two runs that fetched the same two commits could otherwise
	// publish different lists beside the same list_sha256.
	first := []string{"domain:b.cn", "domain:a.cn", "full:exact.cn"}
	second := []string{"full:included.cn", "domain:a.cn", "regexp:^edge\\.cn$"}
	want := []string{"domain:a.cn", "domain:b.cn", "full:exact.cn", "full:included.cn", "regexp:^edge\\.cn$"}

	forward := mergeExpressions(first, second)
	backward := mergeExpressions(second, first)
	if strings.Join(forward, "\n") != strings.Join(want, "\n") {
		t.Fatalf("merged = %q, want %q", forward, want)
	}
	if strings.Join(backward, "\n") != strings.Join(want, "\n") {
		t.Fatalf("merged the other way round = %q, want %q", backward, want)
	}
	// A rule both sources state appears once. The dnsmasq source and v2fly carry
	// many of the same domains, and the committed-list gate -- firstDuplicate in
	// committed_test.go, which refuses a repeated line -- is what the overlap
	// would fail there; the matcher would not care either way.
	if len(forward) != len(want) {
		t.Fatalf("the union repeats a rule both sources state: %q", forward)
	}
	// Neither input is rewritten: the converter's own slice belongs to the
	// primary, and sorting in place would reorder what the caller handed over.
	if strings.Join(first, ",") != "domain:b.cn,domain:a.cn,full:exact.cn" {
		t.Fatalf("mergeExpressions wrote to the first set: %q", first)
	}
}

// twoSourceLock resolves both fixtures through the origin and returns the lock
// the published list derives from: the dnsmasq source as the primary, and the
// 70 rules its syntax cannot say as a supplement.
//
// The supplement's kinds come from SupplementKinds, never from the exported
// slice: a lock that shared the backing array of a package-level value would
// hand its holder a way to rewrite the reviewed set in place, and that defence
// has a test of its own.
func twoSourceLock(t *testing.T, remote *fakeRemote, primaryCommit, supplementCommit string) SourceLock {
	t.Helper()
	primary, err := ResolvePinned(context.Background(), remote.client, RepositoryChina, primaryCommit)
	if err != nil {
		t.Fatalf("resolve the primary: %v", err)
	}
	supplement, err := ResolvePinned(context.Background(), remote.client, RepositoryV2Fly, supplementCommit)
	if err != nil {
		t.Fatalf("resolve the supplement: %v", err)
	}
	ref := supplement.Primary
	ref.Kinds = SupplementKinds()
	return SourceLock{SchemaVersion: schemaVersion, Primary: primary.Primary, Supplement: &ref}
}

func TestTheFallbackPublicationHasNothingToMerge(t *testing.T) {
	// The fallback publication is v2fly as the primary and no supplement: there
	// is nothing to add to it, because v2fly already holds every rule the
	// supplement exists to carry. A lock naming v2fly twice is refused, and the
	// refusal is about the document rather than about the two repositories
	// being equal: a supplement that re-reads the same repository attributes
	// `full:`/`regexp:` rules to a second source that already carried them,
	// which is exactly the untrue-document failure the kinds allowlist exists to
	// prevent -- except here the primary is the one carrying every kind, so no
	// `kinds` value could ever make it true.
	//
	// The second case is the other half: the fallback itself has to keep
	// working, or this refusal would be a refusal of the shape the committed
	// pair is written in.
	tests := []struct {
		name       string
		supplement *SourceRef
		wantErr    bool
	}{
		{
			name:       "no supplement at all",
			supplement: nil,
		},
		{
			name:       "the same repository as a supplement",
			supplement: &SourceRef{Repository: RepositoryV2Fly, Commit: strings.Repeat("c", 40), SHA256: strings.Repeat("d", 64), Entry: V2FlyEntry, Kinds: SupplementKinds()},
			wantErr:    true,
		},
		{
			// The dnsmasq source cannot be a supplement at all, so this one is
			// refused for that reason rather than for this one. It is here to
			// keep the refusal from being read as "the repository is the rule".
			name:       "the other reviewed repository as a supplement",
			supplement: &SourceRef{Repository: RepositoryChina, Commit: strings.Repeat("c", 40), SHA256: strings.Repeat("d", 64), Entries: ChinaConfEntries, Kinds: SupplementKinds()},
			wantErr:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lock := SourceLock{
				SchemaVersion: schemaVersion,
				Primary:       pinnedLock(t, fixtureArchive(t)).Primary,
				Supplement:    test.supplement,
				ListSHA256:    strings.Repeat("e", 64),
			}
			err := lock.Validate()
			if test.wantErr {
				if err == nil {
					t.Fatalf("a lock with a v2fly primary and %v was accepted", test.supplement)
				}
				return
			}
			if err != nil {
				t.Fatalf("the fallback publication is refused: %v", err)
			}
		})
	}
}

func TestASupplementIsRefusedWhenThePrimaryAlreadyCarriesItsKinds(t *testing.T) {
	// The v2fly primary holds every rule kind there is, so a supplement beside
	// it contributes nothing and the document claims otherwise. Checked on the
	// lock Download is given, not only on Validate, because Download is the
	// function that would publish it.
	archive := fixtureArchive(t)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryV2Fly: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: archive}},
		RepositoryChina: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: chinaArchive(t)}},
	})
	lock := twoSourceLock(t, remote, fixtureCommit, fixtureCommit)
	lock.Primary = pinnedLock(t, archive).Primary
	resolved := len(remote.requests)

	lockReturned, list, err := Download(context.Background(), remote.client, lock)
	if err == nil {
		t.Fatalf("Download accepted a v2fly primary with a supplement and returned %q", list)
	}
	if list != nil || !sameLock(lockReturned, SourceLock{}) {
		t.Fatalf("Download returned something on failure: lock %+v, list %q", lockReturned, list)
	}
	if len(remote.requests) != resolved {
		t.Fatalf("Download fetched %v before refusing the lock", remote.requests[resolved:])
	}
}

func TestASupplementNamesOnlyTheKindsThePrimaryCannotCarry(t *testing.T) {
	// The published list is the union of two repositories, and a supplement
	// says which of its rules went into it. Naming a kind the primary already
	// carries makes the document untrue about how the list was built: it claims
	// `domain:` rules came from v2fly when the dnsmasq source held every one of
	// them, so a later reader comparing the two sources would count the same
	// domains twice and call it agreement.
	supplement := func(kinds []string) SourceLock {
		return SourceLock{
			SchemaVersion: schemaVersion,
			Primary:       chinaPinnedLock(t, []byte("x")).Primary,
			Supplement: &SourceRef{
				Repository: RepositoryV2Fly,
				Commit:     strings.Repeat("c", 40),
				SHA256:     strings.Repeat("d", 64),
				Entry:      V2FlyEntry,
				Kinds:      kinds,
			},
			ListSHA256: strings.Repeat("e", 64),
		}
	}
	tests := []struct {
		name       string
		kinds      []string
		wantErr    bool
		wantSubstr string
	}{
		{name: "the reviewed kinds", kinds: []string{"full", "regexp"}},
		{name: "one of them", kinds: []string{"regexp"}},
		{name: "a kind the primary carries", kinds: []string{"full", "domain"}, wantErr: true, wantSubstr: `"domain"`},
		{name: "a kind this build does not know", kinds: []string{"ipcidr"}, wantErr: true, wantSubstr: `"ipcidr"`},
		{name: "no kinds at all", kinds: nil, wantErr: true, wantSubstr: "records which rule kinds"},
		{name: "an empty set", kinds: []string{}, wantErr: true, wantSubstr: "records which rule kinds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := supplement(test.kinds).Validate()
			if test.wantErr {
				if err == nil {
					t.Fatalf("a supplement naming %v was accepted", test.kinds)
				}
				if !strings.Contains(err.Error(), test.wantSubstr) {
					t.Fatalf("error = %v, want it to mention %q", err, test.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("a supplement naming %v was refused: %v", test.kinds, err)
			}
		})
	}
}

func TestAPrimaryCannotSayWhichKindsItContributes(t *testing.T) {
	// `kinds` is a supplement's field: it narrows what a SECOND source adds. On
	// the primary it would mean "these and only these rules came from here",
	// which is a claim about the conversion rather than a description of the
	// source -- and one nothing here could check, because the primary contributes
	// every rule its repository holds.
	lock := SourceLock{
		SchemaVersion: schemaVersion,
		Primary:       chinaPinnedLock(t, []byte("x")).Primary,
		ListSHA256:    strings.Repeat("e", 64),
	}
	lock.Primary.Kinds = []string{"domain"}
	if err := lock.Validate(); err == nil {
		t.Fatal("a primary naming the kinds it contributes was accepted")
	}
}

func TestALockHoldingTheSupplementKindsDoesNotHoldTheExportedSet(t *testing.T) {
	// The reviewed kinds are a package-level slice, and a lock that shared their
	// backing array would hand a caller a way to rewrite the package's own
	// constant through the lock -- and through the next lock built from it. The
	// primary's entry set is copied for the same reason, and the two sets end up
	// in the same document.
	kinds := SupplementKinds()
	kinds[0] = "domain"
	if V2FlySupplementKinds[0] != "full" {
		t.Fatalf("the exported set was written through a lock: %v", V2FlySupplementKinds)
	}
}

func TestASupplementHasToBeTheOneRepositoryThatMaySupplement(t *testing.T) {
	// The dnsmasq source is the primary or it is nothing: it cannot be the 70
	// extra rules of a v2fly publication, because those rules are its own kind
	// and merging it into itself would convert the same domains twice.
	lock := SourceLock{
		SchemaVersion: schemaVersion,
		Primary:       chinaPinnedLock(t, []byte("x")).Primary,
		Supplement: &SourceRef{
			Repository: RepositoryChina,
			Commit:     strings.Repeat("c", 40),
			SHA256:     strings.Repeat("d", 64),
			Entries:    ChinaConfEntries,
			Kinds:      SupplementKinds(),
		},
		ListSHA256: strings.Repeat("e", 64),
	}
	if err := lock.Validate(); err == nil {
		t.Fatal("the primary was accepted as its own supplement")
	}
}

func TestResolveCommitNamesTheEntriesItsRepositoryConverts(t *testing.T) {
	// The lock a pin writes has to say what will be read, or a later run is
	// guessing: a commit and a repository alone do not say whether the source is
	// one file or three. The origin answers for both repositories, because an
	// origin that knows only one would refuse the other's HEAD and the test
	// would be about the fixture rather than about the lock.
	archive := fixtureArchive(t)
	remote := newFakeRemoteSources(t, map[string]fakeSource{
		RepositoryV2Fly: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: archive}},
		RepositoryChina: {head: fixtureCommit, archives: map[string][]byte{fixtureCommit: chinaArchive(t)}},
	})

	china, err := ResolveCommit(context.Background(), remote.client, RepositoryChina, "")
	if err != nil {
		t.Fatalf("ResolveCommit(china): %v", err)
	}
	if china.Primary.Repository != RepositoryChina || !slices.Equal(china.Primary.Entries, ChinaConfEntries) || china.Primary.Entry != "" {
		t.Fatalf("the dnsmasq lock does not name its three files: %+v", china.Primary)
	}
	if want := "/repos/" + RepositoryChina + "/commits/HEAD"; !remote.served(want) {
		t.Fatalf("ResolveCommit fetched %v, want %q", remote.requests, want)
	}

	v2fly, err := ResolveCommit(context.Background(), remote.client, RepositoryV2Fly, fixtureCommit)
	if err != nil {
		t.Fatalf("ResolveCommit(v2fly): %v", err)
	}
	if v2fly.Primary.Entry != V2FlyEntry || len(v2fly.Primary.Entries) != 0 {
		t.Fatalf("the v2fly lock does not name its one file: %+v", v2fly.Primary)
	}
}

func TestAFetchRefusesARepositoryTheAllowlistDoesNotHoldBeforeBuildingAURL(t *testing.T) {
	// The repository becomes a path under a fixed host, so the name is checked
	// against the allowlist before it is concatenated rather than after: a name
	// is not a URL fragment, and this is the one function every fetch goes
	// through.
	//
	// Both refs are exercised because both reach this function. Once the fetch
	// took a lock rather than a ref it read lock.Primary, which would leave the
	// supplement's repository reaching the URL ungated -- a gate that checks
	// one ref and is handed two is a gate on one of them.
	archive := fixtureArchive(t)
	remote := newFakeRemote(t, fixtureCommit, archive)

	tests := []struct {
		name string
		ref  *SourceRef
	}{
		{name: "a primary's", ref: &SourceRef{Repository: "attacker/domain-list", Commit: fixtureCommit, Entries: ChinaConfEntries}},
		{name: "a supplement's", ref: &SourceRef{Repository: "attacker/domain-list", Commit: fixtureCommit, Entry: V2FlyEntry, Kinds: SupplementKinds()}},
	}
	for _, test := range tests {
		if _, err := fetchArchive(context.Background(), remote.client, test.ref); err == nil {
			t.Errorf("fetchArchive requested an archive for %s repository nobody reviewed", test.name)
		} else if !strings.Contains(err.Error(), "attacker/domain-list") {
			t.Errorf("the refusal for %s repository does not name it: %v", test.name, err)
		}
	}
	if len(remote.requests) != 0 {
		t.Fatalf("an unreviewed repository reached the network: %v", remote.requests)
	}
}

// zeroReader streams count zero bytes without allocating them.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
