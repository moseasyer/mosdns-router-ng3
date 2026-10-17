package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"mosdns-router/internal/config"
	"mosdns-router/internal/filelock"
	"mosdns-router/internal/rules"
)

// Every test here runs the real command against a fake GitHub origin: the
// command builds its real api.github.com and codeload.github.com URLs and the
// injected client only rewrites where they are sent. Nothing reaches the network,
// and the files it publishes are the ones a real pin would publish.

const (
	// lockedCommit is the commit a published pair in these fixtures claims.
	lockedCommit = "1111111111111111111111111111111111111111"
	// remoteCommit is the commit the fake origin publishes for HEAD.
	remoteCommit = "2222222222222222222222222222222222222222"
	// lockedSupplementCommit is the commit a published pair's SUPPLEMENT claims.
	// It is a commit of its own repository and shares no value with the primary's,
	// so a case that puts the wrong one in a lock cannot pass by accident.
	lockedSupplementCommit = "4444444444444444444444444444444444444444"
	// remoteSupplementCommit is the commit the fake origin publishes for the
	// SUPPLEMENT repository's HEAD, for the cases that have to tell the two roles
	// apart.
	//
	// It exists because serving both repositories at remoteCommit makes a request
	// for one of their archives indistinguishable from a request for the other's:
	// "was the v2fly archive fetched" then cannot say whether it was fetched as the
	// primary or as the supplement. With the repositories on different commits it
	// can -- asking the v2fly repository for the primary's commit is a role
	// confusion and nothing else -- so the cases that need the distinction serve
	// the supplement at this commit instead.
	remoteSupplementCommit = "5555555555555555555555555555555555555555"
	// firstInstallArchive is the archive served for a first pin.
	firstInstallArchive = "domain:first.cn\nfull:exact.cn\ninclude:shared\n"
	// shared is the list the archive's China list includes.
	sharedList = "regexp:^static[0-9]+\\.example\\.cn$\n"
)

// fakeSource is one repository the origin will answer for.
type fakeSource struct {
	// head is what /commits/HEAD answers for this repository.
	head string
	// archives is what the origin holds for this repository, keyed by commit.
	archives map[string][]byte
}

// fakeOrigin is a GitHub-shaped test origin: it publishes one commit for HEAD,
// and serves a source archive only for the commits it holds one for.
//
// It answers for SEVERAL repositories rather than one. An origin that knows a
// single repository's tar.gz prefix cannot observe a request for another one at
// all -- the request 404s and the test passes either way -- which is how a
// fixture ends up modelling a machine that has only ever had one source.
type fakeOrigin struct {
	server *httptest.Server
	client *http.Client
	// sources is what the origin can serve, keyed by repository.
	sources  map[string]fakeSource
	status   int
	requests []string
	// onServe runs before each answer, so a case can make the origin answer
	// differently the second time it is asked. Upstream moving during a run is not
	// something a static fixture can otherwise express, and it is exactly the race the
	// automatic refresh has to be immune to. It gets the request because "the third
	// time it is asked for HEAD" is the only way to say WHEN upstream moved, and a
	// hook that could only flip on the first request would move it before the run had
	// measured anything -- which is a different scenario that passes either way.
	onServe func(*fakeOrigin, *http.Request)
}

func newFakeOrigin(t *testing.T, head string, archives map[string][]byte) *fakeOrigin {
	t.Helper()
	return newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryV2Fly: {head: head, archives: archives},
	})
}

func newFakeOriginSources(t *testing.T, sources map[string]fakeSource) *fakeOrigin {
	t.Helper()
	origin := &fakeOrigin{sources: make(map[string]fakeSource, len(sources)), status: http.StatusOK}
	for repository, source := range sources {
		origin.sources[repository] = source
	}
	origin.server = httptest.NewServer(http.HandlerFunc(origin.serve))
	t.Cleanup(origin.server.Close)
	origin.client = rewritingClient(t, origin.server.URL)
	return origin
}

// setArchives replaces what the origin can serve for one repository, so a case
// that arranges a different archive does not have to reach into a map it does
// not own.
func (o *fakeOrigin) setArchives(repository string, archives map[string][]byte) {
	source := o.sources[repository]
	source.archives = archives
	o.sources[repository] = source
}

// setHead replaces the commit the origin publishes for one repository's default
// branch.
func (o *fakeOrigin) setHead(repository, head string) {
	source := o.sources[repository]
	source.head = head
	o.sources[repository] = source
}

func (o *fakeOrigin) serve(writer http.ResponseWriter, request *http.Request) {
	o.requests = append(o.requests, request.URL.Path)
	if o.onServe != nil {
		o.onServe(o, request)
	}
	if o.status != http.StatusOK {
		writer.WriteHeader(o.status)
		return
	}
	repository := repositoryOfPath(request.URL.Path)
	if repository == "" {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if commit, isArchive := strings.CutPrefix(request.URL.Path, archivePrefix(repository)); isArchive {
		archive, known := o.sources[repository].archives[commit]
		if !known {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/gzip")
		writer.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		_, _ = writer.Write(archive)
		return
	}
	if request.URL.Path != o.commitPath(repository) {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(writer, fmt.Sprintf(`{"sha":%q,"node_id":"C_1","parents":[]}`, o.sources[repository].head))
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
func repositoryOfPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
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

func (o *fakeOrigin) fetched(path string) bool {
	for _, requested := range o.requests {
		if requested == path {
			return true
		}
	}
	return false
}

func (o *fakeOrigin) archivePath(repository, commit string) string {
	return archivePrefix(repository) + commit
}

func (o *fakeOrigin) commitPath(repository string) string {
	return "/repos/" + repository + "/commits/HEAD"
}

// rewritingClient sends every request to the test origin, so the command under
// test keeps building its real GitHub URLs.
func rewritingClient(t *testing.T, serverURL string) *http.Client {
	t.Helper()
	target, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: rewriteTo{target: target, inner: &http.Transport{}}}
}

type rewriteTo struct {
	target *url.URL
	inner  *http.Transport
}

func (r rewriteTo) RoundTrip(request *http.Request) (*http.Response, error) {
	next := request.Clone(request.Context())
	next.URL.Scheme = r.target.Scheme
	next.URL.Host = r.target.Host
	return r.inner.RoundTrip(next)
}

// publishChinaPair writes a valid lock whose PRIMARY is the dnsmasq source, so
// a test starts from a machine pinned to the other reviewed source.
//
// It exists because publishPair is v2fly at all 25 of its call sites: a fixture
// that only ever holds one repository cannot observe whether the command asked
// that repository or the other one, so every check and refresh test would pass
// identically whichever the code named.
func publishChinaPair(t *testing.T, paths listPaths, commit string, list []byte) {
	t.Helper()
	writePublishedPair(t, paths, rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryChina,
			Commit:     commit,
			SHA256:     sha256Hex([]byte("the archive " + commit)),
			Entries:    append([]string(nil), rules.ChinaConfEntries...),
		},
		ListSHA256: sha256Hex(list),
	}, list)
}

// publishChinaPairWithSupplement writes the pair a machine carries once the China
// list derives from TWO sources: the dnsmasq primary, and the v2fly supplement that
// carries the rules its syntax cannot express.
//
// publishChinaPair cannot express that state -- a v2 lock's supplement field is the
// whole difference -- and every guardrail case starts from it, because the two counts
// being compared are the subject of those cases.
func publishChinaPairWithSupplement(t *testing.T, paths listPaths, primaryCommit, supplementCommit string, list []byte) {
	t.Helper()
	writePublishedPair(t, paths, rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryChina,
			Commit:     primaryCommit,
			SHA256:     sha256Hex([]byte("the archive " + primaryCommit)),
			Entries:    append([]string(nil), rules.ChinaConfEntries...),
		},
		Supplement: &rules.SourceRef{
			Repository: rules.RepositoryV2Fly,
			Commit:     supplementCommit,
			SHA256:     sha256Hex([]byte("the supplement archive " + supplementCommit)),
			Entry:      rules.V2FlyEntry,
			// The kinds are copied rather than aliased, for the reason
			// rules.SupplementKinds exists: a lock that shared the backing array of
			// the package's own set could be rewritten through it.
			Kinds: rules.SupplementKinds(),
		},
		ListSHA256: sha256Hex(list),
	}, list)
}

// writePublishedPair writes a lock and the list it describes, so every fixture that
// starts a case from an installed state writes the same two files the same way.
func writePublishedPair(t *testing.T, paths listPaths, lock rules.SourceLock, list []byte) {
	t.Helper()
	encoded, err := lock.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.sourceLock, encoded, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.listFile, list, 0o640); err != nil {
		t.Fatal(err)
	}
}

// publishChinaPairFromOrigin writes the pair a machine carries when every archive
// digest in the lock is one the origin can actually serve.
//
// publishChinaPair and its siblings write digests that match nothing, which is
// harmless while every case resolves a commit the lock does not name -- the fiction
// is never checked. It stops being harmless the moment a command re-fetches a commit
// the lock ALREADY names, which is what carrying a supplement across a pin does, so
// this fixture derives both digests from the archives themselves.
func publishChinaPairFromOrigin(t *testing.T, paths listPaths, primary, supplement fakeSource, list []byte) {
	t.Helper()
	writePublishedPair(t, paths, rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryChina,
			Commit:     primary.head,
			SHA256:     sha256Hex(oneArchive(t, "primary", primary)),
			Entries:    append([]string(nil), rules.ChinaConfEntries...),
		},
		Supplement: &rules.SourceRef{
			Repository: rules.RepositoryV2Fly,
			Commit:     supplement.head,
			SHA256:     sha256Hex(oneArchive(t, "supplement", supplement)),
			Entry:      rules.V2FlyEntry,
			Kinds:      rules.SupplementKinds(),
		},
		ListSHA256: sha256Hex(list),
	}, list)
}

// oneArchive is the single archive a fixture source holds, which is all of them.
func oneArchive(t *testing.T, role string, source fakeSource) []byte {
	t.Helper()
	if len(source.archives) != 1 {
		t.Fatalf("the %s source holds %d archives, want exactly 1", role, len(source.archives))
	}
	for _, archive := range source.archives {
		return archive
	}
	return nil
}

// publishV1Pair writes the pair a machine carried BEFORE this build existed: a
// schema version 1 lock and the list it describes.
//
// It exists because the deadlock it models is the whole point. A v1 lock is
// refused by ParseSourceLock, and the refusal tells the operator to run
// `update-lists --pin-remote`. If that command also refuses a v1 lock on disk,
// the message sends the operator into a command that cannot succeed, and the
// only way out is editing a file by hand. No other fixture can produce this
// state: publishPair writes v2 at all 25 of its call sites.
func publishV1Pair(t *testing.T, paths listPaths, commit string, list []byte) {
	t.Helper()
	lock := `{
  "schema_version": 1,
  "repository": "` + rules.RepositoryV2Fly + `",
  "commit": "` + commit + `",
  "sha256": "` + sha256Hex([]byte("the archive "+commit)) + `",
  "list_sha256": "` + sha256Hex(list) + `",
  "entry": "` + rules.V2FlyEntry + `"
}
`
	if err := os.WriteFile(paths.sourceLock, []byte(lock), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.listFile, list, 0o640); err != nil {
		t.Fatal(err)
	}
}

// sourceArchive builds a source archive for a commit, carrying the lists it is
// given under the single top-level directory GitHub names them with.
func sourceArchive(t *testing.T, commit, cnList string) []byte {
	t.Helper()
	return sourceArchiveUnder(t, "domain-list-community-"+commit, map[string]string{
		"data/cn":     cnList,
		"data/shared": sharedList,
		"README.md":   "# fixture\n",
	})
}

// chinaSourceArchive builds a dnsmasq source archive for a commit: the three
// files the pin names, under the top-level directory THAT repository's source
// archive is named after. The address is not the one upstream really uses, so a
// converter that published it could not pass by publishing the right value.
func chinaSourceArchive(t *testing.T, commit, domains string) []byte {
	t.Helper()
	// The fixture argument is a MOSDNS list; it is turned back into the dnsmasq
	// lines upstream writes, so a case can state the domains it expects to end
	// up published without restating them in the other syntax.
	var primary strings.Builder
	for _, domain := range strings.Split(strings.TrimSuffix(domains, "\n"), "\n") {
		if domain == "" {
			continue
		}
		primary.WriteString("server=/" + strings.TrimPrefix(domain, "domain:") + "/203.0.113.7\n")
	}
	files := map[string]string{
		rules.ChinaConfEntries[0]: primary.String(),
		rules.ChinaConfEntries[1]: "server=/apple.cn/203.0.113.7\n",
		rules.ChinaConfEntries[2]: "server=/google.cn/203.0.113.7\n",
	}
	return sourceArchiveUnder(t, "dnsmasq-china-list-"+commit, files)
}

// sourceArchiveUnder builds a source archive whose single top-level directory is
// root, which is what proves an extractor that looks for the wrong repository's
// directory refuses it rather than converting a subset of it.
func sourceArchiveUnder(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, body := range files {
		header := &tar.Header{
			Name:     root + "/" + name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tarWriter, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// listPaths is where a test's command writes, and what it then inspects.
type listPaths struct {
	dir         string
	sourceLock  string
	listFile    string
	controlLock string
	// previousLock is the archive a pin writes the outgoing pin to. It is a field of
	// the fixture because the production default is /var/lib/mosdns/lists, and a test
	// that ran a pin without overriding it tried to write there -- which is the same
	// mistake render's fixtures made with /usr/lib/systemd/system, and the reason this
	// fixture hands every path to the command rather than letting it pick one.
	previousLock string
	lockContents func() []byte
	listContents func() []byte
}

func newListPaths(t *testing.T) listPaths {
	t.Helper()
	dir := t.TempDir()
	return listPaths{
		dir:          dir,
		sourceLock:   filepath.Join(dir, "source-lock.json"),
		listFile:     filepath.Join(dir, "cn-domains.txt"),
		controlLock:  filepath.Join(dir, "control.lock"),
		previousLock: filepath.Join(dir, "source-lock.previous.json"),
		lockContents: func() []byte { return mustReadFile(t, filepath.Join(dir, "source-lock.json")) },
		listContents: func() []byte { return mustReadFile(t, filepath.Join(dir, "cn-domains.txt")) },
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return contents
}

// publishPair writes a valid lock and list, so a test starts from an installed
// state rather than a first install.
func publishPair(t *testing.T, paths listPaths, commit string, list []byte) {
	t.Helper()
	writePublishedPair(t, paths, rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryV2Fly,
			Commit:     commit,
			SHA256:     sha256Hex([]byte("the archive " + commit)),
			Entry:      rules.V2FlyEntry,
		},
		ListSHA256: sha256Hex(list),
	}, list)
}

// supplementArchive builds a v2fly source archive whose China list contributes
// exactly count rules of the kinds a supplement may contribute.
//
// The `domain:` line is what a China list normally opens with; it is dropped by the
// kinds filter, so it never reaches a published list and the case can be read as
// "this is a China list of which these few lines are the supplement's part". A
// supplement that resolved to nothing at all would be a different failure -- one the
// converter refuses rather than one the guardrail measures.
func supplementArchive(t *testing.T, commit string, count int) []byte {
	t.Helper()
	var body strings.Builder
	body.WriteString("domain:supplement.cn\n")
	for index := 0; index < count; index++ {
		fmt.Fprintf(&body, "full:exact%03d.cn\n", index)
	}
	return sourceArchive(t, commit, body.String())
}

// supplementRules is the contribution supplementArchive puts at its own count into a
// published list, rendered as the lines a reader would find there.
func supplementRules(count int) []byte {
	var list bytes.Buffer
	for index := 0; index < count; index++ {
		fmt.Fprintf(&list, "full:exact%03d.cn\n", index)
	}
	return list.Bytes()
}

// services points the command at a fake origin and nothing else: the control
// lock, the publication and the conversion are the real ones.
func (o *fakeOrigin) services() services {
	services := productionServices()
	services.newHTTPClient = func() *http.Client { return o.client }
	return services
}

// pinServices is what a hand-run `--pin-remote` is given, and it differs from
// services in one field.
//
// A pin now reads the policy to know which of the primary source's files to convert,
// so it cannot be run without one it can read -- the same way `--automatic` always
// could not. Without this injection every pin case would read the HOST's
// /etc/mosdns/policy.yaml: it would pass on a developer machine that has one, fail
// on a machine that does not, and change meaning with whatever that file happens to
// say. A case is about the repository, the supplement or the archive ordering, not
// about the switches, so the answer for all of them is config.Defaults(): no switch
// named, every entry converted, which is exactly what each of them was written
// against. A case that IS about the switches sets its own policy -- see
// TestAPinPublishesOnlyTheEntriesTheOperatorLeftOn.
func (o *fakeOrigin) pinServices() services {
	services := o.services()
	services.loadPolicy = func(string) (config.Policy, error) { return config.Defaults(), nil }
	return services
}

// snapshot records the directory so a test can prove a command wrote nothing.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	return snapshotWithout(t, dir, "")
}

// snapshotWithout records the directory, leaving out one file. A pin creates the
// control lock file even when it publishes nothing, so the failure cases look at
// the published pair and ignore that file.
func snapshotWithout(t *testing.T, dir, except string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.Name() == except {
			continue
		}
		// Directories are walked rather than read. The render fixture publishes a
		// systemd unit now, so the tree it snapshots has a subdirectory in it, and
		// os.ReadFile on one is "is a directory" -- a failure that reads as the
		// snapshot being broken when it is the tree that grew a level.
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			for name, contents := range snapshotTree(t, path) {
				state[entry.Name()+"/"+name] = contents
			}
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		state[entry.Name()] = contents
	}
	return state
}

func assertUnchanged(t *testing.T, before map[string][]byte, dir, except string) {
	t.Helper()
	after := snapshotWithout(t, dir, except)
	if len(after) != len(before) {
		t.Fatalf("directory changed: %v, want %v", names(after), names(before))
	}
	for name, contents := range before {
		if !bytes.Equal(after[name], contents) {
			t.Fatalf("%s changed:\n got: %q\nwant: %q", name, after[name], contents)
		}
	}
}

func names(state map[string][]byte) []string {
	list := make([]string, 0, len(state))
	for name := range state {
		list = append(list, name)
	}
	sort.Strings(list)
	return list
}

func runCLI(t *testing.T, services services, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWith(append([]string{"update-lists"}, args...), &stdout, &stderr, services)
	return code, stdout.String(), stderr.String()
}

