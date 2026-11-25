package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/release"
)

const testVersion = "0.2.16"

type entry struct {
	name string
	body string
	typ  byte
	mode int64
	link string
	size int64 // when set, the header claims this size
}

func makeTarball(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// release is one fake bundle: its files, SHA256SUMS, signature and tarball.
type fakeRelease struct {
	files   map[string]string
	sums    []byte
	sig     []byte
	tarball []byte
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
}

func newRelease(t *testing.T, version string) *fakeRelease {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRelease{pub: pub, priv: priv, files: map[string]string{
		"install.sh":                                    "#!/bin/sh\n",
		"bot-agent-v" + version + "-linux-arm64":        "agent arm64",
		"bot-agent-v" + version + "-linux-amd64":        "agent amd64",
		"bot-agent-runner-v" + version + "-linux-arm64": "runner arm64",
		"units/bot-agent.service":                       "[Unit]\n",
		"units/bot-agent-runner.socket":                 "[Socket]\n",
		"units/bot-agent-runner@.service":               "[Service]\n",
		"units/tmpfiles-bot-agent-runner.conf":          "d /run/bot-agent 0700 root root -\n",
	}}
	r.build(t, version)
	return r
}

func (r *fakeRelease) build(t *testing.T, version string) {
	t.Helper()
	names := make([]string, 0, len(r.files))
	for n := range r.files {
		names = append(names, n)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, n := range names {
		fmt.Fprintf(&sums, "%s  %s\n", sum(r.files[n]), n)
	}
	r.sums = []byte(sums.String())
	r.sig = release.EncodeSig(ed25519.Sign(r.priv, r.sums))
	top := "bot-agent-v" + version
	entries := []entry{{name: top + "/", typ: tar.TypeDir, mode: 0o755}, {name: top + "/units/", typ: tar.TypeDir, mode: 0o755}}
	for _, n := range names {
		mode := int64(0o644)
		if !strings.HasPrefix(n, "units/") {
			mode = 0o755
		}
		entries = append(entries, entry{name: top + "/" + n, body: r.files[n], mode: mode})
	}
	entries = append(entries, entry{name: top + "/SHA256SUMS", body: string(r.sums)})
	r.tarball = makeTarball(t, entries)
}

type mapFetcher struct {
	files map[string][]byte
	err   error
	calls []string
}

func (m *mapFetcher) Fetch(_ context.Context, version, name string) ([]byte, error) {
	m.calls = append(m.calls, name)
	if m.err != nil {
		return nil, m.err
	}
	b, ok := m.files[name]
	if !ok {
		return nil, errors.New("404")
	}
	return b, nil
}

type runRig struct {
	env        Env
	installs   [][]string
	installRC  int
	installOut string
	version    string
}

func newRunRig(t *testing.T, r *fakeRelease, version string) *runRig {
	t.Helper()
	rg := &runRig{version: version}
	rg.env = Env{
		StateDir: filepath.Join(t.TempDir(), "upgrade"),
		Fetch: &mapFetcher{files: map[string][]byte{
			"SHA256SUMS": r.sums, "SHA256SUMS.sig": r.sig, "bot-agent-v" + version + ".tar.gz": r.tarball,
		}},
		PublicKeys:      []ed25519.PublicKey{r.pub},
		Arch:            "arm64",
		RunnerInstalled: func() bool { return true },
		Version: func(_ context.Context, bin string) (string, error) {
			if filepath.Base(bin) != "bot-agent-v"+version+"-linux-arm64" {
				return "", errors.New("wrong binary")
			}
			return rg.version, nil
		},
		Install: func(_ context.Context, dir string, args []string) (int, string, error) {
			rg.installs = append(rg.installs, append([]string{dir}, args...))
			return rg.installRC, rg.installOut, nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC) },
	}
	return rg
}

func (rg *runRig) run(t *testing.T) agentupgrade.Result {
	t.Helper()
	res, err := Run(context.Background(), testVersion, rg.env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(rg.env.StateDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := agentupgrade.DecodeResult(bytes.TrimSpace(b))
	if !ok || got != res {
		t.Fatalf("result.json %s vs %+v", b, res)
	}
	fi, err := os.Stat(filepath.Join(rg.env.StateDir, "result.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("result.json mode %v", fi.Mode())
	}
	return res
}

func want(t *testing.T, res agentupgrade.Result, outcome, reason string) {
	t.Helper()
	if res.Outcome != outcome || res.Reason != reason || res.Version != testVersion || res.At != "2026-11-23T10:00:00Z" {
		t.Fatalf("got %+v, want %s %s", res, outcome, reason)
	}
}

func TestRunSucceeded(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	// An older staged version is removed; the current one stays.
	old := filepath.Join(rg.env.StateDir, "0.2.15")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	res := rg.run(t)
	want(t, res, "succeeded", "")
	bundle := filepath.Join(rg.env.StateDir, testVersion, "bot-agent-v"+testVersion)
	if len(rg.installs) != 1 || rg.installs[0][0] != bundle || strings.Join(rg.installs[0][1:], " ") != "upgrade --runner" {
		t.Fatalf("install %q", rg.installs)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old staged version kept")
	}
	fi, err := os.Stat(filepath.Join(rg.env.StateDir, testVersion))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("stage mode %v %v", fi, err)
	}
	fi, err = os.Stat(rg.env.StateDir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v %v", fi, err)
	}
}

func TestRunWithoutRunnerFlag(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	rg.env.RunnerInstalled = func() bool { return false }
	want(t, rg.run(t), "succeeded", "")
	if strings.Join(rg.installs[0][1:], " ") != "upgrade" {
		t.Fatalf("install %q", rg.installs)
	}
}

func TestRunDownloadFailed(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	rg.env.Fetch = &mapFetcher{err: errors.New("offline")}
	want(t, rg.run(t), "failed", "download_failed")
	if len(rg.installs) != 0 {
		t.Fatal("installed")
	}
}

func TestRunSignatureInvalid(t *testing.T) {
	cases := map[string]func(*fakeRelease, *runRig){
		"bad signature": func(r *fakeRelease, rg *runRig) {
			sig := ed25519.Sign(r.priv, append([]byte("x"), r.sums...))
			rg.env.Fetch.(*mapFetcher).files["SHA256SUMS.sig"] = release.EncodeSig(sig)
		},
		"wrong key": func(r *fakeRelease, rg *runRig) {
			other, _, _ := ed25519.GenerateKey(rand.Reader)
			rg.env.PublicKeys = []ed25519.PublicKey{other}
		},
		"placeholder key": func(r *fakeRelease, rg *runRig) {
			rg.env.PublicKeys = []ed25519.PublicKey{ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))}
		},
		"placeholder next to the signing key": func(r *fakeRelease, rg *runRig) {
			rg.env.PublicKeys = append(rg.env.PublicKeys, ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
		},
		"empty key list": func(r *fakeRelease, rg *runRig) {
			rg.env.PublicKeys = []ed25519.PublicKey{}
		},
		"changed SHA256SUMS": func(r *fakeRelease, rg *runRig) {
			rg.env.Fetch.(*mapFetcher).files["SHA256SUMS"] = append([]byte(strings.Repeat("0", 64)+"  extra\n"), r.sums...)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, testVersion)
			rg := newRunRig(t, r, testVersion)
			mutate(r, rg)
			want(t, rg.run(t), "failed", "signature_invalid")
			if len(rg.installs) != 0 {
				t.Fatal("installed")
			}
			if _, err := os.Stat(filepath.Join(rg.env.StateDir, testVersion)); !os.IsNotExist(err) {
				t.Fatal("unpacked before the signature check")
			}
		})
	}
}

