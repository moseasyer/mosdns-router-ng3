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
	lock := rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryChina,
			Commit:     commit,
			SHA256:     sha256Hex([]byte("the archive " + commit)),
			Entries:    append([]string(nil), rules.ChinaConfEntries...),
		},
		ListSHA256: sha256Hex(list),
	}
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
	lock := rules.SourceLock{
		SchemaVersion: 2,
		Primary: rules.SourceRef{
			Repository: rules.RepositoryV2Fly,
			Commit:     commit,
			SHA256:     sha256Hex([]byte("the archive " + commit)),
			Entry:      rules.V2FlyEntry,
		},
		ListSHA256: sha256Hex(list),
	}
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

// services points the command at a fake origin and nothing else: the control
// lock, the publication and the conversion are the real ones.
func (o *fakeOrigin) services() services {
	services := productionServices()
	services.newHTTPClient = func() *http.Client { return o.client }
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
	paths := newListPaths(t)
	archive := sourceArchive(t, remoteCommit, firstInstallArchive)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: archive})

	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

func TestUpdateListsPinRemoteReplacesAnInstalledPair(t *testing.T) {
	// The second pin of an installation is the one that has to leave no trace of
	// the old list: a router reading a mixture of two sources would route against
	// rules nobody pinned together.
	paths := newListPaths(t)
	previous := []byte("domain:previous.cn\n")
	publishPair(t, paths, lockedCommit, previous)
	archive := sourceArchive(t, remoteCommit, firstInstallArchive)
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{remoteCommit: archive})

	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

			code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", lockedCommit,
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
	code, _, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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
	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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
	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

	code, _, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
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

	code, stdout, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
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

	code, _, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
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
	_ = before
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
		code, _, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
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
		code, _, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD", "--pin-source", rules.RepositoryV2Fly,
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

	code, _, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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

	code, _, stderr := runCLI(t, origin.services(), "--pin-remote", "HEAD",
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
		rules.RepositoryV2Fly: {head: lockedCommit},
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
	for _, path := range []string{
		origin.commitPath(rules.RepositoryV2Fly),
		origin.archivePath(rules.RepositoryV2Fly, remoteCommit),
	} {
		if origin.fetched(path) {
			t.Errorf("--automatic fetched %q, which belongs to a source this list was not published from", path)
		}
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
	publishPair(t, paths, lockedCommit, rulesOf(200))
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(200))),
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
	publishPair(t, paths, lockedCommit, published)
	// Upstream now publishes one rule where this machine has two hundred.
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, "domain:first.cn\n"),
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
	publishPair(t, paths, lockedCommit, rulesOf(1000))
	// 980 of 1000: a 2% reduction, comfortably clear of the threshold, because a case
	// balanced exactly on a boundary passes for the wrong reason when the boundary
	// moves by one.
	origin := newFakeOrigin(t, remoteCommit, map[string][]byte{
		remoteCommit: sourceArchive(t, remoteCommit, string(rulesOf(980))),
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
	publishPair(t, paths, lockedCommit, published)
	measured := remoteCommit
	collapsed := "3333333333333333333333333333333333333333"
	origin := newFakeOrigin(t, measured, map[string][]byte{
		measured:  sourceArchive(t, measured, string(rulesOf(200))),
		collapsed: sourceArchive(t, collapsed, "domain:only.cn\n"),
	})
	// Upstream answers `measured` for its first three resolutions of HEAD and
	// `collapsed` from the fourth on. Three is the drift report's two plus the
	// refresh's one, so `measured` is what this run measures; the fourth answer only
	// reaches a caller that resolves AGAIN after having decided what to publish.
	//
	// The count is anchored to the code and that is stated rather than hidden. Two
	// earlier anchors were tried and both failed the same way -- they fired during the
	// drift report, so the thing that got measured was already the collapsed commit and
	// both implementations refused. A case whose scenario depends on this number
	// staying 3 says so in a comment; if the report's resolution count changes, this
	// gate fails loudly rather than quietly testing nothing.
	//
	// The flip takes effect on the request that sets it, because the origin answers
	// from the field after the hook runs. Getting that backwards makes the FIRST
	// resolution the collapsed one, which is how the first attempt at this case
	// refused a run that was doing exactly the right thing.
	resolutions := 0
	origin.onServe = func(o *fakeOrigin, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/commits/HEAD") {
			return
		}
		resolutions++
		if resolutions >= 4 {
			o.setHead(rules.RepositoryV2Fly, collapsed)
		}
	}
	code, _, stderr := runCLI(t, origin.services(), "--automatic",
		"--policy", automaticPolicy(t, true, false),
		"--source-lock", paths.sourceLock, "--list-file", paths.listFile,
		"--control-lock", paths.controlLock, "--previous-lock", paths.previousLock)
	if code != exitSuccess {
		t.Fatalf("--automatic exited %d (stderr: %s)", code, stderr)
	}
	lock, list, _, err := rules.ReadPublishedPair(paths.sourceLock, paths.listFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Primary.Commit == collapsed {
		t.Fatalf("published %s, which nothing measured: its list has 1 rule where %d were published",
			collapsed, countListRules(published))
	}
	if lock.Primary.Commit != measured {
		t.Errorf("published %s, want the commit that was measured (%s)", lock.Primary.Commit, measured)
	}
	// And the list that is published is the measured commit's, not the collapsed one.
	if got := countListRules(list); got != countListRules(published) {
		t.Errorf("the published list holds %d rules, want the measured commit's %d",
			got, countListRules(published))
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
		{name: "unknown flag", args: []string{"--check", "--force"}},
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