func TestUpdateListsCheckReportsAnUnchangedSourceWithoutFetchingTheArchive(t *testing.T) {
	// A daily check must be cheap and silent about files: when the remote still
	// publishes the locked commit, the report is made from the commit document
	// alone. Fetching an archive here would make every check download one, and
	// writing anything would turn a check into an update.
	paths := newListPaths(t)
	published := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, published)
	before := snapshot(t, paths.dir)
	origin := newFakeOrigin(t, lockedCommit, map[string][]byte{lockedCommit: sourceArchive(t, lockedCommit, firstInstallArchive)})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	for _, want := range []string{lockedCommit, "up-to-date: true", "repository: " + rules.RepositoryV2Fly, "entry: " + rules.V2FlyEntry} {
		if !strings.Contains(stdout, want) {
			t.Errorf("check output does not report %q:\n%s", want, stdout)
		}
	}
	if origin.fetched(origin.archivePath(rules.RepositoryV2Fly, lockedCommit)) {
		t.Error("check downloaded the archive although the locked commit is unchanged")
	}
	if stderr != "" {
		t.Errorf("check wrote a diagnostic: %q", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

func TestUpdateListsCheckReportsDriftWithTheRemoteArchiveDigest(t *testing.T) {
	// The remote has moved on. The report has to name the commit the gateway
	// runs on and the commit the remote publishes, plus the remote archive's
	// digest, and it still has to write nothing: accepting a new source is an
	// explicit pin.
	paths := newListPaths(t)
	published := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, published)
	before := snapshot(t, paths.dir)
	archive := sourceArchive(t, remoteCommit, firstInstallArchive)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: archive})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	for _, want := range []string{
		"locked-commit: " + lockedCommit,
		"remote-commit: " + remoteCommit,
		"remote-archive-sha256: " + sha256Hex(archive),
		"up-to-date: false",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("drift report does not include %q:\n%s", want, stdout)
		}
	}
	if !origin.fetched(origin.archivePath(rules.RepositoryV2Fly, remoteCommit)) {
		t.Errorf("drift report did not read the remote archive: %v", origin.requests)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// The published China list is built from TWO repositories, so the report has to name
// both of them.
//
// The primary half is not the whole subject: a lock that names a supplement carries
// that supplement's commit, and a report that printed only the primary would be a
// complete-looking account of half the sources the list came from. Every line of the
// supplement's identity is read off the lock on disk, so what this asserts is what a
// reader can check the machine against.
func TestUpdateListsCheckReportsTheSupplementTheLockNames(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(200), supplementRules(2)...)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, published)
	before := snapshot(t, paths.dir)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: lockedSupplementCommit,
			archives: map[string][]byte{
				lockedSupplementCommit: supplementArchive(t, lockedSupplementCommit, 2),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	for _, want := range []string{
		"supplement-present: true",
		"supplement-repository: " + rules.RepositoryV2Fly,
		"supplement-commit: " + lockedSupplementCommit,
		"supplement-kinds: " + strings.Join(rules.SupplementKinds(), " "),
		"supplement-remote-commit: " + lockedSupplementCommit,
		"up-to-date: true",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("check output does not report %q:\n%s", want, stdout)
		}
	}
	// The supplement's archive digest is the lock's own: nothing moved, so nothing
	// was downloaded to compute a new one.
	if !strings.Contains(stdout, "supplement-archive-sha256: ") {
		t.Errorf("check does not report the archive digest the lock records:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("check wrote a diagnostic: %q", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// An ABSENT supplement is a state this machine can be in, and a report that is silent
// about it is a report that cannot be told from a healthy pair's.
//
// `pinSupplement` publishes the primary alone when the v2fly repository cannot be
// reached, and nothing about that pair looks wrong from the outside: the list converts,
// sorts and routes, it just stops matching the ~70 rules only the second source can
// express. So the check says so in so many words, and names the command that fixes it.
func TestUpdateListsCheckReportsAnAbsentSupplement(t *testing.T) {
	paths := newListPaths(t)
	// A dnsmasq primary published alone: exactly the degraded pair pinSupplement
	// leaves behind when the v2fly repository is unreachable.
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	before := snapshot(t, paths.dir)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {head: remoteCommit},
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if !strings.Contains(stdout, "supplement-present: false") {
		t.Errorf("the report does not say the supplement is absent:\n%s", stdout)
	}
	for _, unwanted := range []string{"supplement-commit:", "supplement-remote-commit:", "supplement-repository:"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("the report carries %q for a pair with no supplement:\n%s", unwanted, stdout)
		}
	}
	// The remedy is named, because this is the state whose only fix is a command the
	// operator has to know about.
	for _, want := range []string{rules.RepositoryV2Fly, "--pin-remote HEAD"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the absent-supplement line does not name %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("check wrote a diagnostic: %q", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// A fallback publication -- v2fly as the primary -- has no supplement BY DESIGN, and
// the report must not present that as a fault the operator should go and repair.
//
// v2fly already carries every rule kind a supplement could contribute, so a lock
// naming one beside it would be untrue about how the list was built. The line still
// says the supplement is absent -- that is a fact about the machine -- but it must not
// tell this machine's operator to go and do something about it.
func TestUpdateListsCheckDoesNotCallAFallbackPairBroken(t *testing.T) {
	paths := newListPaths(t)
	publishPair(t, paths, lockedCommit, rulesOf(200))
	before := snapshot(t, paths.dir)
	origin := newFakeOrigin(t, lockedCommit, map[string][]byte{
		lockedCommit: sourceArchive(t, lockedCommit, "domain:first.cn\n"),
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if !strings.Contains(stdout, "supplement-present: false") {
		t.Errorf("the report does not record the absent supplement:\n%s", stdout)
	}
	if strings.Contains(stdout, "supplement-missing") {
		t.Errorf("the report calls a fallback publication a machine to repair:\n%s", stdout)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// The verdict has to cover both pinned sources.
//
// This is the whole point of the finding. `up-to-date: true` is the line
// mosdns-list-check.timer acts on, and with two sources a verdict computed from the
// primary alone says "true" about a machine whose v2fly supplement has moved on. A
// verdict that silently covers half its subject is worse than no verdict, because the
// operator acts on it.
func TestTheCheckVerdictCoversTheSupplementToo(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(200), supplementRules(2)...)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, published)
	before := snapshot(t, paths.dir)
	// The primary has not moved. The supplement has.
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 3),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if !strings.Contains(stdout, "up-to-date: false") {
		t.Errorf("the primary is unchanged and the supplement has moved, so the verdict must be false:\n%s", stdout)
	}
	if !strings.Contains(stdout, "supplement-remote-commit: "+remoteSupplementCommit) {
		t.Errorf("the report does not name the commit the supplement has moved to:\n%s", stdout)
	}
	if !strings.Contains(stdout, "supplement-remote-archive-sha256: ") {
		t.Errorf("the report does not carry the digest the new pin would record for the supplement:\n%s", stdout)
	}
	// The primary's own verdict is still there and still true; only the machine's is
	// stale. A report that said "up-to-date: false" without saying which source moved
	// would send the operator to the wrong repository.
	if !strings.Contains(stdout, "remote-commit: "+lockedCommit) {
		t.Errorf("the report does not record that the primary itself has not moved:\n%s", stdout)
	}
	// The primary's own archive is not fetched when the primary is the source that
	// has NOT moved: a report that carried a digest for it would be reporting a
	// download that did not happen.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "remote-archive-sha256: ") {
			t.Errorf("the primary has not drifted, so its archive digest is not reported:\n%s", stdout)
		}
	}
	// The verdict is the LAST line and is spelled the way the timer that acts on it
	// expects. Adding the second source to the report must not move it.
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if last := lines[len(lines)-1]; last != "up-to-date: false" {
		t.Errorf("the verdict is %q, want it last and spelled `up-to-date: false`:\n%s", last, stdout)
	}
	if stderr != "" {
		t.Errorf("check wrote a diagnostic: %q", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// The verdict is fail-closed about the second source as it is about the first.
//
// A machine that cannot reach the v2fly repository cannot be told whether its
// supplement has moved on. It must not be told "up-to-date: true" for that reason, and
// it must not be given a verdict at all -- stdout stays empty, the half of the report
// that needed no request goes to stderr, and the exit status is the check's own.
func TestTheCheckFailsClosedWhenTheSupplementCannotBeReached(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(200), supplementRules(2)...)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, published)
	before := snapshot(t, paths.dir)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		// Served with no HEAD of its own: the resolution cannot be answered.
		rules.RepositoryV2Fly: {head: ""},
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitStateUnavailable {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitStateUnavailable, stderr)
	}
	if stdout != "" {
		t.Errorf("a check that could not ask the second source wrote a verdict: %q", stdout)
	}
	if !strings.Contains(stderr, "supplement-commit: "+lockedSupplementCommit) {
		t.Errorf("the disk half on the failure path does not carry the supplement's identity:\n%s", stderr)
	}
	if !strings.Contains(stderr, "ranges-pin:") {
		t.Errorf("the ranges half is missing from the failure path:\n%s", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

func TestUpdateListsCheckRefusesToRunWithoutAValidPinnedPair(t *testing.T) {
	// A check that cannot see what it is comparing against is useless, and
	// inventing a comparison from an invalid pair would report drift against
	// something nobody pinned.
	tampered := []byte("domain:tampered.cn\n")
	invalidLock := []byte(`{"schema_version":1,"repository":"v2fly/domain-list-community","commit":"` + lockedCommit + `","sha256":"x","list_sha256":"y","entry":"data/cn"}`)
	tests := []struct {
		name    string
		arrange func(t *testing.T, paths listPaths)
	}{
		{
			name:    "nothing published",
			arrange: func(t *testing.T, paths listPaths) {},
		},
		{
			name: "list does not match its lock",
			arrange: func(t *testing.T, paths listPaths) {
				publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
				if err := os.WriteFile(paths.listFile, tampered, 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock is not a valid pin",
			arrange: func(t *testing.T, paths listPaths) {
				if err := os.WriteFile(paths.sourceLock, invalidLock, 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.listFile, tampered, 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock without a list",
			arrange: func(t *testing.T, paths listPaths) {
				publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
				if err := os.Remove(paths.listFile); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := newListPaths(t)
			test.arrange(t, paths)
			before := snapshot(t, paths.dir)
			origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)})

			code, stdout, stderr := runCLI(t, origin.services(), "--check",
				"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
			if code != exitStateUnavailable {
				t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitStateUnavailable, stderr)
			}
			if stdout != "" {
				t.Errorf("refused check wrote a report: %q", stdout)
			}
			if !strings.Contains(stderr, paths.sourceLock) {
				t.Errorf("refusal does not name the source lock path: %q", stderr)
			}
			if len(origin.requests) != 0 {
				t.Errorf("refused check still reached the remote: %v", origin.requests)
			}
			assertUnchanged(t, before, paths.dir, "")
		})
	}
}

func TestUpdateListsPinRemotePublishesTheListAndItsLock(t *testing.T) {
	// A pin is the only way a new source reaches the router, so it has to leave a
	// pair that agrees with itself and a report that names exactly what was
	// published.
	//
	// The source is NAMED rather than left to the first-install default. What this
	// case is about is the shape of a publication -- the list, the lock that
	// describes it, and a report naming both -- and a fixture leaning on whatever
	// the default happens to be would be a machine running a source no operator
	// chose. Naming the fallback says which source is under test and leaves the
	// default to the case that tests it.
	paths := newListPaths(t)
	archive := sourceArchive(t, remoteCommit, firstInstallArchive)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: archive})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatalf("published pair: %v", err)
	}
	if !found {
		t.Fatal("pin published no pair")
	}
	if lock.Primary.Commit != remoteCommit || lock.Primary.SHA256 != sha256Hex(archive) {
		t.Fatalf("published lock does not name the resolved source: %+v", lock)
	}
	if lock.ListSHA256 != sha256Hex(list) {
		t.Fatalf("published lock does not describe the published list: %+v", lock)
	}
	wantList := "domain:first.cn\nfull:exact.cn\nregexp:^static[0-9]+\\.example\\.cn$\n"
	if string(list) != wantList {
		t.Fatalf("published list = %q, want %q", list, wantList)
	}
	for _, want := range []string{
		"pinned-commit: " + remoteCommit,
		"pinned-archive-sha256: " + sha256Hex(archive),
		"pinned-list-sha256: " + sha256Hex([]byte(wantList)),
		"pinned-rules: 3",
		"published-source-lock: " + paths.sourceLock,
		"published-list: " + paths.listFile,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("pin report does not include %q:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("pin wrote a diagnostic: %q", stderr)
	}
	if _, err := os.Stat(paths.controlLock); err != nil {
		t.Errorf("pin did not take the control lock: %v", err)
	}
}

// `pinned-rules:` is how many rules the publication added to the router's China set,
// and a rendered list is not all rules.
//
// The dnsmasq converter keeps upstream's record of the entries it turned off, written
// as a comment block above the rules with a blank line between, so a list carrying
// those is longer in LINES than it is in rules. Counting lines scored the six
// disabled-upstream comments and the separator as rules: on the shipped pair the
// report said 110,558 for a list that holds 110,551 and whose digest the lock names.
// A reader comparing the two had to work out seven that were never rules, and the
// guardrails -- which measure with sourceRuleCounts -- disagreed with the report about
// the same file.
func TestPinnedRulesCountsRulesRatherThanLines(t *testing.T) {
	paths := newListPaths(t)
	// Six disabled records upstream, rendered as the comment block plus its blank
	// separator, and 200 real rules.
	const disabled = 6
	var domains bytes.Buffer
	for index := 0; index < disabled; index++ {
		fmt.Fprintf(&domains, "#server=/off%02d.cn/203.0.113.7 # Disabled: upstream reason %d\n", index, index)
	}
	for index := 0; index < 200; index++ {
		fmt.Fprintf(&domains, "server=/host%05d.example.cn/203.0.113.7\n", index)
	}
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				// Written out rather than built by chinaSourceArchive, which turns a
				// MOSDNS list into dnsmasq lines and so cannot express the disabled
				// records this case is about.
				remoteCommit: sourceArchiveUnder(t, "dnsmasq-china-list-"+remoteCommit, map[string]string{
					rules.ChinaConfEntries[0]: domains.String(),
					rules.ChinaConfEntries[1]: "server=/google.cn/203.0.113.7\n",
					rules.ChinaConfEntries[2]: "server=/apple.cn/203.0.113.7\n",
				}),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	list := paths.listContents()
	lines := strings.Split(strings.TrimSuffix(string(list), "\n"), "\n")
	// The fixture really does carry the shape this is about, or the assertion below
	// is passing for the wrong reason.
	comments, blanks := 0, 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			blanks++
		case strings.HasPrefix(trimmed, "#"):
			comments++
		}
	}
	if comments != disabled || blanks != 1 {
		t.Fatalf("the fixture rendered %d comments and %d blanks, want %d and 1: %q",
			comments, blanks, disabled, list)
	}
	// 200 rules from the main table, one each from the other two entries, and the two
	// the supplement contributes: 204 rules in 211 lines. The seven lines that are not
	// rules are exactly the six disabled records and renderList's one separator, which
	// is the number the line count used to add to the answer.
	const wantRules = 204
	if want := wantRules + comments + blanks; len(lines) != want {
		t.Fatalf("the published list is %d lines, want %d rules + %d comments + %d blank",
			len(lines), wantRules, comments, blanks)
	}
	if !strings.Contains(stdout, fmt.Sprintf("pinned-rules: %d\n", wantRules)) {
		t.Errorf("the report does not say %d rules for a list of %d lines carrying %d comments "+
			"and %d blank:\n%s", wantRules, len(lines), comments, blanks, stdout)
	}
}

// An unqualified `--pin-remote HEAD` on an empty machine installs the PRIMARY.
//
// Before this change there was only one source, so that invocation was
// unambiguous. After it there are two, and an operator who typed it has asked for
// "the China list" -- not for v2fly by name, which is what `--pin-source` is for.
// Defaulting it to the source the spec demotes (v2fly 不再是主源，但保留为回退) would
// also make the two ways of doing the same thing disagree: the automatic cold start
// installs the primary on this very machine, and a hand-run pin would not.
//
// The ROLE is what this case holds, and it is held against which repository was
// asked for which commit rather than against whether an archive was fetched. Both
// roles fetch the v2fly archive on a correct run -- a first install resolves the
// supplement from that same repository -- so "the v2fly archive was fetched" says
// nothing about which role it was resolved for. What says it is that each
// repository is asked only for a commit it issued: a run that resolved v2fly AS
// THE PRIMARY would ask the fallback for the primary's commit, and a run that
// resolved a commit against the wrong repository asks that repository for a commit
// that is not in it. The fixture serves the two repositories at different commits
// so both of those are requests the origin can be observed to have received.
//
// The published lock is the other half of the same claim, and it is the stronger
// one: it is the document that records which source is which.
func TestAnUnqualifiedFirstInstallPublishesThePrimary(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		// Served, at a commit of its own, because a first install resolves the
		// supplement from this repository: the run has to reach it. See
		// TestAFirstInstallPublishesTheUnionAndNotThePrimaryAlone for what it
		// publishes once it does.
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Primary.Repository != rules.RepositoryChina {
		t.Fatalf("an unqualified first install published %q; the operator asked for the China "+
			"list and the default is the primary %q. Name the fallback with `--pin-source %s` "+
			"if that is what you want.", lock.Primary.Repository, rules.RepositoryChina, rules.RepositoryV2Fly)
	}
	if lock.Primary.Commit != remoteCommit {
		t.Errorf("published %s, want the primary's own HEAD %s", lock.Primary.Commit, remoteCommit)
	}
	// Each repository is asked only about its own commits. The first of these is
	// the assertion that replaced "the v2fly archive was not fetched": it cannot be
	// satisfied by the correct run and is satisfied by a run that resolved the
	// fallback in the primary's place.
	if crossed := origin.archivePath(rules.RepositoryV2Fly, remoteCommit); origin.fetched(crossed) {
		t.Errorf("the run asked %s for the primary's commit %s, so it resolved the fallback "+
			"where the primary belonged", rules.RepositoryV2Fly, remoteCommit)
	}
	if crossed := origin.archivePath(rules.RepositoryChina, remoteSupplementCommit); origin.fetched(crossed) {
		t.Errorf("the run asked %s for the supplement's commit %s, so it resolved a commit "+
			"against a repository that did not issue it", rules.RepositoryChina, remoteSupplementCommit)
	}
}

// The fallback is still reachable BY NAME. Demoting a source is not deleting it:
// `--pin-source` is how an operator asks for the fallback deliberately, and it has to
// keep working on exactly the same empty machine the unqualified command now defaults
// away from.
func TestTheFallbackIsStillReachableByNameOnAnEmptyMachine(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-source %s exited %d (stderr: %s)", rules.RepositoryV2Fly, code, stderr)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Primary.Repository != rules.RepositoryV2Fly {
		t.Errorf("asking for the fallback by name published %q, want %q",
			lock.Primary.Repository, rules.RepositoryV2Fly)
	}
	if lock.Supplement != nil {
		t.Errorf("a v2fly primary was published with a supplement beside it: %+v", lock.Supplement)
	}
	if other := origin.archivePath(rules.RepositoryChina, remoteCommit); origin.fetched(other) {
		t.Errorf("naming the fallback still fetched %q", other)
	}
}

// A first install publishes the UNION. The published list is one document
// describing how it was built, and a dnsmasq primary on its own is a DIFFERENT
// document: the syntax the primary is written in cannot say a `full:` or a
// `regexp:` rule, so the seventy-odd rules only the second source can state are
// simply not in it. It is not a shorter version of the specified list; it is not
// the specified list.
//
// Nothing downstream notices. The pair describes itself, so ReadPublishedPair
// accepts it; the list converts, sorts and deduplicates; every routing test
// passes. It just stops matching the AWS China CDN edge names that the
// supplement's `regexp:` rules carry -- which is exactly the loss
// TestTheCommittedListKeepsTheRegexpRulesTheDnsmasqSourceCannotExpress exists to
// catch about the committed pair, and no gate in this command could see it.
//
// The carry-over is what made this invisible: an operator's pin re-pins the
// PRIMARY, and a publication with no published supplement had nothing to carry, so
// the first install -- the only run with nothing to carry -- published the primary
// ALONE.
func TestAFirstInstallPublishesTheUnionAndNotThePrimaryAlone(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Supplement == nil {
		t.Fatalf("the first install published the %s primary alone: %+v. The syntax that primary "+
			"is written in cannot express a `full:` or a `regexp:` rule, so this list is missing "+
			"every rule only the second source can state -- and it converts, sorts and routes "+
			"either way, so nothing about the published pair shows the loss",
			lock.Primary.Repository, lock)
	}
	if lock.Supplement.Repository != rules.RepositoryV2Fly {
		t.Errorf("the supplement is %q, want the only source rules allows beside the primary (%q)",
			lock.Supplement.Repository, rules.RepositoryV2Fly)
	}
	if lock.Supplement.Commit != remoteSupplementCommit {
		t.Errorf("the supplement is %s, want the supplement repository's own HEAD %s",
			lock.Supplement.Commit, remoteSupplementCommit)
	}
	for _, want := range []string{"full:exact000.cn", "full:exact001.cn"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the published list lost %q, which the dnsmasq source cannot express:\n%s", want, list)
		}
	}
}

// A run that quietly ACQUIRES a second source and a run that quietly DROPS one
// look the same from the outside: same exit code, same shape of report, same
// published pair that describes itself. So the acquisition is said, in the report,
// naming the repository and the commit it resolved.
//
// The wording is the assertion. That is deliberate: this is the operator-facing
// sentence and the only record that the machine acquired a source nobody asked
// about, so a change to it is a change to what the operator is told, and the case
// that should fail first is this one.
func TestAFirstInstallSaysItAcquiredTheSecondSource(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	want := "added the " + rules.RepositoryV2Fly + " supplement at " + remoteSupplementCommit
	if !strings.Contains(stdout, want) {
		t.Errorf("the run acquired a second source without saying so, which is indistinguishable "+
			"from a run that quietly dropped one: the report has no %q:\n%s", want, stdout)
	}
}

// The second source cannot be reached on a machine that CAN reach the primary.
// That must not become a hard failure: this command is the documented install, it
// has nothing on disk to keep, and refusing here leaves the machine with no China
// list at all rather than with one that is missing seventy rules out of a hundred
// and eleven thousand. So the primary is published, and what is missing from it is
// said on stderr rather than left for an operator to discover by noticing that some
// names stopped matching.
//
// It is the same judgement `carriedSupplement` already makes for a lock this build
// cannot read, and the reason is the same: a note and not a refusal, because
// refusing strands an operator whose only way out is this command. The two
// guardrails in refreshChinaList do refuse a supplement it cannot resolve, and
// they can: that run has a published pair to keep, so refusing costs it nothing.
func TestAFirstInstallThatCannotReachTheSecondSourceStillPublishesAndSaysWhy(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		// The head answers and the archive does not, which is the shape a blocked
		// host takes: the repository is named, so the resolution is attempted, and
		// the fetch is what fails.
		rules.RepositoryV2Fly: {head: remoteSupplementCommit},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("an install that could reach the primary refused to install it because the "+
			"second source was unreachable (exit %d): %s", code, stderr)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Primary.Repository != rules.RepositoryChina || lock.Supplement != nil {
		t.Fatalf("the degraded publication is %+v, want the %s primary and no supplement",
			lock, rules.RepositoryChina)
	}
	// Said, on stderr, because it is a fault rather than a report, and said with
	// the consequence in it: a note naming a repository without saying what its
	// absence costs is a note an operator files and forgets.
	for _, want := range []string{
		rules.RepositoryV2Fly,
		"could not be resolved",
		"missing from the list this command publishes",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the diagnostic does not say %q:\n%s", want, stderr)
		}
	}
}