// v1.27: a bundle signed by any compiled key is accepted, so a release
// signed by the CI key installs on a helper that also lists the offline key.
func TestRunSignedBySecondKey(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	rg.env.PublicKeys = []ed25519.PublicKey{other, r.pub}
	want(t, rg.run(t), "succeeded", "")
}

// A hash mismatch is refused: the signed SHA256SUMS is right, the bundled
// file is not.
func TestRunFileHashMismatch(t *testing.T) {
	r := newRelease(t, testVersion)
	signedSums, signedSig := r.sums, r.sig
	r.files["install.sh"] = "#!/bin/sh\nrm -rf /\n"
	r.build(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	f := rg.env.Fetch.(*mapFetcher)
	f.files["SHA256SUMS"] = signedSums
	f.files["SHA256SUMS.sig"] = signedSig
	// The tarball's own SHA256SUMS lists the new hash, so it differs from
	// the signed one too; replace it with the signed bytes to test the
	// per-file check on its own.
	top := "bot-agent-v" + testVersion
	entries := []entry{{name: top + "/", typ: tar.TypeDir}}
	for n, body := range r.files {
		entries = append(entries, entry{name: top + "/" + n, body: body, mode: 0o755})
	}
	entries = append(entries, entry{name: top + "/SHA256SUMS", body: string(signedSums)})
	f.files["bot-agent-v"+testVersion+".tar.gz"] = makeTarball(t, entries)
	want(t, rg.run(t), "failed", "bundle_invalid")
	if len(rg.installs) != 0 {
		t.Fatal("installed")
	}
}

func TestRunBundledSumsDiffer(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	top := "bot-agent-v" + testVersion
	entries := []entry{{name: top + "/", typ: tar.TypeDir}}
	for n, body := range r.files {
		entries = append(entries, entry{name: top + "/" + n, body: body, mode: 0o755})
	}
	entries = append(entries, entry{name: top + "/SHA256SUMS", body: string(r.sums) + "\n"})
	rg.env.Fetch.(*mapFetcher).files["bot-agent-v"+testVersion+".tar.gz"] = makeTarball(t, entries)
	want(t, rg.run(t), "failed", "bundle_invalid")
}

func TestRunVersionMismatch(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	rg.version = "0.2.15"
	want(t, rg.run(t), "failed", "version_mismatch")
	if len(rg.installs) != 0 {
		t.Fatal("installed")
	}
	rg = newRunRig(t, r, testVersion)
	rg.env.Version = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
	want(t, rg.run(t), "failed", "version_mismatch")
}

func TestRunNoAgentForArch(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	rg.env.Arch = "riscv64"
	want(t, rg.run(t), "failed", "bundle_invalid")
}

func TestRunInstallOutcomes(t *testing.T) {
	cases := []struct {
		rc      int
		out     string
		outcome string
		reason  string
	}{
		{1, "verify: no hello after restart\nverify failed, rolling back\nupgrade failed, restored 0.2.15\n", "rolled_back", "not_reconnected"},
		{1, "verify: agent is failed\nverify failed, rolling back\nupgrade failed, restored 0.2.15\n", "rolled_back", "install_failed"},
		{1, "verify: no hello after restart\nverify failed, rolling back\nverify: NRestarts is not 0\nrollback verify failed\n", "failed", "install_failed"},
		{1, "verify: no hello after restart\nverify failed, rolling back\nverify: agent is failed\nrollback verify failed\n", "failed", "install_failed"},
		{1, "rollback failed\n", "failed", "install_failed"},
		{1, "sha256 mismatch: install.sh\nnothing changed\n", "failed", "install_failed"},
		{-1, "", "failed", "install_failed"},
	}
	for _, c := range cases {
		r := newRelease(t, testVersion)
		rg := newRunRig(t, r, testVersion)
		rg.installRC, rg.installOut = c.rc, c.out
		want(t, rg.run(t), c.outcome, c.reason)
	}
}

func TestRunBadVersion(t *testing.T) {
	if _, err := Run(context.Background(), "0.2.16;id", Env{StateDir: t.TempDir()}); err == nil {
		t.Fatal("accepted a bad version")
	}
}

// Unpack refusals (item 6.3).
func TestUnpackRefusals(t *testing.T) {
	top := "bot-agent-v" + testVersion
	cases := map[string][]entry{
		"dotdot":         {{name: top + "/", typ: tar.TypeDir}, {name: top + "/../evil", body: "x"}},
		"dotdot inside":  {{name: top + "/a/../../evil", body: "x"}},
		"absolute":       {{name: "/etc/cron.d/evil", body: "x"}},
		"symlink":        {{name: top + "/", typ: tar.TypeDir}, {name: top + "/link", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hardlink":       {{name: top + "/", typ: tar.TypeDir}, {name: top + "/link", typ: tar.TypeLink, link: top + "/install.sh"}},
		"two top dirs":   {{name: top + "/install.sh", body: "x"}, {name: "other/install.sh", body: "x"}},
		"wrong top":      {{name: "bot-agent-v0.2.17/install.sh", body: "x"}},
		"fifo":           {{name: top + "/fifo", typ: tar.TypeFifo}},
		"char device":    {{name: top + "/dev", typ: tar.TypeChar}},
		"dot segment":    {{name: top + "/./install.sh", body: "x"}},
		"duplicate file": {{name: top + "/a", body: "x"}, {name: top + "/a", body: "y"}},
		"no top dir":     {},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "stage")
			if err := Unpack(makeTarball(t, entries), dest, top); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("partial tree left")
			}
			if _, err := os.Stat(filepath.Join(dir, "evil")); !os.IsNotExist(err) {
				t.Fatal("wrote outside dest")
			}
		})
	}
	if err := Unpack([]byte("not a tarball"), filepath.Join(t.TempDir(), "s"), top); err == nil {
		t.Fatal("accepted non-gzip")
	}
}

