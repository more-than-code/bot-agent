package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const otherDigest = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// TestImageDeployRefusals feeds the helper bytes a bad slot and a full cap
// produce. outcomeFor already maps those codes for image_start; this is the
// deploy and rollback path, where a transport error is reported first.
func TestImageDeployRefusals(t *testing.T) {
	kinds := []struct {
		kind string
		spec []byte
	}{
		{"image_deploy", serviceSpec(false, 5, 3031)},
		{"image_rollback", serviceRollbackSpec()},
	}
	for _, kind := range kinds {
		for _, code := range []string{"bad_spec", "policy_denied"} {
			t.Run(kind.kind+"/"+code, func(t *testing.T) {
				a, h, _, _ := imageRun(t)
				h.fn = refuseDeploy(code)
				// execute zeroes the task spec. Each case needs its own copy.
				spec := append([]byte(nil), kind.spec...)
				res := mustImage(t, a, kind.kind, spec, 120)
				m := assertDeploy(t, res, "forbidden_spec", "failed")
				if res.ErrorCode == nil || *res.ErrorCode != "forbidden_spec" || m["outcome"] != "forbidden_spec" {
					t.Fatalf("%+v %v", res, m["outcome"])
				}
				want := []string{images.VerbPull, images.VerbStatus, images.VerbDeploy, images.VerbLogs}
				if kind.kind == "image_rollback" {
					want = append([]string{images.VerbStatus}, want...)
				}
				verbOrder(t, h, want...)
			})
		}
	}

	// docker_unavailable is the bare failed outcomeFor produces. A slot or
	// cap refusal must not arrive as this code.
	t.Run("docker_unavailable stays failed", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		h.fn = refuseDeploy("docker_unavailable")
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		assertDeploy(t, res, "failed", "failed")
		if res.ErrorCode == nil || *res.ErrorCode != "failed" {
			t.Fatalf("%+v", res)
		}
	})
}

func refuseDeploy(code string) func(string, []byte) ([]byte, error) {
	return func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case images.VerbPull:
			return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "sha256:image"}), nil
		case images.VerbStatus:
			return images.Encode(images.StatusResult{Status: missingStatus()}), nil
		case images.VerbDeploy:
			return []byte(`{"error":"` + code + `"}`), nil
		case images.VerbLogs:
			return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
		default:
			return nil, errors.New(verb)
		}
	}
}

