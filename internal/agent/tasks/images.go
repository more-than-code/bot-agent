package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

// ImageReq is a validated image task. Env values and Auth.Token are secrets.
// Callers must not log them and must drop them when the task ends.
type ImageReq struct {
	Project  string
	Entry    string
	Spec     *images.ImageSpec
	Env      map[string]string
	Auth     *images.RegistryAuth
	Force    bool
	SpecHash string
	Digest   string
	Lines    int
	// Files is the resolved top-level list. Content is a secret when Secret is set.
	Files []images.TaskFile
	// SecretKeys is non-nil when the request included secret_keys, including
	// an empty list. Nil, which is also how JSON null decodes, means the key
	// was absent and every env value joins the set, as in v1.4.
	SecretKeys []string
}

// SecretValues is the exact set for this task. With SecretKeys set, only
// those env values join; otherwise every env value does. The registry token
// and the secret-file items always join. The set lives only for the task.
func (r ImageReq) SecretValues() []string {
	vals := make([]string, 0, len(r.Env)+1)
	if r.SecretKeys == nil {
		for _, v := range r.Env {
			vals = append(vals, v)
		}
	} else {
		for _, k := range r.SecretKeys {
			vals = append(vals, r.Env[k])
		}
	}
	if r.Auth != nil {
		vals = append(vals, r.Auth.Token)
	}
	for _, f := range r.Files {
		if f.Secret {
			vals = append(vals, redact.FileSecrets(f.Content)...)
		}
	}
	return vals
}

// ImageKind reports the eight v1.4 task kinds.
func ImageKind(kind string) bool {
	switch kind {
	case "image_deploy", "image_rollback", "image_remove", "image_start",
		"image_stop", "image_restart", "image_status", "image_logs":
		return true
	default:
		return false
	}
}

// ParseImage runs the image checks. An empty code means the task may run.
// Host, expiry, and a non-null service are the caller's checks and already won.
// Closed keys are refused before the policy so a bad shape is bad_spec even
// when images are off. Field rules run after the policy and the project check.
func ParseImage(task protocol.Task, pol policy.Policy) (ImageReq, string) {
	req, code := parseImage(task, pol)
	if code != "" {
		scrubImage(&req)
		return ImageReq{}, code
	}
	return req, ""
}

func parseImage(task protocol.Task, pol policy.Policy) (ImageReq, string) {
	if len(task.Spec) == 0 || len(task.Spec) > protocol.MaxFrame {
		return ImageReq{}, "bad_spec"
	}
	switch task.Kind {
	case "image_deploy":
		return parseDeploy(task.Spec, pol, false)
	case "image_rollback":
		return parseDeploy(task.Spec, pol, true)
	case "image_logs":
		return parseLogs(task.Spec, pol)
	default:
		return parseEntry(task.Spec, pol)
	}
}

type deployBody struct {
	Project      *string              `json:"project"`
	Entry        *string              `json:"entry"`
	Spec         *images.ImageSpec    `json:"spec"`
	Env          map[string]string    `json:"env"`
	RegistryAuth *images.RegistryAuth `json:"registry_auth"`
	Force        bool                 `json:"force"`
	SpecHash     *string              `json:"spec_hash"`
	Files        []images.TaskFile    `json:"files"`
	SecretKeys   []string             `json:"secret_keys"`
}

type rollbackBody struct {
	deployBody
	Digest *string `json:"digest"`
}

func parseDeploy(raw []byte, pol policy.Policy, rollback bool) (ImageReq, string) {
	var body deployBody
	var digest *string
	if rollback {
		var rb rollbackBody
		if err := decodeStrict(raw, &rb); err != nil {
			return ImageReq{}, "bad_spec"
		}
		body = rb.deployBody
		digest = rb.Digest
		if digest == nil {
			return ImageReq{}, "bad_spec"
		}
	} else if err := decodeStrict(raw, &body); err != nil {
		return ImageReq{}, "bad_spec"
	}
	if body.Project == nil || body.Entry == nil || body.Spec == nil || body.SpecHash == nil {
		return ImageReq{}, "bad_spec"
	}
	if code := policyAndProject(pol, *body.Project); code != "" {
		return ImageReq{}, code
	}
	req := ImageReq{
		Project: *body.Project, Entry: *body.Entry, Spec: body.Spec,
		Env: body.Env, Auth: body.RegistryAuth, Force: body.Force, SpecHash: *body.SpecHash,
		Files: body.Files, SecretKeys: body.SecretKeys,
	}
	if digest != nil {
		req.Digest = *digest
	}
	if code := deployFields(req); code != "" {
		return req, code
	}
	if code := imagePattern(pol, req); code != "" {
		return req, code
	}
	return req, ""
}