func TestUnpackSizeCap(t *testing.T) {
	top := "bot-agent-v" + testVersion
	// Headers that claim more than MaxUnpacked in total are refused before
	// the bytes are written.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/big", Typeflag: tar.TypeReg, Mode: 0o644, Size: MaxUnpacked + 1}); err != nil {
		t.Fatal(err)
	}
	// Leave the body short; Unpack must refuse on the header alone.
	_ = gz.Close()
	if err := Unpack(buf.Bytes(), filepath.Join(t.TempDir(), "s"), top); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("size cap: %v", err)
	}
}

func TestUnpackModes(t *testing.T) {
	top := "bot-agent-v" + testVersion
	dest := filepath.Join(t.TempDir(), "s")
	tb := makeTarball(t, []entry{
		{name: top + "/install.sh", body: "#!/bin/sh\n", mode: 0o4755},
		{name: top + "/units/a.service", body: "x", mode: 0o666},
	})
	if err := Unpack(tb, dest, top); err != nil {
		t.Fatal(err)
	}
	for rel, mode := range map[string]os.FileMode{
		"install.sh": 0o700, "units/a.service": 0o600, "units": 0o700, ".": 0o700,
	} {
		fi, err := os.Stat(filepath.Join(dest, top, rel))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode || fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Fatalf("%s mode %v", rel, fi.Mode())
		}
	}
}