// A RE-PIN over a lock that names no second source.
//
// This is the state the defect above leaves behind, and it is why the first-install
// fix was not enough: the first install published the dnsmasq primary alone, and the
// lock it wrote says so. Nothing about that pair looks wrong from the outside --
// ReadPublishedPair accepts it, the list converts, sorts and routes -- so every later
// hand `--pin-remote` on that machine took the "carry the second source forward"
// path and carried nothing. The machine that hit the bug keeps hitting it, and the
// list keeps missing every rule the dnsmasq syntax cannot express.
//
// The state is also reachable without any of this having been released:
// `--pin-remote HEAD --pin-source felixonmars/dnsmasq-china-list` over a v1 lock is
// the recovery path this file's own refusals name, and it publishes the primary
// alone the same way. So this is not a state only a buggy build creates.
//
// And it does not heal by itself on a default installation: the `gaining` path in
// refreshChinaList that would resolve the supplement for it runs only under
// `--automatic`, and lists.china.automatic is OFF BY DEFAULT
// (internal/config/policy.go:46-54). A machine that never opted in has exactly one
// remedy, and it is this command.
//
// The fixture is publishChinaPair, which writes a schema version 2 lock naming the
// dnsmasq primary and no supplement -- a document this build both reads and writes,
// so the case does not rest on a state the command could not produce itself.
func TestARepinOverALockThatNamesNoSecondSourcePublishesTheUnion(t *testing.T) {
	paths := newListPaths(t)
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Supplement == nil {
		t.Errorf("a re-pin over a lock naming no second source published the %s primary alone: "+
			"%+v. The syntax that primary is written in cannot express a `full:` or a `regexp:` rule, "+
			"so this list is missing every rule only the second source can state -- and it converts, "+
			"sorts and routes either way, so nothing about the published pair shows the loss, and "+
			"this machine had nothing to carry from in the first place",
			lock.Primary.Repository, lock)
	}
	for _, want := range []string{"full:exact000.cn", "full:exact001.cn"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the published list has %d rules and no %q, which the dnsmasq source "+
				"cannot express", countListRules(list, lock.Supplement), want)
		}
	}
}

// The same run, from the operator's side of the screen.
//
// A run that quietly gains a second source and a run that quietly drops one are the
// same run seen from outside: same exit code, same shape of report, same published
// pair that describes itself. So the acquisition is said -- and said with the reason,
// because the reason is the only thing that tells the operator this was not the first
// install. "This machine had nothing published" on a machine that has published for
// months is not a true sentence, and a report line an operator has to disbelieve is
// worse than no report line.
func TestARepinOverALockThatNamesNoSecondSourceSaysWhereTheSecondSourceCameFrom(t *testing.T) {
	paths := newListPaths(t)
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	acquired := "added the " + rules.RepositoryV2Fly + " supplement at " + remoteSupplementCommit
	if !strings.Contains(stdout, acquired) {
		t.Errorf("the run acquired a second source without saying so, which is indistinguishable "+
			"from a run that quietly dropped one: the report has no %q:\n%s", acquired, stdout)
	}
	if !strings.Contains(stdout, "named no second source") {
		t.Errorf("the report does not say WHERE the second source came from, so an operator "+
			"watching a machine acquire one it never asked about has nothing to check it "+
			"against:\n%s", stdout)
	}
	if strings.Contains(stdout, "had nothing published") {
		t.Errorf("the report says this machine had nothing published, and it had a list: %q", stdout)
	}
}

// The degraded note PROMISES something: re-run the pin once the second source is
// reachable and "the run that succeeds publishes the union". That promise was false.
//
// After a degraded run the lock on disk names the dnsmasq primary and omits the
// supplement, so the re-run is not a first install -- and a re-pin over a lock naming
// no second source published the primary alone again, with no note, no warning and
// exit 0. Following the advice the command itself printed reproduced the defect in
// silence, which is the worst way for it to fail: the operator did exactly what they
// were told and the list they ended up with is still short of seventy rules.
//
// This is the same defect as the case above, reached by the route an operator is most
// likely to take, and it is here because a promise this file makes must be true. It is
// also the check that the two halves are ONE fix: repairing the re-pin is what makes
// the sentence deliverable, and there is no separate change to the wording.
func TestTheDegradedNotePromisesARerunThatDeliversTheUnion(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		// The head answers and the archive does not, which is the shape a host that
		// can reach one repository and not the other takes.
		rules.RepositoryV2Fly: {head: remoteSupplementCommit},
	})
	pin := []string{
		"--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock,
	}

	code, _, stderr := runCLI(t, origin.pinServices(), pin...)
	if code != exitSuccess {
		t.Fatalf("the degraded install exited %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	// The promise itself, pinned. It passes before the change too -- the sentence is
	// already there, and the finding is that it is not TRUE, so this assertion is a
	// guard on the wording rather than evidence of the fix. It is here so that a later
	// edit cannot quietly soften the claim while fixing the behaviour.
	for _, want := range []string{
		"mosdns-cdnctl update-lists --pin-remote HEAD",
		"publishes the union",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the degraded note does not say %q:\n%s", want, stderr)
		}
	}

	// The machine can now reach the second source, and the operator follows the note.
	origin.setArchives(rules.RepositoryV2Fly, map[string][]byte{
		remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
	})

	code, _, stderr = runCLI(t, origin.pinServices(), pin...)
	if code != exitSuccess {
		t.Fatalf("the re-run the note recommends exited %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Supplement == nil {
		t.Fatalf("the re-run the degraded note recommends published the %s primary alone: %+v. "+
			"Following the note on screen left the machine on exactly the list it was already "+
			"short of, and said nothing while doing it",
			lock.Primary.Repository, lock)
	}
	for _, want := range []string{"full:exact000.cn", "full:exact001.cn"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the published list has %d rules and no %q, which the dnsmasq source "+
				"cannot express", countListRules(list, lock.Supplement), want)
		}
	}
}

// The DEGRADED note on the widened path is a different sentence, because the state
// is. The first-install justification -- "this machine has no China list at all and a
// whole list missing seventy rules is strictly better than none" -- is plainly false
// of a machine with a hundred and eleven thousand rules on disk, and printing it
// there would tell an operator holding a working list that it holds none.
//
// So this pins the widened wording, and specifically pins what it must NOT say. The
// other half is the settled decision that a degraded run is still a successful one:
// exit 0, the primary published, and the gap named. This command's contract already
// contains "published, degraded, loudly, exit 0" -- publishFallback writes an
// 8,363-rule list in place of 110,551 and returns exitSuccess -- so an exit 3 here
// would report "nothing was published" for a publication that happened.
func TestADegradedRunOverALockThatNamesNoSecondSourceDoesNotClaimTheMachineIsEmpty(t *testing.T) {
	paths := newListPaths(t)
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {head: remoteSupplementCommit},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("a degraded pin over a readable lock exited %d, want %d. A publication that "+
			"happened must not be reported as one that did not (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Primary.Repository != rules.RepositoryChina || lock.Supplement != nil {
		t.Fatalf("the degraded publication is %+v, want the %s primary and no supplement",
			lock, rules.RepositoryChina)
	}
	if strings.Contains(stderr, "this machine has no China list at all") {
		t.Errorf("the note told an operator with a list on disk that it has none: %s", stderr)
	}
	for _, want := range []string{rules.RepositoryV2Fly, "already named no second source"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the diagnostic does not say %q:\n%s", want, stderr)
		}
	}
}

// An explicit pin survives a later unqualified run. This is the one way the new default
// could become a new defect: if the first-install default were consulted whenever the
// repository is not named on the command line, a machine that deliberately pinned the
// fallback would be dragged onto the primary by the next bare `--pin-remote HEAD`.
//
// The published lock outranks the default, and this case is what holds that: the origin
// serves BOTH repositories, so a run that asked the wrong one would publish a lock
// naming it and fail here rather than merely looking right.
//
// It also holds the other half of the rule about which repository fills which role.
// A dnsmasq primary is published as a union with a supplement, and a v2fly primary is
// the fallback publication and has none -- so a fix that gave the fallback a
// supplement would make this machine's lock a document untrue about how its list was
// built, and rules.validatePinnedSource would refuse it. The assertion below is what
// would notice first, and the origin serving the primary is what lets it: a run that
// resolved one beside a v2fly primary would fetch the primary's archive.
func TestADeliberateV2FlyPinSurvivesALaterUnqualifiedPin(t *testing.T) {
	paths := newListPaths(t)
	// A machine that asked for the fallback and got it.
	publishPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Repository != rules.RepositoryV2Fly {
		t.Fatalf("an unqualified re-pin moved a machine off the source its lock names, onto "+
			"%q: the lock is what says where this machine runs, and the first-install default is "+
			"only for a machine with nothing published", lock.Primary.Repository)
	}
	if lock.Primary.Commit != remoteCommit {
		t.Errorf("published %s, want the fallback's HEAD %s", lock.Primary.Commit, remoteCommit)
	}
	if lock.Supplement != nil {
		t.Errorf("the re-pin gave a %s primary a supplement: %+v. The fallback already carries "+
			"every rule kind a supplement contributes, so this lock would be untrue about how "+
			"the list was built.", lock.Primary.Repository, lock.Supplement)
	}
	if other := origin.archivePath(rules.RepositoryChina, remoteCommit); origin.fetched(other) {
		t.Errorf("the re-pin fetched %q, so it asked the primary for a machine pinned to the fallback", other)
	}
}

func TestUpdateListsPinRemoteReplacesAnInstalledPair(t *testing.T) {
	// The second pin of an installation is the one that has to leave no trace of
	// the old list: a router reading a mixture of two sources would route against
	// rules nobody pinned together.
	paths := newListPaths(t)
	previous := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, previous)
	archive := sourceArchive(t, remoteCommit, firstInstallArchive)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: archive})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Primary.Commit != remoteCommit {
		t.Fatalf("published lock still names %s, want %s", lock.Primary.Commit, remoteCommit)
	}
	if bytes.Equal(list, previous) {
		t.Fatal("the previous list is still published")
	}
	// Four files, not three: the archive is written by every pin and is named in the
	// gate rather than excluded from it, because a pin that stopped writing it would
	// otherwise still satisfy "the pair and the control lock" and the rollback would
	// quietly stop existing.
	if state := snapshot(t, paths.dir); len(state) != 4 {
		t.Fatalf("published directory holds %v, want the pair, the control lock and the archive",
			names(state))
	}
	archived, err := rules.ReadPrevious(paths.previousLock)
	if err != nil {
		t.Fatalf("the pin wrote no readable archive: %v", err)
	}
	if archived.Primary.Commit == remoteCommit {
		t.Error("the archive names the commit this run just published, which is not a rollback target")
	}
	if !strings.Contains(stdout, "pinned-commit: "+remoteCommit) {
		t.Errorf("pin report does not name the accepted commit:\n%s", stdout)
	}
}

func TestUpdateListsPinRemoteKeepsTheInstalledPairOnEveryFailure(t *testing.T) {
	// Every way a pin can fail has to leave the router's list and the lock that
	// describes it exactly as they were, and has to say so on stderr instead of
	// reporting a commit. The last case covers a pair the command cannot even
	// read: it must refuse rather than replace it.
	tests := []struct {
		name    string
		arrange func(t *testing.T, paths listPaths, origin *fakeOrigin)
	}{
		{
			name: "the remote refuses to name a commit",
			arrange: func(t *testing.T, paths listPaths, origin *fakeOrigin) {
				origin.status = http.StatusInternalServerError
			},
		},
		{
			name: "the remote has no such archive",
			arrange: func(t *testing.T, paths listPaths, origin *fakeOrigin) {
				// The origin publishes a commit it cannot serve an archive for.
				origin.setArchives(rules.RepositoryV2Fly, map[string][]byte{lockedCommit: sourceArchive(t, lockedCommit, firstInstallArchive)})
			},
		},
		{
			name: "the archive is not the list the gateway understands",
			arrange: func(t *testing.T, paths listPaths, origin *fakeOrigin) {
				origin.setArchives(rules.RepositoryV2Fly, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, "ip4:1.2.3.4\n")})
			},
		},
		{
			name: "the archive does not carry the pinned entry",
			arrange: func(t *testing.T, paths listPaths, origin *fakeOrigin) {
				origin.setArchives(rules.RepositoryV2Fly, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, "# nothing here\n")})
			},
		},
		{
			name: "the published pair cannot be read",
			arrange: func(t *testing.T, paths listPaths, origin *fakeOrigin) {
				publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
				if err := os.Remove(paths.listFile); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the published list does not match its lock",
			arrange: func(t *testing.T, paths listPaths, origin *fakeOrigin) {
				publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
				// Somebody edited the list the router is running without re-pinning.
				if err := os.WriteFile(paths.listFile, []byte("domain:edited-by-hand.cn\n"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := newListPaths(t)
			published := []byte("domain:previous.cn\n")
			publishPair(t, paths, lockedCommit, published)
			origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)})
			test.arrange(t, paths, origin)
			before := snapshotWithout(t, paths.dir, filepath.Base(paths.controlLock))

			code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
				"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
			if code == exitSuccess {
				t.Fatalf("pin reported success although it could not publish:\n%s", stdout)
			}
			if code != exitStateUnavailable {
				t.Errorf("pin exit = %d, want %d for a failing pin (stderr: %s)", code, exitStateUnavailable, stderr)
			}
			if strings.Contains(stdout, "pinned-commit") {
				t.Errorf("failed pin reported an accepted commit:\n%s", stdout)
			}
			if stderr == "" {
				t.Error("failed pin wrote no diagnostic")
			}
			assertUnchanged(t, before, paths.dir, filepath.Base(paths.controlLock))
		})
	}
}

// The reason this change exists: --pin-remote could only name upstream's current
// HEAD, so once a commit nobody reviewed was accepted there was no way back to the
// one this machine was on yesterday.
func TestPinRemoteNamesTheCommitItWillPublish(t *testing.T) {
	options, err := parseUpdateListOptions(io.Discard, []string{"--pin-remote", lockedCommit})
	if err != nil {
		t.Fatalf("--pin-remote %s was refused: %v", lockedCommit, err)
	}
	if options.pinRemote != lockedCommit {
		t.Errorf("--pin-remote did not keep the commit: %q", options.pinRemote)
	}
}

// HEAD still means HEAD, and it is normalized to the empty string so there is one
// place in the code that asks "which commit?" rather than two that disagree about
// what the empty value means.
func TestPinRemoteStillAcceptsTheLiteralHEAD(t *testing.T) {
	options, err := parseUpdateListOptions(io.Discard, []string{"--pin-remote", "HEAD"})
	if err != nil {
		t.Fatalf("the documented --pin-remote HEAD was refused: %v", err)
	}
	if options.pinRemote != "" {
		t.Errorf("--pin-remote HEAD left %q in the options, want the empty value that means HEAD", options.pinRemote)
	}
}

// Anything that is neither HEAD nor a full commit is refused on the command line,
// before a request is made: a branch, a tag, an abbreviated commit and HEAD~1 all
// name a moving target or a form this package does not verify, and accepting any of
// them would put a ref where the lock records a commit.
func TestPinRemoteRefusesSomethingThatIsNeitherHEADNorACommit(t *testing.T) {
	for _, ref := range []string{"main", "master", "v0.2.0", "HEAD~1", "zzzzzzzz", "1111111111"} {
		t.Run(ref, func(t *testing.T) {
			options, err := parseUpdateListOptions(io.Discard, []string{"--pin-remote", ref})
			if err == nil {
				t.Fatalf("--pin-remote %q was accepted as %q", ref, options.pinRemote)
			}
			if !strings.Contains(err.Error(), "--pin-remote") {
				t.Errorf("the refusal does not name the flag: %v", err)
			}
		})
	}
	// The empty value is refused too, and by a different rule: it selects no mode at
	// all, so the diagnostic is about the missing mode rather than about the ref. It
	// is listed here because both are refusals and an operator should not be able to
	// tell from the exit code that one of them was a mistake about the value.
	if options, err := parseUpdateListOptions(io.Discard, []string{"--pin-remote", ""}); err == nil {
		t.Errorf("--pin-remote with an empty value selected the mode %+v", options)
	}
}

// Naming a commit publishes THAT commit even when it is not the remote's HEAD. The
// rollback path is the whole use, and a rollback that published the current HEAD
// would move the machine forward instead of back.
func TestPinRemotePublishesTheNamedCommitRatherThanTheRemoteHead(t *testing.T) {
	paths := newListPaths(t)
	olderArchive := sourceArchive(t, lockedCommit, "domain:older.cn\n")
	newerArchive := sourceArchive(t, remoteCommit, firstInstallArchive)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		lockedCommit: olderArchive,
		remoteCommit: newerArchive,
	})

	// The source is named: this case is about pinning a NAMED COMMIT rather than
	// whatever HEAD says now, and leaving it to the first-install default would make
	// it a test of the default as well.
	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", lockedCommit,
		"--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock, "--previous-lock", filepath.Join(paths.dir, "previous.json"))
	if code != exitSuccess {
		t.Fatalf("pinning a named commit exited %d (stderr: %s)", code, stderr)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: found=%v err=%v", found, err)
	}
	if lock.Primary.Commit != lockedCommit {
		t.Fatalf("published %s, want the named %s -- a rollback that moved forward is not a rollback",
			lock.Primary.Commit, lockedCommit)
	}
	if lock.Primary.SHA256 != sha256Hex(olderArchive) {
		t.Errorf("the lock records archive %s, which is the other commit's", lock.Primary.SHA256)
	}
	if !strings.Contains(stdout, "pinned-commit: "+lockedCommit) {
		t.Errorf("the report does not name the commit it published:\n%s", stdout)
	}
	// And it did not ask the API where HEAD is. A named commit needs no resolution
	// round trip, and one that needed it would fail on a machine that can reach the
	// archive host but not the API -- which is the situation an operator doing this
	// is most likely to be in.
	if origin.fetched(origin.commitPath(rules.RepositoryV2Fly)) {
		t.Errorf("pinning a named commit asked the API for HEAD: %v", origin.requests)
	}
}