type entryBody struct {
	Project *string `json:"project"`
	Entry   *string `json:"entry"`
}

func parseEntry(raw []byte, pol policy.Policy) (ImageReq, string) {
	var body entryBody
	if err := decodeStrict(raw, &body); err != nil {
		return ImageReq{}, "bad_spec"
	}
	if body.Project == nil || body.Entry == nil {
		return ImageReq{}, "bad_spec"
	}
	if code := policyAndProject(pol, *body.Project); code != "" {
		return ImageReq{}, code
	}
	req := ImageReq{Project: *body.Project, Entry: *body.Entry}
	if !images.ValidEntry(req.Entry) {
		return req, "bad_spec"
	}
	return req, ""
}

type logsBody struct {
	Project *string `json:"project"`
	Entry   *string `json:"entry"`
	Lines   *int    `json:"lines"`
}

func parseLogs(raw []byte, pol policy.Policy) (ImageReq, string) {
	var body logsBody
	if err := decodeStrict(raw, &body); err != nil {
		return ImageReq{}, "bad_spec"
	}
	if body.Project == nil || body.Entry == nil || body.Lines == nil {
		return ImageReq{}, "bad_spec"
	}
	if code := policyAndProject(pol, *body.Project); code != "" {
		return ImageReq{}, code
	}
	req := ImageReq{Project: *body.Project, Entry: *body.Entry, Lines: *body.Lines}
	if !images.ValidEntry(req.Entry) || req.Lines < 1 || req.Lines > 2000 {
		return req, "bad_spec"
	}
	return req, ""
}

func policyAndProject(pol policy.Policy, project string) string {
	if !pol.ImagesEnabled() {
		return "policy_denied"
	}
	if !projectListed(pol.Projects, project) {
		return "policy_denied"
	}
	return ""
}

func projectListed(projects []string, project string) bool {
	for _, p := range projects {
		if p == project {
			return true
		}
	}
	return false
}

func deployFields(req ImageReq) string {
	if !images.ValidEntry(req.Entry) || !images.ValidSpecHash(req.SpecHash) {
		return "bad_spec"
	}
	if req.Digest != "" && !images.ValidDigest(req.Digest) {
		return "bad_spec"
	}
	if !authOK(req.Auth) {
		return "bad_spec"
	}
	if field := req.Spec.Validate(); field != "" {
		return "bad_spec"
	}
	if field := images.ResolvedEnv(req.Env); field != "" {
		return "bad_spec"
	}
	if req.SecretKeys != nil {
		for _, k := range req.SecretKeys {
			if _, ok := req.Env[k]; !ok {
				return "bad_spec"
			}
		}
	}
	if images.CheckResolved(req.Spec.Files, images.AsMount(req.Files)) != "" {
		return "bad_spec"
	}
	return ""
}

func authOK(a *images.RegistryAuth) bool {
	if a == nil {
		return true
	}
	if a.Host == "" || a.Username == "" || a.Token == "" {
		return false
	}
	if len(a.Host) > 255 || len(a.Username) > 255 || len(a.Token) > 4096 {
		return false
	}
	if strings.ContainsAny(a.Host, "\r\n\x00") || strings.ContainsAny(a.Username, "\r\n\x00") || strings.ContainsAny(a.Token, "\r\n\x00") {
		return false
	}
	return true
}

func imagePattern(pol policy.Policy, req ImageReq) string {
	ref, err := images.Parse(req.Spec.Image)
	if err != nil || !images.Match(ref, pol.Images) {
		return "policy_denied"
	}
	if req.Digest == "" {
		return ""
	}
	pinned, err := images.Parse(ref.Name + "@" + req.Digest)
	if err != nil || !images.Match(pinned, pol.Images) {
		return "policy_denied"
	}
	return ""
}

func decodeStrict(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing")
	}
	return nil
}

func scrubImage(req *ImageReq) {
	if req.Env != nil {
		for k := range req.Env {
			req.Env[k] = ""
		}
		req.Env = nil
	}
	if req.Auth != nil {
		req.Auth.Token = ""
		req.Auth = nil
	}
	for i := range req.Files {
		req.Files[i].Content = ""
	}
	req.Files = nil
	req.Spec = nil
}
