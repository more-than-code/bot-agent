package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
)

// L2: a regular file that SHA256SUMS does not list is refused.
func TestRunUnlistedExtraFile(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	top := "bot-agent-v" + testVersion
	entries := []entry{{name: top + "/", typ: tar.TypeDir}}
	for n, body := range r.files {
		entries = append(entries, entry{name: top + "/" + n, body: body, mode: 0o755})
	}
	entries = append(entries,
		entry{name: top + "/SHA256SUMS", body: string(r.sums)},
		entry{name: top + "/units/evil.service", body: "[Service]\nExecStart=/bin/sh\n"})
	rg.env.Fetch.(*mapFetcher).files["bot-agent-v"+testVersion+".tar.gz"] = makeTarball(t, entries)
	want(t, rg.run(t), "failed", "bundle_invalid")
	if len(rg.installs) != 0 {
		t.Fatal("installed")
	}
}

// L2: install.sh and the four units must be listed.
func TestRunRequiredFilesListed(t *testing.T) {
	for _, missing := range requiredFiles {
		t.Run(missing, func(t *testing.T) {
			r := newRelease(t, testVersion)
			delete(r.files, missing)
			r.build(t, testVersion)
			rg := newRunRig(t, r, testVersion)
			want(t, rg.run(t), "failed", "bundle_invalid")
			if len(rg.installs) != 0 {
				t.Fatal("installed")
			}
		})
	}
}

// L3: SHA256SUMS and the signature come first; the tarball is fetched only
// after the signature verified.
func TestRunFetchOrder(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	want(t, rg.run(t), "succeeded", "")
	f := rg.env.Fetch.(*mapFetcher)
	if strings.Join(f.calls, ",") != "SHA256SUMS,SHA256SUMS.sig,bot-agent-v"+testVersion+".tar.gz" {
		t.Fatalf("order %v", f.calls)
	}
	rg = newRunRig(t, r, testVersion)
	rg.env.PublicKeys = []ed25519.PublicKey{make([]byte, 32)}
	want(t, rg.run(t), "failed", "signature_invalid")
	f = rg.env.Fetch.(*mapFetcher)
	if strings.Join(f.calls, ",") != "SHA256SUMS,SHA256SUMS.sig" {
		t.Fatalf("tarball fetched before the signature check: %v", f.calls)
	}
}

func smallLimits() limits {
	return limits{total: 1 << 20, stream: 2 << 20, entries: 10}
}

// L3: more entries than the cap.
func TestUnpackTooManyEntries(t *testing.T) {
	top := "bot-agent-v" + testVersion
	var entries []entry
	for i := 0; i < 11; i++ {
		entries = append(entries, entry{name: top + "/f" + strings.Repeat("x", i), body: "x"})
	}
	if err := unpack(makeTarball(t, entries), filepath.Join(t.TempDir(), "s"), top, smallLimits()); err == nil || !strings.Contains(err.Error(), "too many entries") {
		t.Fatalf("%v", err)
	}
}