// Review Focus, class 2: the archive must name the pin that was CURRENT when this run
// started, not one an earlier run left behind. A stale archive is worse than none,
// because the operator believes they are rolling back to a commit they chose and
// lands on a different one.
func TestPublishingArchivesThePinThatWasCurrent(t *testing.T) {
	paths := newListPaths(t)
	installed := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, installed)
	previous := filepath.Join(paths.dir, "previous.json")
	stale := rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryV2Fly,
			Commit:     "3333333333333333333333333333333333333333",
			SHA256:     sha256Hex([]byte("an archive from a run nobody remembers")),
			Entry:      rules.V2FlyEntry,
		},
		ListSHA256: sha256Hex([]byte("domain:stale.cn\n")),
	}
	encoded, err := stale.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, encoded, 0o640); err != nil {
		t.Fatal(err)
	}

	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})
	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock, "--previous-lock", previous)
	if code != exitSuccess {
		t.Fatalf("pin exited %d (stderr: %s)", code, stderr)
	}

	archived, err := rules.ReadPrevious(previous)
	if err != nil {
		t.Fatalf("no readable archive after a successful publish: %v", err)
	}
	if archived.Primary.Commit != lockedCommit {
		t.Errorf("the archive names %s, want the pin that was current (%s); a rollback would "+
			"land on a commit the operator did not choose", archived.Primary.Commit, lockedCommit)
	}
	if archived.ListSHA256 != sha256Hex(installed) {
		t.Errorf("the archive records list %s, which is not the list that was installed",
			archived.ListSHA256)
	}
}

// The archive is written BEFORE the publish, so a publish that fails leaves an
// archive naming a commit that really was current -- which is exactly what an
// operator rolling back after a failed unattended refresh needs. Archived after, a
// failed publish would leave the archive naming the pin that was about to be replaced
// by a change that never landed.
func TestAFailedPublishLeavesThePinAndTheArchiveIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this case makes a directory unwritable to fail the publication, which " +
			"does not stop root")
	}
	paths := newListPaths(t)
	installed := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, installed)
	// The list lives in a directory of its own, because a read-only directory is how
	// this case fails the publication and only the publication: a missing directory
	// would fail the pair check first and never reach a publish.
	listDir := filepath.Join(paths.dir, "readonly")
	if err := os.Mkdir(listDir, 0o755); err != nil {
		t.Fatal(err)
	}
	listFile := filepath.Join(listDir, "cn-domains.txt")
	if err := os.WriteFile(listFile, installed, 0o640); err != nil {
		t.Fatal(err)
	}

	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})
	// Taken away BEFORE the run. ReadPublishedPair has to succeed -- it is what proves
	// there is an outgoing pair worth protecting -- and only the publication may fail,
	// so the directory is made unwritable once the files are already in it. Chmod after
	// the run would test nothing: it was the first attempt at this case and it passed
	// a pin that had published successfully.
	if err := os.Chmod(listDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(listDir, 0o755) })

	previous := paths.previousLock
	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", listFile,
		"--control-lock", paths.controlLock, "--previous-lock", previous)
	if code == exitSuccess {
		t.Fatalf("pin reported success with the publication failing: %s", stdout)
	}
	// It is the PUBLICATION that failed here, not the archive, so the refusal names
	// the list. The case where the archive is what fails is the next one, and it
	// asserts the same thing about the other file: a refusal an operator cannot map
	// to a file is a refusal they cannot act on.
	if !strings.Contains(stderr, listFile) {
		t.Errorf("the refusal does not name the file it could not write: %q", stderr)
	}
	// The archive was written before the publish, so it exists and names the pin that
	// really was current. That is the whole claim: after a failed publish, an operator
	// can still get back to where they were.
	archived, err := rules.ReadPrevious(previous)
	if err != nil {
		t.Fatalf("a failed publish left no readable archive: %v", err)
	}
	if archived.Primary.Commit != lockedCommit {
		t.Errorf("the archive names %s, want the pin that was current (%s)", archived.Primary.Commit, lockedCommit)
	}
	after, _, found, err := rules.ReadPublishedPair(paths.sourceLock, listFile)
	if err != nil || !found {
		t.Fatalf("the pair is unreadable after a failed publish: found=%v err=%v", found, err)
	}
	if after.Primary.Commit != lockedCommit {
		t.Errorf("a failed publish moved the pin from %s to %s", lockedCommit, after.Primary.Commit)
	}
	if got := string(mustReadFile(t, listFile)); got != string(installed) {
		t.Errorf("a failed publish changed the list:\n%s", got)
	}
}

// An archive that cannot be written stops the pin rather than being skipped. The
// alternative -- publish, and lose the rollback target -- is the one outcome this
// whole mechanism exists to prevent, and it is unrecoverable afterwards: once the
// old lock is replaced there is no record of it anywhere.
func TestAFailedArchiveRefusesRatherThanPublishing(t *testing.T) {
	paths := newListPaths(t)
	publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
	// A directory where the archive file goes: the rename onto it cannot succeed, and
	// it fails the same way whatever uid the suite runs as.
	previous := filepath.Join(paths.dir, "previous.json")
	if err := os.Mkdir(previous, 0o755); err != nil {
		t.Fatal(err)
	}

	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})
	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock, "--previous-lock", previous)
	if code == exitSuccess {
		t.Fatalf("pin reported success although it kept no rollback target: %s", stdout)
	}
	if !strings.Contains(stderr, previous) {
		t.Errorf("the refusal does not name the archive it could not write: %q", stderr)
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit != lockedCommit {
		t.Errorf("the pin moved to %s although the archive failed, so the previous pin is gone", lock.Primary.Commit)
	}
}

func TestUpdateListsPinRemoteReportsAPublicationFailureInsteadOfSuccess(t *testing.T) {
	// A pin that verified its source and then could not write the pair has to say
	// so. Reporting an accepted commit for a list that was never published is the
	// one outcome an operator cannot recover from, because the next check would
	// compare a lock nobody wrote against a list nobody installed.
	paths := newListPaths(t)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)})
	listFile := filepath.Join(paths.dir, "not-installed", "cn-domains.txt")

	// The source is named: this case is about the refusal a failed PUBLICATION
	// produces, and the repository the source came from is not part of it.
	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitStateUnavailable {
		t.Fatalf("pin with an unwritable list exit = %d, want %d (stderr: %s)", code, exitStateUnavailable, stderr)
	}
	if strings.Contains(stdout, "pinned-commit") {
		t.Errorf("pin reported an accepted commit although it published nothing:\n%s", stdout)
	}
	if !strings.Contains(stderr, listFile) {
		t.Errorf("refusal does not name the list path: %q", stderr)
	}
	if _, err := os.Stat(filepath.Dir(listFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pin created the missing list directory: %v", err)
	}
	if _, err := os.Stat(paths.sourceLock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pin published a lock without a list: %v", err)
	}
}

func TestUpdateListsPinRemoteFailsWithExitFourWhileTheControlLockIsHeld(t *testing.T) {
	// The control lock is what keeps a pin from publishing a pair while the
	// router is being reconfigured. A pin that cannot take it has to stop with
	// the lock exit code, not with a generic failure, so an operator can tell a
	// busy gateway from a broken source.
	paths := newListPaths(t)
	published := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, published)
	held, err := filelock.Acquire(paths.controlLock)
	if err != nil {
		t.Fatalf("take the control lock: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	before := snapshot(t, paths.dir)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitLockHeld {
		t.Fatalf("pin under a held lock exit = %d, want %d (stderr: %s)", code, exitLockHeld, stderr)
	}
	if !strings.Contains(stderr, paths.controlLock) {
		t.Errorf("refusal does not name the control lock path: %q", stderr)
	}
	if strings.Contains(stdout, "pinned-commit") {
		t.Errorf("pin under a held lock reported an accepted commit:\n%s", stdout)
	}
	assertUnchanged(t, before, paths.dir, "")
}

func TestUpdateListsTakesTheControlLockOnlyAfterTheSourceIsVerified(t *testing.T) {
	// The control lock is not a download lock: a check runs daily and must not
	// exclude a running gateway, and a pin that grabbed it before reading the
	// remote would block the router for the length of a download. A failing
	// conversion under a held lock therefore has to report the conversion, which
	// also proves the lock is taken after the conversion.
	paths := newListPaths(t)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, "ip4:1.2.3.4\n")})

	held, err := filelock.Acquire(paths.controlLock)
	if err != nil {
		t.Fatalf("take the control lock: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	// The source is named: this case is about WHEN the control lock is taken relative
	// to the conversion, not about which repository the conversion came from.
	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code == exitLockHeld {
		t.Fatalf("pin reported the held lock although it never reached publication: %s", stderr)
	}
	if code != exitStateUnavailable {
		t.Fatalf("pin exit = %d, want %d (stderr: %s)", code, exitStateUnavailable, stderr)
	}
	if !strings.Contains(stderr, "ip4") {
		t.Errorf("failure is not the conversion failure: %q", stderr)
	}
}

func TestUpdateListsCheckWritesNoPartialReportWhenTheDriftedArchiveCannotBeRead(t *testing.T) {
	// The remote has moved on, so the check has to read its archive to report the
	// digest a pin would record, and the origin has no archive for the commit it
	// just published. A report cut in half, with no verdict in it, is worse than
	// no report: stdout either carries a complete answer or nothing.
	paths := newListPaths(t)
	publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
	before := snapshot(t, paths.dir)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{lockedCommit: sourceArchive(t, lockedCommit, firstInstallArchive)})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitStateUnavailable {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitStateUnavailable, stderr)
	}
	if stdout != "" {
		t.Fatalf("failed check wrote a partial report: %q", stdout)
	}
	if !strings.Contains(stderr, remoteCommit) {
		t.Errorf("refusal does not name the commit it could not read an archive for: %q", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

func TestTheCheckAsksTheRepositoryThePublishedLockNames(t *testing.T) {
	// "Has upstream moved on from what this machine pinned" is a question about
	// one repository. A check that asked the v2fly one about a list built from
	// the dnsmasq source would answer a question nobody asked: it would report a
	// commit, and an up-to-date verdict, about a source this machine is not
	// running. Both facts are visible here because the origin records every path
	// it was asked for, and the two repositories are distinguishable paths.
	paths := newListPaths(t)
	published := []byte("domain:previous.cn\n")
	publishChinaPair(t, paths, lockedCommit, published)
	before := snapshot(t, paths.dir)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, firstInstallArchive),
			},
		},
		rules.RepositoryV2Fly: {head: lockedCommit},
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--check exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if want := origin.commitPath(rules.RepositoryChina); !origin.fetched(want) {
		t.Fatalf("--check fetched %v, want the HEAD of the repository the lock names %q", origin.requests, want)
	}
	if other := origin.commitPath(rules.RepositoryV2Fly); origin.fetched(other) {
		t.Fatalf("--check fetched %q, which is not the source this list was published from", other)
	}
	// The archive follows the same question: a drifted check reports the digest
	// the new pin WOULD record, and that archive is the pinned source's.
	if want := origin.archivePath(rules.RepositoryChina, remoteCommit); !origin.fetched(want) {
		t.Fatalf("--check fetched %v, want the archive at %q", origin.requests, want)
	}
	if other := origin.archivePath(rules.RepositoryV2Fly, remoteCommit); origin.fetched(other) {
		t.Fatalf("--check fetched %q, which belongs to a source this list was not published from", other)
	}
	for _, want := range []string{"repository: " + rules.RepositoryChina, "entries: " + strings.Join(rules.ChinaConfEntries, " "), "up-to-date: false"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("check output does not report %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "entry: ") {
		t.Errorf("check reported a single entry for a source that has none:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("check wrote a diagnostic: %q", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

func TestAMachineOnAV1LockCanFollowTheRefusalItIsGiven(t *testing.T) {
	// This is the deadlock. ParseSourceLock refuses a v1 lock and names
	// `update-lists --pin-remote` as the way out; if that command also refuses a
	// v1 pair on disk, the message sends the operator into a refusal, and the
	// only recovery is hand-editing a file. So the refusal's own advice has to
	// work: an operator who names the source re-pins onto a v2 lock.
	//
	// The source is named on the command line rather than read from the pair,
	// because the pair is exactly what cannot be read.
	paths := newListPaths(t)
	publishV1Pair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote on a v1 pair exited %d, want %d (stderr: %s)\nstdout: %s", code, exitSuccess, stderr, stdout)
	}

	// The published lock must be a v2 document this build can read back, not
	// merely a file that changed.
	published, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatalf("the published pair cannot be read after the re-pin: %v", err)
	}
	if !found {
		t.Fatal("nothing is published after the re-pin")
	}
	if published.SchemaVersion != 2 {
		t.Errorf("the published lock is schema version %d, want 2", published.SchemaVersion)
	}
	if published.Primary.Repository != rules.RepositoryV2Fly || published.Primary.Commit != remoteCommit {
		t.Errorf("the published lock names %+v, want v2fly at %s", published.Primary, remoteCommit)
	}
	if got := sha256Hex(list); got != published.ListSHA256 {
		t.Errorf("the published list digest is %s and the lock records %s", got, published.ListSHA256)
	}
	if !strings.Contains(stdout, "pinned-commit: "+remoteCommit) {
		t.Errorf("the re-pin did not report the commit it published:\n%s", stdout)
	}
}

// A healthy machine that names its source must get the SAME archive every other
// pin gets. Choosing the archiver on which flag was typed rather than on whether
// the outgoing lock parses gives up the property Archive re-encodes for -- that
// the archive is a lock this package writes, not the bytes someone left on disk
// -- and it gives it up silently, on a machine where nothing is wrong.
func TestPinSourceOverAHealthyPairLeavesAnArchiveThisBuildCanReadBack(t *testing.T) {
	// Two properties in one run, because the run is what produces both: it
	// publishes, and the archive it leaves behind is usable. An archive this
	// build cannot parse is a rollback target it cannot roll back to.
	paths := newListPaths(t)
	installed := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, installed)
	// Rewritten on one line, which still parses: a lock is a lock whatever its
	// whitespace. The fixture is what makes the archive comparison below mean
	// something -- a pair written by Encode() looks the same either way, so a
	// test using one cannot tell a copy from a re-encode.
	oneline, err := os.ReadFile(paths.sourceLock)
	if err != nil {
		t.Fatal(err)
	}
	compact := bytes.ReplaceAll(bytes.ReplaceAll(oneline, []byte("\n"), nil), []byte("  "), nil)
	if err := os.WriteFile(paths.sourceLock, compact, 0o640); err != nil {
		t.Fatal(err)
	}
	// The compact lock has to be readable, or this fixture is testing the v1
	// path again under a different name.
	if _, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile); err != nil {
		t.Fatalf("the compact fixture is not a readable pair: %v", err)
	}
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote over a healthy pair exited %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	published, _, found, readErr := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if readErr != nil || !found {
		t.Fatalf("the published pair cannot be read after the pin: %v", readErr)
	}
	if published.Primary.Commit != remoteCommit {
		t.Errorf("published %+v, want the remote commit %s", published.Primary, remoteCommit)
	}

	// The archive is the rollback target, and ReadPrevious is how it is read.
	// A v1 or otherwise unparseable archive is a file this build can see and
	// cannot use, which is the failure this asserts has not happened here.
	archived, err := rules.ReadPrevious(paths.previousLock)
	if err != nil {
		t.Fatalf("the archive is not a lock this build can read back: %v", err)
	}
	if archived.Primary.Commit != lockedCommit || archived.Primary.Repository != rules.RepositoryV2Fly {
		t.Errorf("the archive names %+v, want the outgoing pin at %s", archived.Primary, lockedCommit)
	}
	// And it is the canonical encoding, not the bytes that were on disk. The
	// outgoing lock here was written on one line, so a copy would be visibly
	// different from what this package writes -- which is the whole reason
	// Archive re-encodes rather than copies, and the reason choosing the
	// archiver on WHICH FLAG WAS TYPED is wrong: a compact lock parses, so the
	// machine is healthy and the canonical archive is the one that should
	// happen.
	raw, err := os.ReadFile(paths.previousLock)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := archived.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// Canonical is the expectation, and a verbatim copy is the failure. The
	// fixture makes them different documents, so exactly one of these two
	// comparisons can hold and the test says which.
	if bytes.Equal(raw, compact) {
		t.Errorf("the archive is a verbatim copy of the outgoing lock, which is the property this test exists to catch:\n%s", raw)
	}
	if !bytes.Equal(raw, canonical) {
		t.Errorf("the archive is not what this package writes:\n%s\nwant\n%s", raw, canonical)
	}
	if !strings.Contains(stdout, "pinned-commit: "+remoteCommit) {
		t.Errorf("the pin did not report the commit it published:\n%s", stdout)
	}
}

// The digest tripwire REFUSES, it does not warn. The archive covers the lock
// alone -- nothing here archives cn-domains.txt -- so publishing over a
// hand-edited list destroys the only trace of the edit, and a line on stderr is
// the whole record of it. And a refusal costs nothing legitimate: the lock
// Publish writes records the digest of the list Publish wrote, so a published
// pair can never disagree, and the only way here is a file changed by hand.
func TestPinSourceOverAHandEditedListIsRefused(t *testing.T) {
	paths := newListPaths(t)
	installed := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, installed)
	// Someone edited the published list; the lock still records the old digest.
	tampered := []byte("domain:tampered.cn\n")
	if err := os.WriteFile(paths.listFile, tampered, 0o640); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, paths.dir)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code == exitSuccess {
		t.Fatal("--pin-source published over a published list its own lock does not describe")
	}
	if !strings.Contains(stderr, "list_sha256") {
		t.Errorf("the refusal does not name the disagreement: %q", stderr)
	}
	// Both files still hold what they held: the list is the one that was edited
	// and the lock is the one that was published, because nothing was replaced.
	if list := mustReadFile(t, paths.listFile); !bytes.Equal(list, tampered) {
		t.Errorf("the published list is %q, want the %q that was on disk", list, tampered)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err == nil && found && lock.Primary.Commit != lockedCommit {
		t.Errorf("the pin moved to %s although the pair disagreed", lock.Primary.Commit)
	}
	if _, err := os.Stat(paths.previousLock); err == nil {
		t.Error("a refused pin left an archive, so the next rollback target is a pin that was never published")
	}
	// The snapshot taken before the run is asserted on, rather than taken and
	// discarded: a refusal that wrote something the two file checks above happen
	// to miss would still be a refusal that wrote something. The control lock is
	// excepted because this refusal happens while holding it, and creating it is
	// what holding it means.
	assertUnchanged(t, before, paths.dir, "control.lock")
}

// The v1 migration the flag exists for is UNAFFECTED by the refusal: a schema
// version 1 lock cannot parse, so the digest comparison never runs and there is
// nothing to disagree about. If this ever failed, the flag would have stopped
// being a way out of the state it was written for.
func TestPinSourceOverAV1LockStillPublishesWhereAParsingPairIsRefused(t *testing.T) {
	// The two cases side by side, because the fix is a split and a split is
	// only correct if each side still does what it did.
	t.Run("an unreadable lock is replaced", func(t *testing.T) {
		paths := newListPaths(t)
		publishV1Pair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
		origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
			remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
		})
		code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
			"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
			"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
		if code != exitSuccess {
			t.Fatalf("the v1 migration was refused, which is the one thing the flag exists to prevent: %s", stderr)
		}
	})
	t.Run("a readable pair that disagrees is refused", func(t *testing.T) {
		paths := newListPaths(t)
		publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
		if err := os.WriteFile(paths.listFile, []byte("domain:tampered.cn\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
			remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
		})
		code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
			"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
			"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
		if code == exitSuccess {
			t.Fatal("a pair that disagrees with itself was published over")
		}
		if !strings.Contains(stderr, "Nothing was published") {
			t.Errorf("the refusal does not say that nothing was written: %q", stderr)
		}
	})
}