// Download: the URL is fixed, only listed hosts are reached (also after a
// redirect), and the size and time limits hold.
func TestFileURL(t *testing.T) {
	got, err := FileURL(BaseURL, "0.2.16", "bot-agent-v0.2.16.tar.gz")
	if err != nil || got != "https://github.com/more-than-code/bot-agent/releases/download/v0.2.16/bot-agent-v0.2.16.tar.gz" {
		t.Fatalf("%q %v", got, err)
	}
	for _, n := range []string{"SHA256SUMS", "SHA256SUMS.sig"} {
		if got, err := FileURL(BaseURL, "0.2.16", n); err != nil || got != BaseURL+"v0.2.16/"+n {
			t.Fatalf("%s: %q %v", n, got, err)
		}
	}
	for _, bad := range [][2]string{
		{"0.2.16/../../x", "SHA256SUMS"}, {"0.2.16", "../x"}, {"0.2.16", "bot-agent-v0.2.17.tar.gz"}, {"0.2.16", "install.sh"},
	} {
		if _, err := FileURL(BaseURL, bad[0], bad[1]); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if BaseURL != "https://github.com/more-than-code/bot-agent/releases/download/" {
		t.Fatal("base URL changed")
	}
}

func testFetcher(srv *httptest.Server, hosts ...string) *HTTPFetcher {
	return &HTTPFetcher{
		Base:    srv.URL + "/releases/download/",
		Hosts:   append([]string{strings.TrimPrefix(srv.URL, "https://")}, hosts...),
		Client:  srv.Client(),
		MaxSize: 1024,
		Timeout: 2 * time.Second,
	}
}

func TestFetchOK(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/download/v0.2.16/SHA256SUMS" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "sums")
	}))
	defer srv.Close()
	b, err := testFetcher(srv).Fetch(context.Background(), "0.2.16", "SHA256SUMS")
	if err != nil || string(b) != "sums" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := testFetcher(srv).Fetch(context.Background(), "0.2.16", "SHA256SUMS.sig"); err == nil {
		t.Fatal("404 accepted")
	}
}