func TestServiceRollbackSwap(t *testing.T) {
	match := &images.PrevView{Digest: protocolStr(imgDigest), SpecHash: imgHash}

	t.Run("kept digest", func(t *testing.T) {
		a, h, clock, _ := imageRun(t)
		h.fn = (&swapHelper{prev: match, after: runningStatus(0)}).call
		res := mustImage(t, a, "image_rollback", serviceRollbackSpec(), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != imgDigest || clock.sleeps() != 3 {
			t.Fatalf("digest %s sleeps %d", dig(m), clock.sleeps())
		}
		verbOrder(t, h, images.VerbStatus, images.VerbRollback,
			images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbLogs)
	})

	t.Run("unhealthy", func(t *testing.T) {
		a, h, clock, _ := imageRun(t)
		h.fn = (&swapHelper{prev: match, after: exitedStatus(1)}).call
		res := mustImage(t, a, "image_rollback", serviceRollbackSpec(), 120)
		m := assertDeploy(t, res, "failed", "failed")
		if dig(m) != imgDigest || clock.sleeps() != 0 {
			t.Fatalf("%v sleeps %d", m, clock.sleeps())
		}
		verbOrder(t, h, images.VerbStatus, images.VerbRollback, images.VerbStatus, images.VerbLogs)
	})

	t.Run("other digest", func(t *testing.T) {
		a, h, clock, _ := imageRun(t)
		s := &swapHelper{prev: &images.PrevView{Digest: protocolStr(otherDigest), SpecHash: imgHash}, after: runningStatus(0)}
		h.fn = s.call
		res := mustImage(t, a, "image_rollback", serviceRollbackSpec(), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != imgDigest || s.pullRef != "ghcr.io/acme/web@"+imgDigest || clock.sleeps() != 3 {
			t.Fatalf("ref %s %v", s.pullRef, m)
		}
		if strings.Contains(strings.Join(h.Calls(), " "), images.VerbRollback) {
			t.Fatalf("swapped %v", h.Calls())
		}
		if h.Calls()[0] != images.VerbStatus || h.Calls()[1] != images.VerbPull {
			t.Fatalf("%v", h.Calls())
		}
	})

	t.Run("job stays on pull", func(t *testing.T) {
		a, h, clock, _ := imageRun(t)
		s := &swapHelper{prev: match, after: runningStatus(0)}
		h.fn = s.call
		res := mustImage(t, a, "image_rollback", jobRollbackSpec(), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if s.pullRef != "ghcr.io/acme/web@"+imgDigest || clock.sleeps() != 15 {
			t.Fatalf("ref %s sleeps %d", s.pullRef, clock.sleeps())
		}
		if strings.Contains(strings.Join(h.Calls(), " "), images.VerbRollback) {
			t.Fatalf("job swapped %v", h.Calls())
		}
	})

	t.Run("force still swaps", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		h.fn = (&swapHelper{prev: match, after: runningStatus(0)}).call
		spec := bytes.Replace(serviceRollbackSpec(), []byte(`"force":false`), []byte(`"force":true`), 1)
		res := mustImage(t, a, "image_rollback", spec, 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != imgDigest {
			t.Fatal(dig(m))
		}
		verbOrder(t, h, images.VerbStatus, images.VerbRollback,
			images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbLogs)
	})

	t.Run("null prev digest pulls", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		s := &swapHelper{prev: &images.PrevView{SpecHash: imgHash}, after: runningStatus(0)}
		h.fn = s.call
		res := mustImage(t, a, "image_rollback", serviceRollbackSpec(), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if s.pullRef != "ghcr.io/acme/web@"+imgDigest {
			t.Fatalf("ref %s calls %v", s.pullRef, h.Calls())
		}
		if strings.Contains(strings.Join(h.Calls(), " "), images.VerbRollback) {
			t.Fatal(h.Calls())
		}
	})

	t.Run("status refusal", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		h.fn = (&swapHelper{statusErr: "bad_spec"}).call
		res := mustImage(t, a, "image_rollback", serviceRollbackSpec(), 120)
		m := assertDeploy(t, res, "forbidden_spec", "failed")
		if m["digest"] != nil {
			t.Fatal(m)
		}
		verbOrder(t, h, images.VerbStatus, images.VerbLogs)
	})

	t.Run("status transport", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		h.fn = func(verb string, body []byte) ([]byte, error) {
			if verb == images.VerbLogs {
				return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
			}
			if verb == images.VerbStatus {
				return nil, errors.New("down")
			}
			return nil, errors.New(verb)
		}
		res := mustImage(t, a, "image_rollback", serviceRollbackSpec(), 120)
		m := assertDeploy(t, res, "failed", "failed")
		if m["digest"] != nil {
			t.Fatal(m)
		}
		verbOrder(t, h, images.VerbStatus, images.VerbLogs)
	})

	for _, code := range []string{"bad_spec", "policy_denied"} {
		t.Run("rollback "+code, func(t *testing.T) {
			a, h, _, _ := imageRun(t)
			h.fn = (&swapHelper{prev: match, rollback: code}).call
			res := mustImage(t, a, "image_rollback", append([]byte(nil), serviceRollbackSpec()...), 120)
			m := assertDeploy(t, res, "forbidden_spec", "failed")
			if dig(m) != imgDigest {
				t.Fatal(m)
			}
			verbOrder(t, h, images.VerbStatus, images.VerbRollback, images.VerbLogs)
		})
	}
}

func TestKeptContent(t *testing.T) {
	a, h, _, _ := imageRun(t)
	k := &keptFiles{}
	h.fn = k.call
	aFile := images.TaskFile{Name: "app.conf", Target: "/etc/app.conf", Content: "content-a"}
	bFile := images.TaskFile{Name: "app.conf", Target: "/etc/app.conf", Content: "content-b"}
	res := mustImage(t, a, "image_deploy", fileTask(t, false, []images.TaskFile{aFile}), 120)
	assertDeploy(t, res, "succeeded", "succeeded")
	if k.cur == nil || k.cur.content != "content-a" || k.prev != nil {
		t.Fatalf("after A %+v %+v", k.cur, k.prev)
	}
	res = mustImage(t, a, "image_deploy", fileTask(t, false, []images.TaskFile{bFile}), 120)
	assertDeploy(t, res, "succeeded", "succeeded")
	if k.cur == nil || k.cur.content != "content-b" || k.prev == nil || k.prev.content != "content-a" {
		t.Fatalf("after B %+v %+v", k.cur, k.prev)
	}
	before := len(h.Calls())
	res = mustImage(t, a, "image_rollback", withDigest(t, fileTask(t, false, []images.TaskFile{bFile}), imgDigest), 120)
	m := assertDeploy(t, res, "succeeded", "succeeded")
	if dig(m) != imgDigest {
		t.Fatal(m)
	}
	calls := h.Calls()[before:]
	if len(calls) < 2 || calls[0] != images.VerbStatus || calls[1] != images.VerbRollback {
		t.Fatalf("%v", calls)
	}
	for _, c := range calls {
		if c == images.VerbPull || c == images.VerbDeploy {
			t.Fatalf("redeployed %v", calls)
		}
	}
	if k.cur == nil || k.cur.content != "content-a" || k.prev != nil {
		t.Fatalf("rolled %+v %+v", k.cur, k.prev)
	}
}

func TestImageStatusDropsPrev(t *testing.T) {
	a, h, _, _ := imageRun(t)
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb != images.VerbStatus {
			return nil, errors.New(verb)
		}
		return images.Encode(images.StatusResult{
			Status: runningStatus(0), SpecHash: imgHash,
			Prev: &images.PrevView{Digest: protocolStr(imgDigest), SpecHash: imgHash, FilesHash: otherDigest[7:]},
		}), nil
	}
	res := mustImage(t, a, "image_status", []byte(`{"project":"acme","entry":"web"}`), 60)
	raw := assertStatus(t, res, "succeeded", "succeeded")
	if strings.Contains(string(raw), "prev") || strings.Contains(string(raw), "files_hash") {
		t.Fatalf("%s", raw)
	}
}