// leaveCrashWindow publishes a pair and then replaces ONLY the list, which is
// byte for byte the state Publish leaves when the machine loses power between
// its two renames -- and it is the reason the digest refusal needs an escape.
//
// It is not a hand-edited file. Publish cannot rename the two files as one
// operation, so it publishes the list first and the lock second, and the window
// between them is a state this project creates on purpose. A refusal with no way
// past it would turn a transient state the publisher produces by design into a
// permanent one, and the only recovery would be a human editing a file under
// /var/lib -- which update-lists says is the outcome it exists to prevent.
func leaveCrashWindow(t *testing.T, paths listPaths, commit string, published, replaced string) {
	t.Helper()
	publishPair(t, paths, commit, []byte(published))
	if err := os.WriteFile(paths.listFile, []byte(replaced), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile); err == nil {
		t.Fatal("the fixture is a pair that agrees, so it is not the crash window")
	} else if !rules.PublishedPairRefuses(err) {
		t.Fatalf("the fixture does not produce the state --force answers: %v", err)
	}
}

// The escape exists. Without it the crash window Publish creates on purpose is
// a state nothing can publish over, and the recovery is a human editing a file
// under /var/lib.
func TestForcePublishesOverTheStateACrashBetweenRenamesLeaves(t *testing.T) {
	paths := newListPaths(t)
	leaveCrashWindow(t, paths, lockedCommit, "domain:previous.cn\n", "domain:halfwritten.cn\n")
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--force",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--force could not publish over the crash window, so the state Publish creates has no recovery: %s", stderr)
	}
	// The pair agrees again: that is the whole point, and a lock that parses is
	// not enough -- it has to describe the list actually on disk.
	published, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("the pair still does not describe itself after --force: %v", err)
	}
	if published.Primary.Commit != remoteCommit {
		t.Errorf("published %+v, want the remote commit %s", published.Primary, remoteCommit)
	}
}

// The refusal has to NAME the escape. A refusal that names a re-pin, which then
// refuses the same state, is the defect this task has now been round for twice.
func TestTheRefusalNamesTheCommandThatRecoversFromIt(t *testing.T) {
	paths := newListPaths(t)
	leaveCrashWindow(t, paths, lockedCommit, "domain:previous.cn\n", "domain:halfwritten.cn\n")
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code == exitSuccess {
		t.Fatal("a disagreeing pair was published over without --force")
	}
	for _, want := range []string{"--force", "--pin-remote"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q, so the operator is left with no way past it: %q", want, stderr)
		}
	}
	// And it has to say what the state IS. An operator who lost power
	// recognises "between the two renames"; one who did not should not conclude
	// from this message that they corrupted something.
	if !strings.Contains(strings.ToLower(stderr), "crash") && !strings.Contains(strings.ToLower(stderr), "power") {
		t.Errorf("the refusal does not say this is the state a crash leaves, so an operator who hit one "+
			"reads it as corruption: %q", stderr)
	}
}

// --force answers exactly one question. It is not a general override: a flag
// that also waved through a collapsed list or an unreviewed repository would
// make every other refusal in this command decorative.
func TestForceAnswersOnlyTheDisagreement(t *testing.T) {
	t.Run("it does not waive the collapse guardrail", func(t *testing.T) {
		paths := newListPaths(t)
		publishChinaPair(t, paths, lockedCommit, rulesOf(200))
		// Upstream now publishes a fraction of the list. That is refused on size
		// alone, and --force is not in the conversation.
		origin := newFakeOriginSources(t, map[string]fakeSource{
			rules.RepositoryChina: {
				head: remoteCommit,
				archives: map[string][]byte{
					remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:only.cn\n"),
				},
			},
			rules.RepositoryV2Fly: {
				head: remoteCommit,
				archives: map[string][]byte{
					remoteCommit: supplementArchive(t, remoteCommit, 2),
				},
			},
		})
		before := snapshot(t, paths.dir)
		code, _, stderr := runCLI(t, origin.services(), "--automatic",
			"--policy", automaticPolicy(t, true, false), "--force",
			"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
			"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
		if code == exitSuccess {
			t.Fatal("--force published a list that lost 98% of its rules")
		}
		if !strings.Contains(stderr, "refusing") {
			t.Errorf("the collapse refusal is not in the output: %q", stderr)
		}
		assertUnchanged(t, before, paths.dir, "")
	})
	t.Run("it does not waive the repository allowlist", func(t *testing.T) {
		// The allowlist is checked in the parser, before a URL exists, so no
		// combination of flags can reach the fetch with an unreviewed name.
		for _, args := range [][]string{
			{"--pin-remote", "HEAD", "--force", "--pin-source", "attacker/domain-list"},
			{"--pin-remote", "HEAD", "--force", "--pin-source", "../v2fly/domain-list-community"},
		} {
			if _, err := parseUpdateListOptions(io.Discard, args); err == nil {
				t.Errorf("%v was accepted, so --force reaches past the allowlist", args)
			}
		}
	})
	t.Run("it does not waive an unreadable v1 lock", func(t *testing.T) {
		// A v1 lock is replaced deliberately rather than forced, because naming
		// --pin-source already said it. --force changes nothing about it, and the
		// refusal for a pair that is BOTH unparseable and disagreeing is the
		// unreadable one: there is nothing to compare a digest against.
		paths := newListPaths(t)
		publishV1Pair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
		origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
			remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
		})
		code, _, _ := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--force",
			"--pin-source", rules.RepositoryV2Fly,
			"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
			"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
		if code != exitSuccess {
			t.Errorf("--force broke the v1 migration it was not meant to touch (exit %d)", code)
		}
	})
}

// A flag that is refused where it has nothing to do teaches an operator that
// the command is fussy. --force on a healthy machine is a no-op that publishes.
func TestForceOnAPairThatAgreesIsHarmless(t *testing.T) {
	paths := newListPaths(t)
	installed := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, installed)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--force",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--force on a healthy pair exited %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	published, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("the pair does not describe itself after --force: %v", err)
	}
	if published.Primary.Commit != remoteCommit {
		t.Errorf("published %+v, want %s", published.Primary, remoteCommit)
	}
}

// The flag's value becomes a path in a URL, so it is checked against the
// allowlist where a caller supplies it rather than where the fetch would use it.
func TestTheSourceFlagIsRefusedUnlessItNamesAReviewedRepository(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "v2fly", args: []string{"--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly}},
		{name: "the dnsmasq source", args: []string{"--pin-remote", "HEAD", "--pin-source", rules.RepositoryChina}},
		{name: "a commit with the source", args: []string{"--pin-remote", lockedCommit, "--pin-source", rules.RepositoryChina}},
		{name: "an unreviewed repository", args: []string{"--pin-remote", "HEAD", "--pin-source", "attacker/domain-list"}, wantErr: "is not a reviewed source"},
		{name: "the owner without the name", args: []string{"--pin-remote", "HEAD", "--pin-source", "felixonmars"}, wantErr: "is not a reviewed source"},
		{name: "a path traversal", args: []string{"--pin-remote", "HEAD", "--pin-source", "../v2fly/domain-list-community"}, wantErr: "is not a reviewed source"},
		{name: "the right name in another case", args: []string{"--pin-remote", "HEAD", "--pin-source", "V2Fly/domain-list-community"}, wantErr: "is not a reviewed source"},
		{name: "without a pin", args: []string{"--pin-source", rules.RepositoryV2Fly}, wantErr: "requires --pin-remote"},
		{name: "with a check instead", args: []string{"--check", "--pin-source", rules.RepositoryV2Fly}, wantErr: "requires --pin-remote"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseUpdateListOptions(io.Discard, test.args)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("the flag set was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("the flag set was accepted, want a refusal naming %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

// The refusal names the command an operator can actually run, flag and all: a
// message that omits the flag names a command that deadlocks on the same lock.
func TestTheV1RefusalNamesACommandThatWorks(t *testing.T) {
	v1 := []byte(`{"schema_version":1,"repository":"v2fly/domain-list-community","commit":"` +
		strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) +
		`","list_sha256":"` + strings.Repeat("e", 64) + `","entry":"data/cn"}`)
	_, err := rules.ParseSourceLock(v1)
	if err == nil {
		t.Fatal("a v1 lock was accepted")
	}
	if !strings.Contains(err.Error(), "--pin-remote HEAD --pin-source") {
		t.Fatalf("the refusal names a command that cannot follow it, because it omits the flag that names the source: %v", err)
	}
}

// Without the flag there is no way to answer which repository, so the refusal
// has to say that rather than naming a repository nobody asked about.
func TestARefusalToDefaultTheRepositoryNamesTheFlagThatWouldHaveWorked(t *testing.T) {
	paths := newListPaths(t)
	publishV1Pair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code == exitSuccess {
		t.Fatal("--pin-remote published over a v1 pair without being told which source to use")
	}
	if !strings.Contains(stderr, "--pin-source") {
		t.Fatalf("the refusal does not say which flag would have worked: %q", stderr)
	}
	if !strings.Contains(stderr, rules.RepositoryV2Fly) || !strings.Contains(stderr, rules.RepositoryChina) {
		t.Fatalf("the refusal does not name the sources it would accept: %q", stderr)
	}
}

func TestAPinTakesTheRepositoryFromThePublishedLockWhenNothingNamesIt(t *testing.T) {
	// A pin asked for by name has no other way to learn which source this machine
	// runs: the published lock is the only document here that says so. Asking a
	// different repository would fetch an archive of another source's content
	// and publish it under a lock naming the source the operator already had.
	paths := newListPaths(t)
	installed := []byte("domain:previous.cn\n")
	publishChinaPair(t, paths, lockedCommit, installed)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, "domain:repinned.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {head: remoteCommit},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if want := origin.archivePath(rules.RepositoryChina, lockedCommit); !origin.fetched(want) {
		t.Fatalf("the pin fetched %v, want the archive at %q", origin.requests, want)
	}
	if other := origin.archivePath(rules.RepositoryV2Fly, lockedCommit); origin.fetched(other) {
		t.Fatalf("the pin fetched %q, which belongs to a source this machine does not run", other)
	}
	published, err := rules.ParseSourceLock(mustReadFile(t, paths.sourceLock))
	if err != nil {
		t.Fatalf("parse the published lock: %v", err)
	}
	if published.Primary.Repository != rules.RepositoryChina {
		t.Errorf("the published lock names %q, want %q", published.Primary.Repository, rules.RepositoryChina)
	}
}

func TestTheUnattendedRefreshResolvesAndPublishesFromThePinnedRepository(t *testing.T) {
	// The refresh measures one source and then hands its commit to the pin
	// path. Both halves have to be about the same repository: a commit id names
	// a file inside one of them, so resolving the measured commit against the
	// other would verify a digest against an archive the commit is not in. This
	// is the case that fails loudly, at the publish, after the whole unattended
	// run has been built on the wrong repository.
	paths := newListPaths(t)
	installed := []byte("domain:previous.cn\n")
	publishChinaPair(t, paths, lockedCommit, installed)
	policy := automaticPolicy(t, true, false)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:refreshed.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {
			head: lockedSupplementCommit,
			archives: map[string][]byte{
				lockedSupplementCommit: supplementArchive(t, lockedSupplementCommit, 2),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--automatic", "--policy", policy,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--automatic exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	for _, path := range []string{
		origin.commitPath(rules.RepositoryChina),
		origin.archivePath(rules.RepositoryChina, remoteCommit),
	} {
		if !origin.fetched(path) {
			t.Errorf("--automatic fetched %v, want %q", origin.requests, path)
		}
	}
	// The supplement IS fetched, because the dnsmasq primary cannot express the
	// rule kinds it carries -- but from its OWN repository and at its OWN commit.
	// That is the half of the rule this case exists for: the measured commit is a
	// v2fly commit here, and asking the dnsmasq repository for it would verify a
	// digest against an archive that commit is not in.
	if other := origin.archivePath(rules.RepositoryChina, lockedSupplementCommit); origin.fetched(other) {
		t.Errorf("--automatic fetched %q, which is a commit of the other repository", other)
	}
	if want := origin.archivePath(rules.RepositoryV2Fly, lockedSupplementCommit); !origin.fetched(want) {
		t.Errorf("--automatic fetched %v, want the supplement at %q", origin.requests, want)
	}
	if !strings.Contains(stdout, "pinned-commit: "+remoteCommit) {
		t.Errorf("the refresh did not publish the measured commit:\n%s", stdout)
	}
	published, err := rules.ParseSourceLock(mustReadFile(t, paths.sourceLock))
	if err != nil {
		t.Fatalf("parse the published lock: %v", err)
	}
	if published.Primary.Repository != rules.RepositoryChina || published.Primary.Commit != remoteCommit {
		t.Errorf("the published lock names %+v, want the dnsmasq source at %s", published.Primary, remoteCommit)
	}
}

// --- unattended refresh -------------------------------------------------

// automaticPolicy writes a policy with the two switches set as asked, and the rest of
// the reviewed defaults, so a case is about the switches and not about a policy it
// had to invent.
func automaticPolicy(t *testing.T, china, cloudflare bool) string {
	t.Helper()
	policy := config.Defaults()
	policy.Lists.China.Automatic = china
	policy.Lists.Cloudflare.Automatic = cloudflare
	encoded, err := config.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runAutomatic runs the unattended refresh against origin, on a policy with only the
// China switch on, writing the pair into paths.
func runAutomatic(t *testing.T, paths listPaths, origin *fakeOrigin) (int, string, string) {
	t.Helper()
	return runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, true, false),
		"--source-lock", paths.sourceLock,
		"--list-file", paths.listFile,
		"--previous-lock", paths.previousLock,
		"--control-lock", paths.controlLock)
}

// rulesOf builds a China list with n expressions in it, for the cases about how many
// rules an incoming list has.
func rulesOf(n int) []byte {
	var list bytes.Buffer
	for index := 0; index < n; index++ {
		fmt.Fprintf(&list, "domain:host%05d.example.cn\n", index)
	}
	return list.Bytes()
}

// With both switches off, --automatic must do exactly what --check did. The shipped
// default has to behave identically to the mode it replaces, or turning the feature
// on and off changes the machine's behaviour rather than its configuration.
func TestAutomaticWithBothSwitchesOffIsTheCheck(t *testing.T) {
	paths := newListPaths(t)
	publishPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOrigin(t, lockedCommit, map[string][]byte{
		lockedCommit: sourceArchive(t, lockedCommit, "domain:first.cn\n"),
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, false, false),
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--automatic with both switches off exited %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "up-to-date:") {
		t.Errorf("the daily report lost its drift line:\n%s", stdout)
	}
	// Nothing was written. Not the pair, not the archive -- an archive taken on a run
	// that refreshed nothing is a rollback target that was never about to be replaced.
	if _, err := rules.ReadPrevious(paths.previousLock); err == nil {
		t.Error("a run that refreshed nothing left an archive")
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit != lockedCommit {
		t.Errorf("the pin moved to %s with both switches off", lock.Primary.Commit)
	}
}

// The report is produced either way. A machine that has the switches on should not
// stop being told what its pin is, because that report is the only place the answer
// is written down at all.
func TestAutomaticStillReportsDriftWhenItRefreshes(t *testing.T) {
	paths := newListPaths(t)
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, true, false),
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--automatic with lists.china.automatic on exited %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "up-to-date:") {
		t.Errorf("the drift line is gone once the switch is on:\n%s", stdout)
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit != remoteCommit {
		t.Errorf("lists.china.automatic was on and the pin stayed at %s", lock.Primary.Commit)
	}
	archived, err := rules.ReadPrevious(paths.previousLock)
	if err != nil {
		t.Fatalf("the refresh left no rollback target: %v", err)
	}
	if archived.Primary.Commit != lockedCommit {
		t.Errorf("the archive names %s, want the pin that was current (%s)", archived.Primary.Commit, lockedCommit)
	}
}

// The two switches are two switches. Cloudflare's ranges choose an edge; data/cn
// chooses which names take the foreign branch. Turning on the cheap one must not
// open the expensive one.
//
// It uses the range fixture rather than the China-list one, and that is not a style
// choice. The China fixture's client rewrites every request to one server, so a
// refresh of the ranges reaches the GitHub origin's handler and 404s -- and then the
// pinned snapshot this package SHIPS stands in, at /usr/share/mosdns-router. On a
// machine with the package installed that made the case pass, and on a clean checkout
// it failed, which is exactly what the CI run this case was written for turned up.
// The range fixture serves both hosts and the snapshot paths are the fixture's own,
// so the case is about the two switches on every machine.
func TestAutomaticHonoursEachSwitchSeparately(t *testing.T) {
	paths := newRangePaths(t)
	publishPair(t, paths.listPaths(), lockedCommit, rulesOf(200))
	ranges := newRangeOrigin(t)
	github := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(200))),
	})

	var stdout, stderr bytes.Buffer
	code := runWith(append([]string{"update-lists", "--automatic",
		"--policy", automaticPolicy(t, false, true),
		"--previous-lock", filepath.Join(paths.dir, "source-lock.previous.json")},
		refreshArgs(paths)...), &stdout, &stderr, ranges.services(github.client))
	if code != exitSuccess {
		t.Fatalf("--automatic with lists.cloudflare.automatic on exited %d (stderr: %s)",
			code, stderr.String())
	}
	published, err := os.ReadFile(paths.prefixList)
	if err != nil {
		t.Fatalf("the ranges were not published: %v", err)
	}
	if want := "104.16.0.0/22\n172.64.0.0/21\n"; string(published) != want {
		t.Errorf("published prefix list = %q, want %q", published, want)
	}
	if !strings.Contains(stdout.String(), "published-prefix-list:") {
		t.Errorf("the report does not say the ranges were published:\n%s", stdout.String())
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit != lockedCommit {
		t.Errorf("the China list was re-pinned to %s with lists.china.automatic off", lock.Primary.Commit)
	}
	if _, err := rules.ReadPrevious(filepath.Join(paths.dir, "source-lock.previous.json")); err == nil {
		t.Error("refreshing only the ranges left an archive of the China pin")
	}
}

// Review Focus, class 1. An upstream commit whose data/cn is a fraction of its former
// size VERIFIES -- it really is the upstream document -- and would send far more names
// down the foreign branch than anybody reviewed. The guard refuses and says so; the
// operator can then publish that commit deliberately, having read the diff.
func TestAListThatShrinksSharplyIsRefused(t *testing.T) {
	paths := newListPaths(t)
	published := rulesOf(200)
	publishChinaPair(t, paths, lockedCommit, published)
	// Upstream now publishes one rule where this machine has two hundred.
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:first.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, true, false),
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code == exitSuccess {
		t.Fatal("a list that fell from 200 rules to 1 was published")
	}
	if !strings.Contains(stderr, "fewer rules") {
		t.Errorf("the refusal does not say what it noticed: %q", stderr)
	}
	lock, list, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit != lockedCommit {
		t.Errorf("the refused list was published anyway, moving the pin to %s", lock.Primary.Commit)
	}
	if !bytes.Equal(list, published) {
		t.Error("the refused list replaced the published one")
	}
	// And no archive: the refusal happened before anything was written, so there is
	// no rollback target to be had and none is claimed.
	if _, err := rules.ReadPrevious(paths.previousLock); err == nil {
		t.Error("a refused refresh left an archive")
	}
}