func TestFetchRedirects(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "evil")
	}))
	defer other.Close()
	hops := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/elsewhere"):
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/plain"):
			http.Redirect(w, r, "http://"+strings.TrimPrefix(other.URL, "https://")+"/x", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/hop/"):
			n := 0
			fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/hop/"), "%d", &n)
			hops++
			if n == 0 {
				io.WriteString(w, "done")
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	redirect := func(target string) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, srv.URL+target, http.StatusFound)
		}))
	}
	// A redirect to a host that is not listed is refused.
	front := redirect("/elsewhere")
	defer front.Close()
	f := testFetcher(front, strings.TrimPrefix(srv.URL, "https://"))
	f.Client = srv.Client()
	if _, err := f.Fetch(context.Background(), "0.2.16", "SHA256SUMS"); err == nil {
		t.Fatal("followed a redirect to an unlisted host")
	}
	// A redirect to plain http is refused even for a listed host.
	plain := redirect("/plain")
	defer plain.Close()
	f = testFetcher(plain, strings.TrimPrefix(srv.URL, "https://"), strings.TrimPrefix(other.URL, "https://"))
	f.Client = srv.Client()
	if _, err := f.Fetch(context.Background(), "0.2.16", "SHA256SUMS"); err == nil {
		t.Fatal("followed a redirect to http")
	}
	// Five redirects are followed (the front one plus four hops), six are not.
	for _, tc := range []struct {
		hops int
		ok   bool
	}{{4, true}, {5, false}} {
		front := redirect(fmt.Sprintf("/hop/%d", tc.hops))
		f := testFetcher(front, strings.TrimPrefix(srv.URL, "https://"))
		f.Client = srv.Client()
		b, err := f.Fetch(context.Background(), "0.2.16", "SHA256SUMS")
		front.Close()
		if tc.ok && (err != nil || string(b) != "done") {
			t.Fatalf("%d hops: %q %v", tc.hops, b, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%d hops followed", tc.hops)
		}
	}
}

func TestFetchHostNotListed(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") }))
	defer srv.Close()
	f := testFetcher(srv)
	f.Hosts = AllowedHosts
	if _, err := f.Fetch(context.Background(), "0.2.16", "SHA256SUMS"); err == nil {
		t.Fatal("fetched from an unlisted host")
	}
	f = testFetcher(srv)
	f.Base = strings.Replace(f.Base, "https://", "http://", 1)
	if _, err := f.Fetch(context.Background(), "0.2.16", "SHA256SUMS"); err == nil {
		t.Fatal("fetched over http")
	}
}

func TestFetchSizeCap(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			// No Content-Length: the cap is enforced while reading.
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, strings.Repeat("x", 1025))
	}))
	defer srv.Close()
	for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sig"} {
		if _, err := testFetcher(srv).Fetch(context.Background(), "0.2.16", name); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestFetchTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	f := testFetcher(srv)
	f.Timeout = 100 * time.Millisecond
	start := time.Now()
	if _, err := f.Fetch(context.Background(), "0.2.16", "SHA256SUMS"); err == nil {
		t.Fatal("no timeout")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not applied")
	}
}

func TestProductionLimits(t *testing.T) {
	f := NewHTTPFetcher(nil)
	if f.Base != BaseURL || f.MaxSize != 64<<20 || f.Timeout != 120*time.Second || MaxRedirects != 5 {
		t.Fatalf("%+v", f)
	}
	if strings.Join(AllowedHosts, ",") != "github.com,objects.githubusercontent.com,release-assets.githubusercontent.com" {
		t.Fatal("allowed hosts changed")
	}
}