// L3: PAX headers are not file sizes; the stream cap stops a flood of them.
func TestUnpackPAXFlood(t *testing.T) {
	top := "bot-agent-v" + testVersion
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i < 5; i++ {
		hdr := &tar.Header{
			Name: top + "/d" + strings.Repeat("y", i) + "/", Typeflag: tar.TypeDir, Mode: 0o755,
			PAXRecords: map[string]string{"comment": strings.Repeat("p", 900<<10)},
			Format:     tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	err := unpack(buf.Bytes(), filepath.Join(t.TempDir(), "s"), top, smallLimits())
	if err == nil || !strings.Contains(err.Error(), "stream") {
		t.Fatalf("%v", err)
	}
}

// L3: a small gzip that inflates past the stream cap is refused.
func TestUnpackDecompressionBomb(t *testing.T) {
	top := "bot-agent-v" + testVersion
	lim := limits{total: 8 << 20, stream: 2 << 20, entries: 10}
	tb := makeTarball(t, []entry{{name: top + "/zeros", body: string(make([]byte, 4<<20))}})
	if len(tb) > 64<<10 {
		t.Fatalf("bomb is %d bytes", len(tb))
	}
	dest := filepath.Join(t.TempDir(), "s")
	if err := unpack(tb, dest, top, lim); err == nil {
		t.Fatal("accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("partial tree left")
	}
}

// The stream cap allows exactly its size.
func TestStreamCapExact(t *testing.T) {
	c := &streamCap{r: strings.NewReader("abcd"), n: 4}
	var out bytes.Buffer
	if _, err := out.ReadFrom(c); err != nil || out.String() != "abcd" {
		t.Fatalf("%q %v", out.String(), err)
	}
	c = &streamCap{r: strings.NewReader("abcde"), n: 4}
	out.Reset()
	if _, err := out.ReadFrom(c); err != errStream {
		t.Fatalf("%v", err)
	}
}

// M1: a cancelled run sends install.sh SIGTERM, its trap runs, and the
// result is failed install_failed.
func TestRunInstallSIGTERMRunsTrap(t *testing.T) {
	old := installGrace
	installGrace = 10 * time.Second
	defer func() { installGrace = old }()
	dir := t.TempDir()
	script := "trap 'echo trapped > trapped; echo \"upgrade failed, restored 0.2.15\" >&2; exit 1' TERM\n" +
		": > started\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	code, out, _ := RunInstall(ctx, dir, []string{"upgrade"})
	if time.Since(start) > 8*time.Second {
		t.Fatal("waited for the kill, not the trap")
	}
	b, err := os.ReadFile(filepath.Join(dir, "trapped"))
	if err != nil || strings.TrimSpace(string(b)) != "trapped" {
		t.Fatalf("trap did not run: %v (code %d out %q)", err, code, out)
	}
	if !strings.Contains(out, "restored") {
		t.Fatalf("out %q", out)
	}

	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	rctx, rcancel := context.WithCancel(context.Background())
	rg.env.Install = func(ctx context.Context, dir string, args []string) (int, string, error) {
		rcancel()
		<-ctx.Done()
		return 1, "verify failed, rolling back\nupgrade failed, restored 0.2.15\n", nil
	}
	res, err := Run(rctx, testVersion, rg.env)
	if err != nil {
		t.Fatal(err)
	}
	want(t, res, "failed", "install_failed")
	got, ok := agentupgrade.DecodeResult(bytes.TrimSpace(mustRead(t, filepath.Join(rg.env.StateDir, "result.json"))))
	if !ok || got != res {
		t.Fatalf("result.json %+v", got)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A second upgrade-run while the lock is held stops without a result.
func TestRunLockHeld(t *testing.T) {
	r := newRelease(t, testVersion)
	rg := newRunRig(t, r, testVersion)
	lock := filepath.Join(t.TempDir(), "run", "bot-agent", "upgrade.lock")
	rg.env.LockPath = lock
	unlock, err := holdLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), testVersion, rg.env); err == nil {
		t.Fatal("ran beside another upgrade")
	}
	if _, err := os.Stat(filepath.Join(rg.env.StateDir, "result.json")); !os.IsNotExist(err) {
		t.Fatal("wrote a result")
	}
	unlock()
	want(t, rg.run(t), "succeeded", "")
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal("lock kept after the run")
	}
}

// L7: the production allowlist refuses userinfo and other ports.
func TestAllowedHostRules(t *testing.T) {
	f := NewHTTPFetcher(nil)
	for raw, ok := range map[string]bool{
		"https://github.com/x":                           true,
		"https://github.com:443/x":                       true,
		"https://release-assets.githubusercontent.com/x": true,
		"https://objects.githubusercontent.com/x":        true,
		"https://user@github.com/x":                      false,
		"https://user:pw@github.com/x":                   false,
		"https://github.com:8443/x":                      false,
		"http://github.com/x":                            false,
		"https://github.com.evil.example/x":              false,
		"https://api.github.com/x":                       false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.allowed(u) == nil; got != ok {
			t.Errorf("%s: allowed=%v", raw, got)
		}
	}
}

// L1: the proxy comes only from the runner.toml value.
func TestProxyFunc(t *testing.T) {
	fn, err := ProxyFunc("")
	if err != nil || fn != nil {
		t.Fatal("empty proxy")
	}
	fn, err = ProxyFunc("http://proxy.lan:3128")
	if err != nil || fn == nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://github.com/x", nil)
	u, err := fn(req)
	if err != nil || u.String() != "http://proxy.lan:3128" {
		t.Fatalf("%v %v", u, err)
	}
	for _, bad := range []string{"http://u:p@proxy:3128", "proxy:3128", "http://proxy:3128/x"} {
		if _, err := ProxyFunc(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if NewHTTPFetcher(nil).Client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("environment proxy used")
	}
}