// A real curation commit removes a few rules and must NOT trip the guard, or the
// guard is just a way of never updating -- which is the same as being off with more
// machinery attached.
func TestASmallReductionIsPublished(t *testing.T) {
	paths := newListPaths(t)
	publishChinaPair(t, paths, lockedCommit, rulesOf(1000))
	// 980 of 1000: a 2% reduction, comfortably clear of the threshold, because a case
	// balanced exactly on a boundary passes for the wrong reason when the boundary
	// moves by one.
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(980))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, true, false),
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("a 2%% reduction was refused: %s", stderr)
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit != remoteCommit {
		t.Errorf("the reduced list was not published, the pin is still %s", lock.Primary.Commit)
	}
}

// Review Focus, class 5. A machine with no route out runs this daily. It must fail,
// it must say so, and it must leave the pin and the archive exactly as they were.
func TestAFailedAutomaticRunTouchesNothing(t *testing.T) {
	paths := newListPaths(t)
	published := rulesOf(200)
	publishPair(t, paths, lockedCommit, published)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(200))),
	})
	origin.status = http.StatusInternalServerError

	code, stdout, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, true, false),
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code == exitSuccess {
		t.Fatalf("a run that could not reach upstream reported success:\n%s", stdout)
	}
	if !strings.Contains(stderr, "unexpected status") {
		t.Errorf("the failure is not in the report: %q", stderr)
	}
	lock, list, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit != lockedCommit {
		t.Errorf("the pin changed on a failed run: %s -> %s", lockedCommit, lock.Primary.Commit)
	}
	if !bytes.Equal(list, published) {
		t.Error("the list changed on a failed run")
	}
	if _, err := rules.ReadPrevious(paths.previousLock); err == nil {
		t.Error("a failed run left an archive, so a rollback would land on a commit that " +
			"was only ever about to be replaced")
	}
}

// The guard measured one commit, so the publication has to be that commit.
//
// Upstream publishes a healthy list, and then -- while this run is working -- moves
// to a commit whose list has collapsed. If the refresh re-resolved HEAD after
// measuring, the size check would have applied to one commit and the publication to
// another, so upstream moving during the run would publish an unmeasured list through
// the very mechanism built to prevent that. The run therefore has to end up on the
// commit it measured, and on nothing else.
func TestTheAutomaticRefreshPublishesTheCommitItMeasured(t *testing.T) {
	paths := newListPaths(t)
	published := rulesOf(200)
	publishChinaPair(t, paths, lockedCommit, published)
	measured := remoteCommit
	collapsed := "3333333333333333333333333333333333333333"
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: measured,
			archives: map[string][]byte{
				measured:  chinaSourceArchive(t, measured, string(rulesOf(200))),
				collapsed: chinaSourceArchive(t, collapsed, "domain:only.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
	})
	// Upstream answers `measured` for the first THREE resolutions of the PRIMARY's
	// HEAD and `collapsed` from the fourth on. Three is the drift report's two --
	// a cheap commit check, then the archive digest it reports -- plus the
	// refresh's one measurement. A fourth primary resolution is therefore a caller
	// that ignored that measurement and asked again, which is the defect this case
	// is about; since the measured ref is handed to the publication as it stands
	// rather than re-resolved, a correct run never reaches one.
	//
	// The count is of the PRIMARY's resolutions ONLY, and the supplement's is
	// deliberately excluded. A run also resolves the supplement against its own
	// repository, so counting every HEAD request put the fourth at the SUPPLEMENT
	// -- which meant the gate fired on every correct run and therefore said nothing
	// about the defect it names, while a real re-resolution of the primary would
	// have been the fifth and tripped nothing either. The gate was decorative; the
	// scenario behind it was not, and this is the count that says so.
	//
	// The number is ASSERTED below rather than assumed, which is what the earlier
	// version of this comment claimed and did not do: if the flow ever stops
	// resolving the primary exactly three times, this fails with that fact instead
	// of quietly going on testing nothing.
	//
	// The flip takes effect on the request that sets it, because the origin answers
	// from the field after the hook runs. Getting that backwards makes the FIRST
	// resolution the collapsed one, which is how the first attempt at this case
	// refused a run that was doing exactly the right thing.
	primaryResolutions := 0
	origin.onServe = func(o *fakeOrigin, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/commits/HEAD") {
			return
		}
		if !strings.Contains(request.URL.Path, rules.RepositoryChina) {
			return
		}
		primaryResolutions++
		if primaryResolutions >= 4 {
			o.setHead(rules.RepositoryChina, collapsed)
		}
	}
	code, _, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, true, false),
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--automatic exited %d (stderr: %s)", code, stderr)
	}
	// The anchor, checked. A correct run resolves the primary three times; if that
	// number moves, the gate above is no longer sitting where a re-resolution would
	// land, and every assertion below it would pass without ever being threatened.
	if primaryResolutions != 3 {
		t.Fatalf("this case is anchored to the primary being resolved 3 times and it was "+
			"resolved %d times, so the gate that flips the origin to the collapsed commit no "+
			"longer fires on a caller that re-resolves after measuring: update the anchor",
			primaryResolutions)
	}
	lock, list, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit == collapsed {
		t.Fatalf("published %s, which nothing measured: its list has 1 rule where %d were published",
			collapsed, countListRules(published, lock.Supplement))
	}
	if lock.Primary.Commit != measured {
		t.Errorf("published %s, want the commit that was measured (%s)", lock.Primary.Commit, measured)
	}
	// And the list that is published is the measured commit's, not the collapsed
	// one's. Said by what is IN it rather than by counting: the measured archive
	// holds two hundred domains and the collapsed one holds a single different
	// domain, so each is named and the other forbidden.
	publishedList := string(list)
	if !strings.Contains(publishedList, "domain:host00199.example.cn") {
		t.Errorf("the published list is not the measured commit's -- its last domain is missing:\n%s", list)
	}
	if strings.Contains(publishedList, "domain:only.cn") {
		t.Errorf("the published list is the collapsed commit's, which nothing measured:\n%s", list)
	}
}

// --- the refresh decision ------------------------------------------------

// A supplement that lost its rules is refused, and the union cannot see it.
//
// The published list is 2,002 rules of which the supplement contributed two. The
// incoming list is 2,003 rules of which the supplement contributed ONE: the union
// GREW, so a comparison of the whole list against the whole list sees a healthy
// update and publishes. Production has the same shape and a worse ratio -- seventy
// rules beside 110,551 is 0.063%, so a supplement that lost every single rule would
// move the union by less than a tenth of a percent and pass a 5% guardrail.
//
// The two counts are compared because they are the only two that are comparable: the
// primary's against the primary's, the supplement's against the supplement's.
func TestASupplementThatLostItsRulesIsRefusedAndPublishesNothing(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(2000), supplementRules(2)...)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, published)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(2000))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 1),
			},
		},
	})
	before := snapshot(t, paths.dir)

	code, _, stderr := runAutomatic(t, paths, origin)
	if code == exitSuccess {
		t.Fatalf("a supplement that lost half its rules was published: 1 where %d were published, "+
			"inside a list that grew", 2)
	}
	if !strings.Contains(stderr, "fewer rules") {
		t.Fatalf("the refusal does not say what it noticed: %s", stderr)
	}
	if !strings.Contains(stderr, "supplement") {
		t.Errorf("the refusal does not say which source collapsed: %s", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// The mirror of that case: the primary is judged by its own count, not by a union
// that the supplement can outvote. Twenty primary rules become four, and the
// supplement happens to gain twenty, so the list grows from 40 to 44 -- and a
// comparison of the whole list publishes a list that lost four fifths of the
// domains that decide which names take the foreign branch.
func TestThePrimaryIsJudgedByItsOwnCountRatherThanTheUnion(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(20), supplementRules(20)...)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, published)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				// Twenty published domains become two; the fixture's other two
				// entries are the reviewed ones this converter always requires.
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:host00000.example.cn\ndomain:host00001.example.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 40),
			},
		},
	})
	before := snapshot(t, paths.dir)

	code, _, stderr := runAutomatic(t, paths, origin)
	if code == exitSuccess {
		t.Fatal("a primary that fell from 20 rules to 4 was published, because the supplement grew")
	}
	if !strings.Contains(stderr, "fewer rules") || !strings.Contains(stderr, "primary") {
		t.Fatalf("the refusal does not name the source that collapsed: %s", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// A machine published from the dnsmasq primary with NO supplement gains one even
// though its primary has not moved.
//
// This is the state a first migration from the fallback leaves behind: a dnsmasq
// primary published without the second source, because the fallback publication it
// replaced had none and the refresh that installed the primary refused to carry one it
// could not find. A manual `--pin-source felixonmars/dnsmasq-china-list` first install
// publishes the same shape.
//
// Reporting "already at" here would freeze the machine on a list that is missing every
// rule the supplement carries -- the dnsmasq syntax cannot express `full:` or `regexp:`,
// so those ~70 rules are simply not on disk -- for as long as the primary's own HEAD
// stays quiet. So "already at" has to mean "both sources are where this machine left
// them", and a source this machine has never published is not where it left it.
func TestAMachineWithNoSupplementGainsOneEvenWhenThePrimaryHasNotMoved(t *testing.T) {
	paths := newListPaths(t)
	// No supplement beside the primary: this is the pair a first migration and a
	// manual first install both leave behind.
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			// The primary has NOT moved. That is the whole point of the case: the
			// run must still act, because the second source is new to this machine.
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
	})

	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code != exitSuccess {
		t.Fatalf("--automatic exited %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	lock, list, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatalf("the refresh published a pair that does not describe itself: %v", err)
	}
	if strings.Contains(stdout, "already at") {
		t.Fatalf("the refresh reported already-at while the machine had never published a "+
			"supplement, so the rules only it can express stay missing for good:\n%s", stdout)
	}
	if lock.Supplement == nil {
		t.Errorf("the refresh left a dnsmasq primary published without a supplement, so the "+
			"list it wrote is missing every rule the dnsmasq syntax cannot express: %+v", lock)
	}
	for _, want := range []string{"full:exact000.cn", "full:exact001.cn"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the published list lost %q, which only the second source can express:\n%s", want, list)
		}
	}
	// The primary did not move, so it must not have been re-pointed at anything.
	if lock.Primary.Commit != lockedCommit {
		t.Errorf("the primary moved to %s although its own HEAD did not", lock.Primary.Commit)
	}
}

// The same run has to say that the supplement's guardrail was skipped, because there
// is no previous supplement on this machine to weigh it against -- the same argument
// the migration report makes for the primary, applied to the other source.
func TestANewlyGainedSupplementSaysItsGuardrailWasSkipped(t *testing.T) {
	paths := newListPaths(t)
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
	})

	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code != exitSuccess {
		t.Fatalf("--automatic exited %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "supplement") || !strings.Contains(stdout, "guardrail") {
		t.Errorf("the report does not say the supplement's guardrail was skipped and why, so a "+
			"reader cannot tell it from a run that weighed it: %s", stdout)
	}
}

// The supplement is a source in its own right, so it is refreshed when it moves --
// including on a day the primary does not. Reporting "already at" because the PRIMARY
// is unchanged would freeze the seventy rules the primary cannot express for as long
// as the other repository stays quiet, which is not a state anybody chose.
func TestASupplementIsRefreshedWhenThePrimaryHasNotMoved(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(200), supplementRules(2)...)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, published)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			// The primary is still where this machine left it.
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 3),
			},
		},
	})

	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code != exitSuccess {
		t.Fatalf("--automatic exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	if strings.Contains(stdout, "already at") {
		t.Fatalf("the refresh reported already-at while the supplement had moved on:\n%s", stdout)
	}
	// It was asked of the repository the supplement lives in, not of the primary's.
	if want := origin.commitPath(rules.RepositoryV2Fly); !origin.fetched(want) {
		t.Errorf("--automatic fetched %v, want the HEAD of the supplement's repository", origin.requests)
	}
	publishedLock, err := rules.ParseSourceLock(mustReadFile(t, paths.sourceLock))
	if err != nil {
		t.Fatalf("parse the published lock: %v", err)
	}
	if publishedLock.Supplement == nil {
		t.Fatalf("the published lock has no supplement: %+v", publishedLock)
	}
	if publishedLock.Supplement.Commit != remoteCommit {
		t.Errorf("the published supplement is %s, want the refreshed %s",
			publishedLock.Supplement.Commit, remoteCommit)
	}
	if publishedLock.Primary.Commit != lockedCommit {
		t.Errorf("the primary moved to %s although its own HEAD did not", publishedLock.Primary.Commit)
	}
}

// A machine with a good list keeps it when the origin cannot be reached.
//
// "Only publish the fallback when there is nothing" is the whole rule. The last good
// result is strictly better than the fallback -- which is an eighth the size -- and a
// failed refresh that replaced it would turn an outage into a downgrade.
func TestAMachineWithAGoodListKeepsItWhenTheOriginIsUnreachable(t *testing.T) {
	paths := newListPaths(t)
	published := []byte("domain:kept.cn\n")
	publishChinaPair(t, paths, lockedCommit, published)
	origin := newFakeOriginSources(t, map[string]fakeSource{})
	origin.status = http.StatusBadGateway
	before := snapshot(t, paths.dir)

	runAutomatic(t, paths, origin)

	if !bytes.Equal(mustReadFile(t, paths.listFile), published) {
		t.Errorf("a failed refresh replaced a good list:\n got: %q\nwant: %q",
			mustReadFile(t, paths.listFile), published)
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatalf("the published pair no longer agrees with itself: %v", err)
	}
	if lock.Primary.Commit != lockedCommit {
		t.Errorf("a failed refresh moved the pin to %s", lock.Primary.Commit)
	}
	assertUnchanged(t, before, paths.dir, filepath.Base(paths.controlLock))
}

// A cold start with no route out is a machine whose every query goes abroad. The
// fallback is worse than the list it would have published and better than a hole, and
// it is the ONLY publication a failed refresh may make: the guardrail is what keeps
// that true, since 110,551 down to 8,363 is a 92.4% drop and no refresh could
// mistake the fallback for a list that shrank.
func TestAColdStartWithNoRoutePublishesTheFallbackRatherThanNothing(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{})
	origin.status = http.StatusBadGateway

	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code == exitSuccess {
		t.Fatalf("a cold start that could not reach the origin reported success: %s%s", stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "fallback") {
		t.Fatalf("the report does not name the fallback: %s%s", stdout, stderr)
	}
	if _, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile); err != nil || found {
		t.Fatalf("nothing could be fetched, so nothing should have been published: found=%t err=%v", found, err)
	}
}

// A cold start on a WORKING network installs the primary. The fallback is what
// this project falls back TO, not what it starts on.
//
// A fresh machine that resolved the fallback's repository, published 8,363 rules
// and exited 0 would be running an eighth of the list this change exists to ship --
// and, before the migration rule below, on that list forever, while every report
// said it had published the reviewed default.
func TestAColdStartWithAReachableOriginPublishesThePrimary(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(300))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
			},
		},
	})

	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code != exitSuccess {
		t.Fatalf("--automatic on a machine with nothing exited %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("the cold start published no pair: found=%t err=%v", found, err)
	}
	if lock.Primary.Repository != rules.RepositoryChina {
		t.Errorf("a cold start on a working network published %q, want the primary %q",
			lock.Primary.Repository, rules.RepositoryChina)
	}
	// The primary cannot express the kinds a supplement carries, so a primary
	// published alone silently drops them. This is a first migration as much as a
	// cold start: a fallback publication has no supplement by construction.
	if lock.Supplement == nil {
		t.Errorf("the primary was published without the supplement whose rules its syntax "+
			"cannot express: %+v", lock)
	}
	if len(list) == 0 {
		t.Error("the cold start published an empty list, which sends every query abroad")
	}
}

// A machine published from the fallback migrates. The fallback is not the primary
// -- it is what this project falls back TO -- so a refresh that asked the fallback
// where it had moved would answer a question nobody asked, and republish 8,363
// rules forever while the primary was never fetched at all.
//
// The origin serves BOTH repositories, because a fixture that served only the one
// the machine is on could not tell this apart from "kept refreshing where it was",
// which is the defect.
func TestAMachinePublishedFromTheFallbackMigratesToThePrimary(t *testing.T) {
	paths := newListPaths(t)
	// publishPair is the fallback publication: v2fly as the primary and no
	// supplement, which is the only shape a v2fly primary may have.
	publishPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				// The supplement's kinds, and not the fallback's: a v2fly list
				// converted as a supplement keeps only the rule forms the dnsmasq
				// syntax cannot say, and a fixture that served plain `domain:` lines
				// would resolve to nothing and be refused before it got here.
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(300))),
			},
		},
	})

	code, _, stderr := runAutomatic(t, paths, origin)
	if code != exitSuccess {
		t.Fatalf("--automatic exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatalf("the migration published a pair that does not describe itself: %v", err)
	}
	if lock.Primary.Repository != rules.RepositoryChina {
		t.Fatalf("the machine was still published from %q after a refresh, so it would follow "+
			"the fallback forever; want the primary %q", lock.Primary.Repository, rules.RepositoryChina)
	}
	if lock.Primary.Commit != remoteCommit {
		t.Errorf("the migrated pin names %s, want the primary's HEAD %s", lock.Primary.Commit, remoteCommit)
	}
	// The dnsmasq syntax cannot express what the supplement carries, so the migrated
	// machine takes one. The fallback publication had none by construction, so this
	// one has to be resolved fresh rather than carried.
	if lock.Supplement == nil {
		t.Errorf("the migration published a primary with no supplement, dropping every rule "+
			"only the supplement can express: %+v", lock)
	}
	// A migration is a real publication, so the fallback it replaces is a rollback
	// target.
	archived, err := rules.ReadPrevious(paths.previousLock)
	if err != nil {
		t.Errorf("the migration left no rollback target: %v", err)
	} else if archived.Primary.Repository != rules.RepositoryV2Fly {
		t.Errorf("the archive names %q, want the fallback this machine was on", archived.Primary.Repository)
	}
}

// The migration is the one transition where the two sources are not comparable, and
// the guardrail has to say so rather than skip in silence: a skipped guard that
// leaves no trace is indistinguishable from one that passed.
//
// The primary here publishes THREE rules where the fallback published 200. Against
// the fallback's count that is a collapse -- and across a source change it is a
// first migration with no baseline, which is exactly what the per-source rule says
// must not be compared.
func TestTheMigrationSaysWhichGuardrailItSkippedAndWhy(t *testing.T) {
	paths := newListPaths(t)
	publishPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				// Three rules where the fallback published two hundred.
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:only.cn\n"),
			},
		},
	})

	code, stdout, stderr := runAutomatic(t, paths, origin)
	if code != exitSuccess {
		t.Fatalf("a first migration was refused against the rule count of the source it is "+
			"leaving, which is the cross-source comparison the guard exists to refuse: %s%s", stdout, stderr)
	}
	if !strings.Contains(stdout, rules.RepositoryChina) || !strings.Contains(stdout, rules.RepositoryV2Fly) {
		t.Errorf("the report does not name the source it moved between: %s", stdout)
	}
	if !strings.Contains(stdout, "guardrail") {
		t.Errorf("the report does not say a guardrail was skipped, so a reader cannot tell a "+
			"first migration from an ordinary refresh that passed one: %s", stdout)
	}
	lock, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Repository != rules.RepositoryChina {
		t.Errorf("published %q, want the primary %q", lock.Primary.Repository, rules.RepositoryChina)
	}
}