func protocolStr(s string) *string { return &s }

type swapHelper struct {
	prev      *images.PrevView
	after     images.ImageStatus
	rolled    bool
	deployed  bool
	pullRef   string
	rollback  string
	statusErr string
}

func (s *swapHelper) call(verb string, body []byte) ([]byte, error) {
	switch verb {
	case images.VerbStatus:
		if s.statusErr != "" && !s.rolled && !s.deployed {
			return []byte(`{"error":"` + s.statusErr + `"}`), nil
		}
		if s.rolled || s.deployed {
			return images.Encode(images.StatusResult{Status: s.after, SpecHash: imgHash}), nil
		}
		return images.Encode(images.StatusResult{Status: missingStatus(), Prev: s.prev}), nil
	case images.VerbRollback:
		if err := readRollback(body); err != nil {
			return nil, err
		}
		if s.rollback != "" {
			return []byte(`{"error":"` + s.rollback + `"}`), nil
		}
		s.rolled = true
		return images.Encode(images.IDResponse{ContainerID: "prev"}), nil
	case images.VerbPull:
		var req images.PullRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		s.pullRef = req.Ref
		return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "sha256:image"}), nil
	case images.VerbDeploy:
		s.deployed = true
		return images.Encode(images.IDResponse{ContainerID: "c1"}), nil
	case images.VerbLogs:
		return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
	default:
		return nil, errors.New(verb)
	}
}

type keptFile struct {
	content string
	digest  string
	spec    string
	files   string
}

type keptFiles struct {
	cur, prev *keptFile
}

func (k *keptFiles) call(verb string, body []byte) ([]byte, error) {
	switch verb {
	case images.VerbPull:
		return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "sha256:image"}), nil
	case images.VerbStatus:
		return k.status(), nil
	case images.VerbDeploy:
		var req images.DeployRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		slot := &keptFile{digest: req.Digest, spec: req.SpecHash, files: images.FilesHash(req.Files)}
		if len(req.Files) > 0 {
			slot.content = req.Files[0].Content
		}
		if k.cur != nil {
			k.prev = k.cur
		}
		k.cur = slot
		return images.Encode(images.IDResponse{ContainerID: "c1"}), nil
	case images.VerbRollback:
		if err := readRollback(body); err != nil {
			return nil, err
		}
		if k.prev == nil {
			return []byte(`{"error":"conflict"}`), nil
		}
		k.cur = k.prev
		k.prev = nil
		return images.Encode(images.IDResponse{ContainerID: "prev"}), nil
	case images.VerbLogs:
		return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
	default:
		return nil, errors.New(verb)
	}
}

func (k *keptFiles) status() []byte {
	st := missingStatus()
	var hash, files string
	var prev *images.PrevView
	if k.cur != nil {
		st = runningStatus(0)
		d := k.cur.digest
		st.Digest = &d
		hash = k.cur.spec
		files = k.cur.files
	}
	if k.prev != nil {
		d := k.prev.digest
		prev = &images.PrevView{Digest: &d, SpecHash: k.prev.spec, FilesHash: k.prev.files}
	}
	return images.Encode(images.StatusResult{Status: st, SpecHash: hash, FilesHash: files, Prev: prev})
}

func withDigest(t *testing.T, spec []byte, digest string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(spec, &m); err != nil {
		t.Fatal(err)
	}
	m["digest"] = digest
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