// A published pair naming a repository this build does not know is refused BY
// NAME. It is not defaulted to one of the two reviewed sources: a lock is the
// record of what was reviewed, and quietly moving the machine onto a different
// repository is a publication nobody asked for.
func TestARefreshRefusesAPublishedSourceItDoesNotKnowByName(t *testing.T) {
	paths := newListPaths(t)
	list := rulesOf(200)
	// The allowlist is a rules-layer gate, so this pair cannot be produced by any
	// command here -- which is the point. The case is about what a refresh does
	// when it finds one anyway.
	writePublishedPair(t, paths, rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: "attacker/domain-list",
			Commit:     lockedCommit,
			SHA256:     sha256Hex([]byte("an archive nobody reviewed")),
			Entry:      rules.V2FlyEntry,
		},
		ListSHA256: sha256Hex(list),
	}, list)
	before := snapshot(t, paths.dir)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(300))),
			},
		},
	})

	code, _, stderr := runAutomatic(t, paths, origin)
	if code == exitSuccess {
		t.Fatal("a refresh ran against a lock naming a repository it does not know")
	}
	if !strings.Contains(stderr, "attacker/domain-list") {
		t.Errorf("the refusal does not name the repository it refused: %q", stderr)
	}
	assertUnchanged(t, before, paths.dir, "")
}

// The repository a commit is resolved against comes from the pin, and a MEASURED pin
// is not overridable.
//
// refreshChinaList hands runPinRemote the source it downloaded and converted. If an
// operator's --pin-source could still win, an unattended run would resolve a
// measured commit against a repository that did not issue it -- which is either
// absent or, worse, present and unrelated -- and the guardrail would have measured
// one source while the publication took another.
//
// The parser cannot produce this state: --pin-source requires --pin-remote, and
// --automatic and --pin-remote are mutually exclusive. The case exists anyway,
// because the rule has to hold in runPinRemote rather than in the parser that
// happens to make it unreachable today.
func TestAMeasuredRepositoryCannotBeRedirectedByAPinSource(t *testing.T) {
	paths := newListPaths(t)
	options, err := parseUpdateListOptions(io.Discard, []string{
		"--pin-remote", remoteCommit,
		"--source-lock", paths.sourceLock,
		"--list-file", paths.listFile,
		"--control-lock", paths.controlLock,
		"--previous-lock", paths.previousLock,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The two fields a scheduled run sets, and the operator's flag that must not
	// reach past them.
	options.pinSource = rules.RepositoryV2Fly
	options.primaryPin = &rules.SourceRef{Repository: rules.RepositoryChina}
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:measured.cn\n"),
			},
		},
		// At a commit of its own. A first install resolves the supplement from this
		// repository, so the fallback's archive IS fetched on a correct run here and
		// "was it fetched" cannot say which role it was fetched for. What says it is
		// the commit each repository is asked about: this run must never ask the
		// fallback for the primary's commit, which is what resolving the measured
		// source against --pin-source would look like.
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
				remoteCommit:           sourceArchive(t, remoteCommit, firstInstallArchive),
			},
		},
	})

	var stdout, stderr bytes.Buffer
	code := runPinRemote(context.Background(), options, &stdout, &stderr, origin.pinServices())
	if code != exitSuccess {
		t.Fatalf("the measured pin exited %d (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if crossed := origin.archivePath(rules.RepositoryV2Fly, remoteCommit); origin.fetched(crossed) {
		t.Errorf("--pin-source %q overrode the measured repository: the run asked the fallback "+
			"for the primary's commit %s", rules.RepositoryV2Fly, remoteCommit)
	}
	published, err := rules.ParseSourceLock(mustReadFile(t, paths.sourceLock))
	if err != nil {
		t.Fatalf("parse the published lock: %v", err)
	}
	if published.Primary.Repository != rules.RepositoryChina {
		t.Errorf("published %q, want the repository that was measured (%q)",
			published.Primary.Repository, rules.RepositoryChina)
	}
}

// An operator's pin re-pins the PRIMARY. The second source has to come along for the
// ride: a publication that quietly dropped it would remove every rule only it can
// express, which is the loss the unattended refresh refuses to cause by accident.
//
// The supplement carried over is the one already published, not a fresh resolution,
// because this command was asked about one source. Carrying it means its archive is
// fetched again -- against the digest the lock records, which is why this case starts
// from a fixture whose digests the origin can actually serve.
func TestAPinKeepsTheSupplementThePublishedListAlreadyCarries(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(200), supplementRules(2)...)
	sources := map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: lockedSupplementCommit,
			archives: map[string][]byte{
				lockedSupplementCommit: supplementArchive(t, lockedSupplementCommit, 2),
			},
		},
	}
	origin := newFakeOriginSources(t, sources)
	publishChinaPairFromOrigin(t, paths, sources[rules.RepositoryChina], sources[rules.RepositoryV2Fly], published)

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, list, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatalf("the pin published a pair that does not describe itself: %v", err)
	}
	if lock.Supplement == nil {
		t.Fatalf("the pin dropped the second source: %+v", lock)
	}
	if lock.Supplement.Commit != lockedSupplementCommit {
		t.Errorf("the published supplement is %s, want the one already published (%s)",
			lock.Supplement.Commit, lockedSupplementCommit)
	}
	for _, want := range []string{"full:exact000.cn", "full:exact001.cn"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the published list lost %q, which only the second source can express:\n%s", want, list)
		}
	}
	if strings.Contains(stdout, "already at") {
		t.Errorf("the pin reported already-at instead of publishing:\n%s", stdout)
	}
}

// A pair that disagrees with itself still NAMES a supplement, and --force is the
// flag an operator reaches for precisely when the machine is in that state.
//
// The repository is recovered from the lock file for that case, which is what lets
// --force run at all. The supplement was not recovered with it, so the publication
// replaced a list built from two sources with one built from one -- and the seventy
// rules only the dnsmasq syntax cannot express left with the lock, silently, on the
// one path an operator took to repair the machine.
//
// It starts from a genuinely disagreeing pair rather than a consistent one: the
// existing carry-over case uses a pair that agrees, so ReadPublishedPair succeeds
// there and the recovery below is never reached.
func TestAForcedPinOverAPairThatDisagreesKeepsTheSupplementTheLockNames(t *testing.T) {
	paths := newListPaths(t)
	// The digests are the archives the origin can actually serve, because carrying
	// the supplement across the pin re-fetches it and verifies against the digest
	// the lock records.
	primaryLocked := chinaSourceArchive(t, lockedCommit, string(rulesOf(200)))
	primaryRemote := chinaSourceArchive(t, remoteCommit, string(rulesOf(200)))
	supplementLocked := supplementArchive(t, lockedSupplementCommit, 2)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: primaryRemote,
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				lockedSupplementCommit: supplementLocked,
				remoteCommit:           supplementArchive(t, remoteCommit, 2),
			},
		},
	})
	published := append(rulesOf(200), supplementRules(2)...)
	writePublishedPair(t, paths, rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryChina,
			Commit:     lockedCommit,
			SHA256:     sha256Hex(primaryLocked),
			Entries:    append([]string(nil), rules.ChinaConfEntries...),
		},
		Supplement: &rules.SourceRef{
			Repository: rules.RepositoryV2Fly,
			Commit:     lockedSupplementCommit,
			SHA256:     sha256Hex(supplementLocked),
			Entry:      rules.V2FlyEntry,
			Kinds:      rules.SupplementKinds(),
		},
		ListSHA256: sha256Hex(published),
	}, published)
	// Now break only the list, which is the crash-between-renames state --force
	// exists to publish over.
	leaveDisagreement(t, paths, "domain:halfwritten.cn\n")

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD", "--force",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--force over a disagreeing two-source pair exited %d (stderr: %s)", code, stderr)
	}
	lock, list, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatalf("the forced pin published a pair that does not describe itself: %v", err)
	}
	if lock.Supplement == nil {
		t.Fatalf("the pin published a lock naming no supplement, so the list it wrote dropped "+
			"every rule only the second source can express: %+v", lock)
	}
	if lock.Supplement.Commit != lockedSupplementCommit {
		t.Errorf("the carried supplement is %s, want the one the lock already named (%s)",
			lock.Supplement.Commit, lockedSupplementCommit)
	}
	for _, want := range []string{"full:exact000.cn", "full:exact001.cn"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the published list lost %q, which only the second source can express:\n%s", want, list)
		}
	}
	if strings.Contains(stdout, "already at") {
		t.Errorf("the pin reported already-at instead of publishing:\n%s", stdout)
	}
}

// leaveDisagreement replaces only the list, leaving the lock beside it describing
// a list that is no longer there -- the state Publish itself produces when the
// machine is interrupted between its two renames.
func leaveDisagreement(t *testing.T, paths listPaths, replaced string) {
	t.Helper()
	if err := os.WriteFile(paths.listFile, []byte(replaced), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile); err == nil {
		t.Fatal("the fixture is a pair that agrees, so it is not the crash window")
	} else if !rules.PublishedPairRefuses(err) {
		t.Fatalf("the fixture does not produce the state --force answers: %v", err)
	}
}

// The carry-over has to survive a lock this build cannot read at all, and when it
// cannot it has to say so rather than drop a second source in silence.
//
// The recovery reads the lock file directly, which is what makes the disagreement
// case work. A lock this build refuses outright -- a v1 document, which is the shape
// every machine upgrading from the previous schema has -- parses to nothing, so
// there is no supplement to recover and the publication carries none. That is
// correct, but it is also indistinguishable from the bug above unless the run says
// which happened.
//
// The assertion names the CONDITION rather than the word "supplement". A run that
// merely mentioned the word could satisfy it, and this round added two other
// diagnostics that mention it for different reasons -- one about a second source
// that could not be resolved, one about one this machine never had -- so a loose
// assertion here would now pass on the wrong note. The publication is asserted too:
// the note is only worth writing if the thing it describes is what happened.
//
// The origin serves BOTH repositories, which is the point of this case. Serving only
// the primary made it unable to see the decision it exists to pin down: a run that
// wrongly decided to resolve a second source from a lock it could not parse would
// fail to resolve it, print a different note, and publish the same pair with no
// supplement -- passing here for exactly the wrong reason. With the second source
// served, that run publishes a supplement, and the request assertion catches it
// before the publication one has to.
//
// The request assertion is the primary evidence that the unreadable path keeps its
// own behaviour: a v1 document has no supplement field, so one that does not parse
// cannot describe a second source, and saying "this machine had none to carry" about
// a document whose contents are unknown is a guess dressed as a reading. The
// widened "nothing to carry" rule excludes it for that reason.
func TestAPinOverALockItCannotReadSaysThatNoSupplementIsCarried(t *testing.T) {
	paths := newListPaths(t)
	publishV1Pair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:only.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryChina,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("the v1 re-pin exited %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "could not be read") {
		t.Errorf("the run published a lock naming no supplement without saying WHY, which is "+
			"what a publication that silently dropped one looks like: %q", stderr)
	}
	// Not consulted at all. This is the assertion that would notice if the unreadable
	// path stopped being excluded from the widened "nothing to carry" rule.
	if asked := origin.archivePath(rules.RepositoryV2Fly, remoteSupplementCommit); origin.fetched(asked) {
		t.Errorf("a lock this build cannot read was treated as one naming no second source, and "+
			"the run went to %s for a supplement the document cannot possibly have described", asked)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Supplement != nil {
		t.Errorf("the re-pin published %+v beside a lock it could not read, and the note above "+
			"is only true if nothing was carried over", lock.Supplement)
	}
}

// The v1 recovery path ENDS on a correct document. This is the follow-through on the
// case above, and it is why that one may legitimately publish no supplement rather
// than resolving one.
//
// A lock this build cannot read genuinely cannot describe a second source, so that
// publication has to carry none -- but it used to carry none FOREVER. The re-pin left
// a readable v2 lock naming the dnsmasq primary and no supplement, and every
// subsequent re-pin read that lock, saw something published, carried nothing, and
// published the primary alone again. An operator following the recovery path to its
// end arrived back where they started, silently.
//
// The second run below is ordinary: no flag, no v1 lock, nothing unusual. It is the
// second run of the recovery, and the point of the case is that it repairs the
// document rather than reproducing it.
func TestTheV1RecoveryPathEndsOnAPairWithBothSources(t *testing.T) {
	paths := newListPaths(t)
	publishV1Pair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: supplementArchive(t, remoteSupplementCommit, 2),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryChina,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("the v1 recovery run exited %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}

	// No flag this time: the lock the run above wrote now names the repository, and
	// this is the plain `--pin-remote HEAD` an operator types from here on.
	code, _, stderr = runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("the second run of the v1 recovery exited %d, want %d (stderr: %s)", code, exitSuccess, stderr)
	}
	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Supplement == nil {
		t.Errorf("the recovery path ended where it started: %+v. The operator ran the command the "+
			"refusal named, twice, and is still running a list missing every rule the dnsmasq "+
			"syntax cannot express", lock)
	}
	for _, want := range []string{"full:exact000.cn", "full:exact001.cn"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("the published list has %d rules and no %q, which the dnsmasq source "+
				"cannot express", countListRules(list, lock.Supplement), want)
		}
	}
}

// A machine that DELIBERATELY moves to the fallback must not be handed a supplement,
// and this is the case where the widened predicate could have surprised one.
//
// The state it starts from is the one this defect leaves behind -- a readable lock
// naming the dnsmasq primary and no supplement -- which under the widened rule is a
// machine with nothing to carry. If the rule were only "carry, or resolve", this
// operator would be given a v2fly supplement beside a v2fly primary, and
// rules.validatePinnedSource would refuse the pair: the run would exit 3 for an
// operator who did nothing wrong.
//
// So the repository is the guard, and it is checked AFTER the run rather than before
// it, because before the run the only evidence would be the absence of a fixture.
func TestAMachineMovedToTheFallbackByHandTakesNoSupplement(t *testing.T) {
	paths := newListPaths(t)
	// The no-supplement dnsmasq machine, moved onto the fallback deliberately.
	publishChinaPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				remoteSupplementCommit: sourceArchive(t, remoteSupplementCommit, string(rulesOf(200))),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("moving a machine to the fallback on purpose exited %d, want %d (stderr: %s)",
			code, exitSuccess, stderr)
	}
	lock, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if lock.Primary.Repository != rules.RepositoryV2Fly {
		t.Fatalf("the fallback was not published: %+v", lock)
	}
	if lock.Supplement != nil {
		t.Errorf("a v2fly primary was published beside %+v. That source already carries every "+
			"rule kind a supplement contributes, so the lock would be untrue about how the "+
			"list was built -- and rules.validatePinnedSource refuses the pair, which is how "+
			"this run would have exited 3 for an operator who did nothing wrong", lock.Supplement)
	}
	if strings.Contains(stderr, "supplement could not be resolved") {
		t.Errorf("a machine that asked for the fallback was told its second source had failed: %s", stderr)
	}
}

// Asking for the fallback on a machine that CARRIES a supplement is refused, and the
// refusal has to say how to get past it.
//
// The case above is the deliberate move from a machine that had no supplement; this is
// the same operator typing the same command on a machine that does. The run refuses --
// correctly: a v2fly primary already holds every kind a supplement contributes, so the
// pair would be a lock untrue about how the list was built, and publishing it is not an
// option -- but with nothing published and no remedy named, the only way forward the
// refusal leaves is editing a file under /var/lib by hand and hoping. That is the
// outcome the refusals in this command exist to prevent, and this one is the last one
// that still led there.
func TestTheFallbackRefusalOverASupplementNamesHowToGetPastIt(t *testing.T) {
	paths := newListPaths(t)
	published := append(rulesOf(200), supplementRules(2)...)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, published)
	before := snapshotWithout(t, paths.dir, paths.controlLock)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: lockedCommit,
			archives: map[string][]byte{
				lockedCommit: chinaSourceArchive(t, lockedCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
	})

	code, _, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--pin-source", rules.RepositoryV2Fly,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitStateUnavailable {
		t.Fatalf("the pin exited %d, want %d (stderr: %s)", code, exitStateUnavailable, stderr)
	}
	for _, want := range []string{paths.sourceLock, "supplement"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not name %q, so it leaves a hand edit as the only "+
				"roadmap: %s", want, stderr)
		}
	}
	// The machine is exactly as it was: a refusal that changed something would be a
	// worse problem than the one it refused.
	assertUnchanged(t, before, paths.dir, paths.controlLock)
}

// A supplement that RESOLVES but converts to nothing ends the install -- and the
// message has to say the install is what failed, and what to roll back to.
//
// This is the one failure the "note, not refusal" contract does not cover. That
// contract is about a second source that could not be REACHED: the pin is the
// documented way out, so it publishes and says what is missing. Here the source was
// reached, downloaded, digest-verified, and then yielded no `full:` or `regexp:` rule
// at all. Publishing anyway would write a lock untrue about how the list was built, so
// refusing is right -- but the error as it stood named an upstream change and nothing
// else, on a first install, with nothing published and no forward road.
func TestAnInstallThatCannotConvertTheSupplementSaysSoAndNamesWhatToRollBackTo(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteSupplementCommit,
			archives: map[string][]byte{
				// Reached and verified, and its China list holds no rule of either
				// kind a supplement contributes: upstream moved them.
				remoteSupplementCommit: sourceArchive(t, remoteSupplementCommit, "domain:only.cn\n"),
			},
		},
	})

	code, stdout, stderr := runCLI(t, origin.pinServices(), "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitStateUnavailable {
		t.Fatalf("the pin exited %d, want %d (stderr: %s)", code, exitStateUnavailable, stderr)
	}
	if stdout != "" {
		t.Errorf("a refused pin wrote a publication report: %q", stdout)
	}
	// Nothing was installed, and the message says so in the two ways an operator needs:
	// that this run published nothing, and what is still running.
	for _, want := range []string{"published nothing", paths.sourceLock} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not say %q, so a first-install operator is left with an "+
				"upstream complaint and no idea that the install is what failed: %s", want, stderr)
		}
	}
	if _, _, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile); err != nil || found {
		t.Errorf("a refused install left a pair behind: found=%t err=%v", found, err)
	}
}

// A policy that cannot be loaded stops the run before anything is fetched. The
// policy is what says whether to refresh, so a run that could not read it has no
// business resolving a commit.
func TestAutomaticRefusesAPolicyItCannotLoad(t *testing.T) {
	paths := newListPaths(t)
	publishPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(200))),
	})
	policy := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policy, []byte("schema_version: 1\nschedule: \"5pm\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", policy,
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitInvalidCLI {
		t.Fatalf("--automatic with an invalid policy exited %d, want %d (stderr: %s)",
			code, exitInvalidCLI, stderr)
	}
	if len(origin.requests) != 0 {
		t.Errorf("a run that could not read the policy still reached the remote: %v", origin.requests)
	}
}

// --automatic is a mode, not a modifier: it cannot be combined with another one.
func TestAutomaticRefusesToBeCombinedWithAnotherMode(t *testing.T) {
	for _, args := range [][]string{
		{"--automatic", "--check"},
		{"--automatic", "--pin-remote", "HEAD"},
		{"--automatic", "--refresh-ranges"},
	} {
		if _, err := parseUpdateListOptions(io.Discard, append(args, "--policy", "/etc/mosdns/policy.yaml")); err == nil {
			t.Errorf("%v was accepted, and two modes at once is two publishers", args)
		}
	}
	if options, err := parseUpdateListOptions(io.Discard, []string{"--automatic"}); err != nil {
		t.Fatalf("--automatic alone was refused: %v", err)
	} else if !options.automatic {
		t.Error("--automatic did not select its mode")
	}
}

func TestUpdateListsRejectsInvalidCommandLinesWithoutTouchingAnything(t *testing.T) {
	// The command takes no repository, no ref other than the literal HEAD, and
	// no output path it was not told. Every one of those mistakes is an exit two
	// with a diagnostic and no effect, including no request to the remote.
	tests := []struct {
		name string
		args []string
	}{
		{name: "no mode", args: []string{}},
		{name: "both modes", args: []string{"--check", "--pin-remote", "HEAD"}},
		// A full commit is NOT in this table: it is accepted, because publishing a
		// named commit is how a pin accepted by mistake is undone. See
		// TestPinRemotePublishesTheNamedCommitRatherThanTheRemoteHead. What stays
		// refused is every REF that is not HEAD, because a ref names a moving target
		// and the lock records a commit.
		{name: "ref instead of HEAD", args: []string{"--pin-remote", "refs/heads/master"}},
		{name: "branch instead of HEAD", args: []string{"--pin-remote", "master"}},
		{name: "tag instead of HEAD", args: []string{"--pin-remote", "v1.0.0"}},
		{name: "empty ref", args: []string{"--pin-remote", ""}},
		{name: "unknown flag", args: []string{"--check", "--fource"}},
		{name: "unknown flag with a value", args: []string{"--check", "--repository", rules.RepositoryV2Fly}},
		{name: "positional argument", args: []string{"--check", "extra"}},
		{name: "positional ref", args: []string{"--pin-remote", "HEAD", "extra"}},
		{name: "missing flag value", args: []string{"--check", "--source-lock"}},
		{name: "empty source lock path", args: []string{"--check", "--source-lock", ""}},
		{name: "empty list path", args: []string{"--check", "--list-file", ""}},
		{name: "empty control lock path", args: []string{"--check", "--control-lock", ""}},
		{name: "empty paths with a pin", args: []string{"--pin-remote", "HEAD", "--source-lock", "", "--list-file", ""}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := newListPaths(t)
			publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
			before := snapshot(t, paths.dir)
			origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)})

			args := append([]string{}, test.args...)
			if !containsPathFlag(args) {
				args = append(args, "--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
			}
			code, stdout, stderr := runCLI(t, origin.services(), args...)
			if code != exitInvalidCLI {
				t.Fatalf("args %v exit = %d, want %d (stderr: %s)", args, code, exitInvalidCLI, stderr)
			}
			if stdout != "" {
				t.Errorf("args %v wrote stdout: %q", args, stdout)
			}
			if stderr == "" {
				t.Errorf("args %v wrote no diagnostic", args)
			}
			if len(origin.requests) != 0 {
				t.Errorf("args %v reached the remote: %v", args, origin.requests)
			}
			assertUnchanged(t, before, paths.dir, "")
		})
	}
}

// containsPathFlag reports whether the arguments already supply an output path,
// so the invalid-command-line cases do not silently get a usable path appended.
func containsPathFlag(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--source-lock", "--list-file", "--control-lock", "--previous-lock", "--repository", "--force":
			return true
		}
	}
	return false
}

func TestUpdateListsHelpIsPrintedRatherThanDiscarded(t *testing.T) {
	// `--help` used to parse, print the usage into io.Discard, and exit two with a
	// diagnostic that said nothing about the flags. An operator who typed it got
	// a line of prose and no way to learn that `--check` also reports the ranges
	// or that `--refresh-ranges` is the mode that publishes them.
	//
	// The usage now goes to the command's own stderr, names every flag, and
	// carries the flag descriptions -- so the manual page Task 6 writes inherits
	// the same text this asserts on, rather than a second copy of it.
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			paths := newListPaths(t)
			origin := newFakeOrigin(t, remoteCommit, nil)
			code, stdout, stderr := runCLI(t, origin.services(), flag,
				"--source-lock", paths.sourceLock, "--list-file", paths.listFile)
			if code != exitInvalidCLI {
				t.Fatalf("`%s` exit = %d, want %d", flag, code, exitInvalidCLI)
			}
			if stdout != "" {
				t.Errorf("`%s` wrote stdout: %q", flag, stdout)
			}
			for _, want := range []string{
				"update-lists",
				"-check",
				"-pin-remote",
				"-refresh-ranges",
				"-ranges-cache",
				"-control-lock",
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("`%s` usage does not mention %s:\n%s", flag, want, stderr)
				}
			}
			if len(origin.requests) != 0 {
				t.Errorf("`%s` reached the remote: %v", flag, origin.requests)
			}
		})
	}
}

func TestTheCheckFlagSaysWhatItNowReports(t *testing.T) {
	// The reviewer's finding: the brief asked the command to "state explicitly
	// what --check now means for the ranges", and the only place that meaning
	// existed was a doc comment and a report. Task 6 owns the manual page, so the
	// flag's own description is where the page inherits it from.
	paths := newListPaths(t)
	origin := newFakeOrigin(t, remoteCommit, nil)
	_, _, usage := runCLI(t, origin.services(), "--help",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile)
	for _, want := range []string{
		"ranges",
		"writes nothing",
		"--refresh-ranges",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("the usage does not say %q about --check:\n%s", want, usage)
		}
	}
}

// The `--check` flag's own description is where the manual page inherits its meaning
// from, so the two-source verdict has to be in it. A flag whose help says "the pinned
// source" while the command judges two of them is a description that is one migration
// out of date.
func TestTheCheckFlagSaysTheVerdictCoversBothSources(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOrigin(t, remoteCommit, nil)
	_, _, usage := runCLI(t, origin.services(), "--help",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile)
	for _, want := range []string{"pinned sources", "both"} {
		if !strings.Contains(usage, want) {
			t.Errorf("the usage does not say %q about the two-source verdict:\n%s", want, usage)
		}
	}
}

// The fallback is named as such only on a machine that has nothing installed. On a
// machine whose lock already carries a supplement, `--pin-source v2fly` is refused --
// the supplement cannot travel beside a primary that already holds every kind it
// contributes -- and the help is where an operator looks to find out why.
func TestThePinSourceFlagCarriesTheFallbacksQualifier(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOrigin(t, remoteCommit, nil)
	_, _, usage := runCLI(t, origin.services(), "--help",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile)
	for _, want := range []string{"nothing installed", "supplement"} {
		if !strings.Contains(usage, want) {
			t.Errorf("the --pin-source help does not say %q:\n%s", want, usage)
		}
	}
}

func TestUpdateListsDefaultsAreTheInstalledPaths(t *testing.T) {
	// The packaged timer runs the command with no paths at all, so the defaults
	// are the only thing that makes it update the list the router reads. A
	// default that pointed anywhere else would leave the router on a list no
	// pin ever replaced.
	options, err := parseUpdateListOptions(io.Discard, []string{"--check"})
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	pinned, err := parseUpdateListOptions(io.Discard, []string{"--pin-remote", "HEAD"})
	if err != nil {
		t.Fatalf("parse pin defaults: %v", err)
	}
	for name, options := range map[string]updateListOptions{"check": options, "pin-remote": pinned} {
		if options.sourceLock != "/var/lib/mosdns/lists/source-lock.json" {
			t.Errorf("%s default source lock = %q, want the installed list directory", name, options.sourceLock)
		}
		if options.listFile != "/var/lib/mosdns/lists/cn-domains.txt" {
			t.Errorf("%s default list file = %q, want the installed list directory", name, options.listFile)
		}
		if options.controlLock != "/var/lib/mosdns/runtime/control.lock" {
			t.Errorf("%s default control lock = %q, want the installed runtime directory", name, options.controlLock)
		}
	}
}

func TestUpdateListsCheckNamesTheInstalledSourceLockPathWhenNothingIsPinned(t *testing.T) {
	// With the default paths and nothing published, the refusal has to name the
	// file that is missing, or an operator cannot tell an un-installed gateway
	// from a broken one. This run reads paths only: it never reaches the remote.
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)})

	code, stdout, stderr := runCLI(t, origin.services(), "--check")
	if code != exitStateUnavailable {
		t.Skipf("the installed list directory exists on this host (exit %d, stderr %q), so the default-path refusal cannot be observed", code, stderr)
	}
	if stdout != "" {
		t.Errorf("refused check wrote a report: %q", stdout)
	}
	if !strings.Contains(stderr, "/var/lib/mosdns/lists/source-lock.json") {
		t.Errorf("refusal does not name the installed source lock: %q", stderr)
	}
	if len(origin.requests) != 0 {
		t.Errorf("refused check reached the remote: %v", origin.requests)
	}
}

func TestProductionListClientUsesTheSystemResolver(t *testing.T) {
	// The gateway's own updates must resolve GitHub the way the host resolves
	// every other foreign name, through MOSDNS. A client with its own transport
	// or its own dialer could quietly point the pin at a resolver of somebody
	// else's choosing, and a client without a timeout could hang a timer run
	// forever.
	client := productionServices().newHTTPClient()
	if client == nil {
		t.Fatal("production services build no HTTP client")
	}
	if client.Transport != nil {
		t.Error("production HTTP client installs its own transport, so its name resolution is not the system one")
	}
	if client.Timeout <= 0 {
		t.Error("production HTTP client has no timeout")
	}
}

func TestUpdateListsContextIsCancelledWhenTheCommandIsGivenACancelledContext(t *testing.T) {
	// The command boundary takes its context, so a caller that stops waiting
	// stops the update instead of leaving a download running.
	paths := newListPaths(t)
	publishPair(t, paths, lockedCommit, []byte("domain:previous.cn\n"))
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	code := runWithContext(ctx, []string{"update-lists", "--check",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile, "--control-lock", paths.controlLock, "--previous-lock", paths.previousLock}, &stdout, &stderr, origin.services())
	if code == exitSuccess {
		t.Fatalf("check reported success for a cancelled update:\n%s", stdout.String())
	}
	if len(origin.requests) != 0 {
		t.Errorf("cancelled check reached the remote: %v", origin.requests)
	}
}

// snapshotTree is snapshotWithout for a subtree, keyed by the path below dir. It
// exists because the fixture directories now contain a systemd subdirectory, and a
// snapshot helper that cannot read one is a helper that reports every "validate
// wrote nothing" case as a crash instead of as an answer.
func snapshotTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	state := make(map[string][]byte)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || path == dir {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		relative, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		state[relative] = contents
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// A pin publishes only the entries the operator left on, and the lock says which.
//
// The switch is CONTENT and the pin is neither content nor time: what changes is
// which files of the primary source's archive become rules, so a pin on a machine
// that turned google off has to record that in `entries` rather than convert all
// three and let the operator wonder why the list still carries the names they
// switched off.
//
// It is a manual pin here rather than an unattended refresh because this is the
// path that read no policy at all until now, and a switch that only the timer
// honours is one an operator who pins by hand believes they have turned off.
func TestAPinPublishesOnlyTheEntriesTheOperatorLeftOn(t *testing.T) {
	paths := newListPaths(t)
	policy := config.Defaults()
	off := false
	policy.Lists.China.Google = &off
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:kept.cn\n"),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: sourceArchive(t, remoteCommit, firstInstallArchive),
			},
		},
	})
	services := origin.services()
	services.loadPolicy = func(string) (config.Policy, error) { return policy, nil }

	code, stdout, stderr := runCLI(t, services, "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--pin-remote exit = %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}

	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if slices.Contains(lock.Primary.Entries, "google.china.conf") {
		t.Fatalf("google is off but the lock records it: %v", lock.Primary.Entries)
	}
	if !slices.Contains(lock.Primary.Entries, rules.ChinaConfBaseEntry) {
		t.Fatalf("the base entry went missing: %v", lock.Primary.Entries)
	}
	if !strings.Contains(string(list), "kept.cn") {
		t.Fatalf("the list does not carry what the remaining entries contributed: %q", list)
	}
	// The switch is a switch on the LIST, not only on the lock: google.china.conf is
	// the only file of the archive that carries google.cn, so a lock that dropped the
	// entry while the list kept the rule would be a machine still routing those names
	// domestically after the operator said otherwise.
	if strings.Contains(string(list), "google.cn") {
		t.Fatalf("google is off but the published list still carries its names: %q", list)
	}
	if !strings.Contains(string(list), "apple.cn") {
		t.Fatalf("apple is on, so the list must still carry what it contributed: %q", list)
	}
}

// A machine that cannot read which entries it wants is not one to guess on.
//
// The guess is the whole danger: an entry set read as "all of them" publishes a
// list that is quietly missing nothing at all, and an entry set read as "none of
// them" publishes one missing a category of domestic names -- every one of which
// quietly takes the foreign branch, with no exit code, no stderr line and nothing
// in the report to notice it. So `--pin-remote`, which used to read no policy at
// all, now refuses when it cannot.
//
// The cost is that a machine with a broken policy.yaml cannot pin by hand, and it
// is paid deliberately: a broken policy makes `--automatic` and the schedule
// untrustworthy too, so a pin that succeeds there would be a false reassurance.
func TestAMachineThatCannotReadItsPolicyCannotPinEither(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {head: remoteCommit, archives: map[string][]byte{
			remoteCommit: chinaSourceArchive(t, remoteCommit, "domain:a.cn\n")}},
	})
	services := origin.services()
	services.loadPolicy = func(string) (config.Policy, error) {
		return config.Policy{}, errors.New("policy.yaml: line 11: found a tab")
	}

	code, _, stderr := runCLI(t, services, "--pin-remote", "HEAD",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitInvalidCLI {
		t.Fatalf("exit = %d, want %d: a machine that cannot read which entries it wants is not "+
			"one to guess on (stderr: %s)", code, exitInvalidCLI, stderr)
	}
	if !strings.Contains(stderr, "policy") {
		t.Fatalf("the refusal does not name what it could not read: %s", stderr)
	}
}

// The unattended path honours the switches too, and reads the policy once.
//
// `--automatic` reads the policy for two different questions -- when to refresh and
// which files to convert -- and those two answers have to come from ONE reading.
// A second read is not merely wasteful: the file could change between them, and the
// run would then have decided to refresh under one policy and converted under
// another, which is precisely the disagreement a single file exists to prevent.
// Counting the reads is what makes the property real rather than intended.
//
// The switch assertions are the same ones the pin case makes, deliberately: the
// mode that refreshes and the mode an operator runs by hand publish through
// different code, and a switch honoured by only one of them is a switch the
// operator turned off and did not get.
func TestAnUnattendedRefreshConvertsOnlyTheEntriesTheOperatorLeftOn(t *testing.T) {
	paths := newListPaths(t)
	publishChinaPairWithSupplement(t, paths, lockedCommit, lockedSupplementCommit, rulesOf(200))
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: supplementArchive(t, remoteCommit, 2),
			},
		},
	})
	off := false
	policy := config.Defaults()
	policy.Lists.China.Automatic = true
	policy.Lists.China.Google = &off
	reads := 0
	services := origin.services()
	services.loadPolicy = func(string) (config.Policy, error) {
		reads++
		return policy, nil
	}

	code, stdout, stderr := runCLI(t, services, "--automatic",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--automatic exit = %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	if reads != 1 {
		t.Errorf("the policy was read %d times in one unattended run, want exactly 1: the same "+
			"file says when to refresh and which files to convert, and two readings could be two "+
			"different policies", reads)
	}

	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if slices.Contains(lock.Primary.Entries, "google.china.conf") {
		t.Fatalf("google is off but the unattended lock records it: %v", lock.Primary.Entries)
	}
	if !slices.Contains(lock.Primary.Entries, rules.ChinaConfBaseEntry) {
		t.Fatalf("the base entry went missing: %v", lock.Primary.Entries)
	}
	if strings.Contains(string(list), "google.cn") {
		t.Fatalf("google is off but the unattended publication still carries its names: %q", list)
	}
	if !strings.Contains(string(list), "apple.cn") {
		t.Fatalf("apple is on, so the unattended publication must still carry what it contributed: %q", list)
	}
	if !strings.Contains(string(list), "domain:host00000.example.cn") {
		t.Fatalf("the unattended publication lost the base file's own contribution: %q", list)
	}
}

// chinaEntriesFrom may remove an entry the operator switched off. It may not remove
// one they left on, and the difference has to be a refusal rather than a shorter
// list, because a shorter list is the one failure nothing downstream reports.
//
// Nothing reaches this from a command today: every resolution of the dnsmasq source
// names the whole reviewed set, and the policy's set is a subset of it by
// construction. This test therefore pins the invariant against a future change that
// breaks it rather than against a defect that exists now -- which is still worth
// doing, because the alternative is an assertion that reads "cannot happen" and
// costs nothing to write and everything to be wrong about.
// Turning a switch off has to take effect even when upstream has not moved.
//
// The unattended refresh used to answer "already at" on the strength of the two
// COMMITS alone, which is the right question for a drifted source and the wrong
// one for a reconfigured machine: the operator changed a switch in a file the run
// reads every night, and nothing about the two commits says so. Upstream commits
// rarely, so the switch would appear to do nothing for weeks while the run reported
// `up-to-date: true` and did no work -- the quietest possible way for a conversion
// parameter to be ignored.
//
// So the entry set is part of what "already at" means. This is the same predicate
// the check reports on, and it is here rather than there because this is the half
// that ACTS: a check that says a machine is out of date while the refresh declines
// to move it is a report and a fix that disagree.
func TestAnUnattendedRefreshConvertsAfterTheOperatorTurnsAnEntryOff(t *testing.T) {
	paths := newListPaths(t)
	origin := newFakeOriginSources(t, map[string]fakeSource{
		rules.RepositoryChina: {
			// Already where this machine is published: nothing upstream has moved.
			head: remoteCommit,
			archives: map[string][]byte{
				remoteCommit: chinaSourceArchive(t, remoteCommit, string(rulesOf(200))),
			},
		},
		rules.RepositoryV2Fly: {
			head: lockedSupplementCommit,
			archives: map[string][]byte{
				lockedSupplementCommit: supplementArchive(t, lockedSupplementCommit, 2),
			},
		},
	})
	// Published with google ON, at the very commit upstream still points at.
	//
	// Two hundred base rules, because the collapse guardrail measures the incoming
	// primary against what this machine published and the shape that matters is the
	// real one: the base file is 110,205 of 110,481 published rules, so switching a
	// whole entry off is a 0.25% move and the guardrail never sees it. A three-rule
	// list would make the switch-off a 33% shrink and the guardrail would refuse it
	// as an upstream collapse -- a shape this project does not publish, so a fixture
	// that used it would be testing arithmetic rather than the run.
	publishChinaPairWithSupplement(t, paths, remoteCommit, lockedSupplementCommit,
		append(append([]byte("domain:apple.cn\ndomain:google.cn\n"), rulesOf(200)...), supplementRules(2)...))

	off := false
	policy := config.Defaults()
	policy.Lists.China.Automatic = true
	policy.Lists.China.Google = &off
	services := origin.services()
	services.loadPolicy = func(string) (config.Policy, error) { return policy, nil }

	code, stdout, stderr := runCLI(t, services, "--automatic",
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--automatic exit = %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	if strings.Contains(stdout, "already at") {
		t.Fatalf("the run did nothing although the entry set changed:\n%s", stdout)
	}

	lock, list, found, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil || !found {
		t.Fatalf("published pair: %v found=%t", err, found)
	}
	if slices.Contains(lock.Primary.Entries, "google.china.conf") {
		t.Fatalf("google is off but the lock still records it: %v", lock.Primary.Entries)
	}
	if strings.Contains(string(list), "google.cn") {
		t.Fatalf("google is off but the published list still carries its names: %q", list)
	}
	if !strings.Contains(string(list), "apple.cn") {
		t.Fatalf("apple is on, so the publication must still carry what it contributed: %q", list)
	}
}

func TestChinaEntriesFromRefusesToDropAnEntryThePolicyNamed(t *testing.T) {
	resolved := rules.ChinaConfEntries
	wanted := []string{rules.ChinaConfBaseEntry, "google.china.conf"}

	got, err := chinaEntriesFrom(resolved, wanted)
	if err != nil {
		t.Fatalf("narrowing %v to %v refused: %v", resolved, wanted, err)
	}
	if !slices.Equal(got, wanted) {
		t.Fatalf("narrowing %v to %v gave %v, want %v", resolved, wanted, got, wanted)
	}

	// The base entry alone is enough to make a refusal, because its absence is the
	// failure this change exists to prevent: 99.75%% of the list, gone in silence.
	if _, err := chinaEntriesFrom([]string{"google.china.conf"}, wanted); err == nil {
		t.Fatal("a resolution carrying none of what the policy names was trimmed instead of refused")
	}
	if _, err := chinaEntriesFrom([]string{rules.ChinaConfBaseEntry, "apple.china.conf"}, wanted); err == nil {
		t.Fatal("a resolution missing one file the policy names was trimmed instead of refused")
	}
}
