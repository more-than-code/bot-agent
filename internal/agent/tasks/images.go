package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
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
	// EnvHash is the task's opaque env_hash (agent-protocol v1.14), or ""
	// when the task had none. It is not a secret.
	EnvHash string
}

// SecretValues splits the exact set for this task. env is the env secret
// values: every env value when SecretKeys is nil, otherwise only the listed
// keys. file is the registry token and the secret-file pieces. Env values
// are previewed. The token and file pieces stay [REDACTED]. Both slices
// live only for the task.
func (r ImageReq) SecretValues() (env, file []string) {
	if r.SecretKeys == nil {
		env = make([]string, 0, len(r.Env))
		for _, v := range r.Env {
			env = append(env, v)
		}
	} else {
		env = make([]string, 0, len(r.SecretKeys))
		for _, k := range r.SecretKeys {
			env = append(env, r.Env[k])
		}
	}
	file = make([]string, 0, 1)
	if r.Auth != nil {
		file = append(file, r.Auth.Token)
	}
	for _, f := range r.Files {
		if f.Secret {
			file = append(file, redact.FileSecrets(f.Content)...)
		}
	}
	return env, file
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
	req, code, _ := parseImage(task, pol)
	if code != "" {
		scrubImage(&req)
		return ImageReq{}, code
	}
	return req, ""
}

// BadSpecField is the task_reject field (agent-protocol v1.23) of an image
// task that ParseImage refuses with bad_spec: the task key that failed, or
// "spec.<key>" for an ImageSpec rule, from a fixed set of key names. It
// never holds a value or an env key name. "" when the task is not refused
// with bad_spec or no single key is to blame (the spec is not JSON).
func BadSpecField(task protocol.Task, pol policy.Policy) string {
	if task.Service != nil {
		return "service"
	}
	req, code, field := parseImage(task, pol)
	scrubImage(&req)
	if code != "bad_spec" || !protocol.ValidRejectField(field) {
		return ""
	}
	return field
}

func parseImage(task protocol.Task, pol policy.Policy) (ImageReq, string, string) {
	if len(task.Spec) == 0 || len(task.Spec) > protocol.MaxFrame {
		return ImageReq{}, "bad_spec", ""
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
	EnvHash      *string              `json:"env_hash"`
}

type rollbackBody struct {
	deployBody
	Digest *string `json:"digest"`
}

func parseDeploy(raw []byte, pol policy.Policy, rollback bool) (ImageReq, string, string) {
	var body deployBody
	var digest *string
	if rollback {
		var rb rollbackBody
		if err := decodeStrict(raw, &rb); err != nil {
			scrubBody(&rb.deployBody)
			return ImageReq{}, "bad_spec", decodeField(err)
		}
		body = rb.deployBody
		digest = rb.Digest
		if digest == nil {
			scrubBody(&body)
			return ImageReq{}, "bad_spec", "digest"
		}
	} else if err := decodeStrict(raw, &body); err != nil {
		scrubBody(&body)
		return ImageReq{}, "bad_spec", decodeField(err)
	}
	if field := missingDeployKey(body); field != "" {
		scrubBody(&body)
		return ImageReq{}, "bad_spec", field
	}
	if code := policyAndProject(pol, *body.Project); code != "" {
		scrubBody(&body)
		return ImageReq{}, code, ""
	}
	req := ImageReq{
		Project: *body.Project, Entry: *body.Entry, Spec: body.Spec,
		Env: body.Env, Auth: body.RegistryAuth, Force: body.Force, SpecHash: *body.SpecHash,
		Files: body.Files, SecretKeys: body.SecretKeys,
	}
	if digest != nil {
		req.Digest = *digest
	}
	if body.EnvHash != nil {
		// Present means exactly 64 lowercase hex. "" is refused too.
		if !images.ValidSpecHash(*body.EnvHash) {
			return req, "bad_spec", "env_hash"
		}
		req.EnvHash = *body.EnvHash
	}
	if field := deployFields(req); field != "" {
		return req, "bad_spec", field
	}
	if code := imagePattern(pol, req); code != "" {
		return req, code, ""
	}
	if code := sharedNetworks(pol, req); code != "" {
		return req, code, ""
	}
	// v1.21 item 3: network "host" only for a host_network_projects slug.
	if req.Spec.HostNetwork() && !pol.HostNetworkAllowed(req.Project) {
		return req, "policy_denied", ""
	}
	return req, "", ""
}

// scrubBody drops the secrets of a deploy body that never became an
// ImageReq (ParseImage scrubs the ImageReq it returns).
func scrubBody(body *deployBody) {
	req := ImageReq{Env: body.Env, Auth: body.RegistryAuth, Files: body.Files}
	scrubImage(&req)
	body.Env, body.RegistryAuth, body.Files = nil, nil, nil
}

// missingDeployKey is the first required deploy key that is absent or null.
func missingDeployKey(body deployBody) string {
	switch {
	case body.Project == nil:
		return "project"
	case body.Entry == nil:
		return "entry"
	case body.Spec == nil:
		return "spec"
	case body.SpecHash == nil:
		return "spec_hash"
	}
	return ""
}

// sharedNetworks is the v1.13 pre-check: every spec network is defined in
// the policy and lists the task's project. The shape was checked by Validate.
func sharedNetworks(pol policy.Policy, req ImageReq) string {
	for _, name := range req.Spec.Networks {
		if !pol.NetworkAllowed(name, req.Project) {
			return "policy_denied"
		}
	}
	return ""
}

type entryBody struct {
	Project *string `json:"project"`
	Entry   *string `json:"entry"`
}

func parseEntry(raw []byte, pol policy.Policy) (ImageReq, string, string) {
	var body entryBody
	if err := decodeStrict(raw, &body); err != nil {
		return ImageReq{}, "bad_spec", decodeField(err)
	}
	if body.Project == nil {
		return ImageReq{}, "bad_spec", "project"
	}
	if body.Entry == nil {
		return ImageReq{}, "bad_spec", "entry"
	}
	if code := policyAndProject(pol, *body.Project); code != "" {
		return ImageReq{}, code, ""
	}
	req := ImageReq{Project: *body.Project, Entry: *body.Entry}
	if !images.ValidEntry(req.Entry) {
		return req, "bad_spec", "entry"
	}
	return req, "", ""
}

type logsBody struct {
	Project *string `json:"project"`
	Entry   *string `json:"entry"`
	Lines   *int    `json:"lines"`
}

func parseLogs(raw []byte, pol policy.Policy) (ImageReq, string, string) {
	var body logsBody
	if err := decodeStrict(raw, &body); err != nil {
		return ImageReq{}, "bad_spec", decodeField(err)
	}
	switch {
	case body.Project == nil:
		return ImageReq{}, "bad_spec", "project"
	case body.Entry == nil:
		return ImageReq{}, "bad_spec", "entry"
	case body.Lines == nil:
		return ImageReq{}, "bad_spec", "lines"
	}
	if code := policyAndProject(pol, *body.Project); code != "" {
		return ImageReq{}, code, ""
	}
	req := ImageReq{Project: *body.Project, Entry: *body.Entry, Lines: *body.Lines}
	if !images.ValidEntry(req.Entry) {
		return req, "bad_spec", "entry"
	}
	if req.Lines < 1 || req.Lines > 2000 {
		return req, "bad_spec", "lines"
	}
	return req, "", ""
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

// deployFields runs the field rules of a deploy or rollback. It returns
// the field that failed (BadSpecField), or "" when every rule passes.
func deployFields(req ImageReq) string {
	if !images.ValidEntry(req.Entry) {
		return "entry"
	}
	if !images.ValidSpecHash(req.SpecHash) {
		return "spec_hash"
	}
	if req.Digest != "" && !images.ValidDigest(req.Digest) {
		return "digest"
	}
	if !authOK(req.Auth) {
		return "registry_auth"
	}
	if field := req.Spec.Validate(); field != "" {
		return "spec." + field
	}
	if images.ResolvedEnv(req.Env) != "" {
		return "env"
	}
	if req.SecretKeys != nil {
		for _, k := range req.SecretKeys {
			if _, ok := req.Env[k]; !ok {
				return "secret_keys"
			}
		}
	}
	if images.CheckResolved(req.Spec.Files, images.AsMount(req.Files)) != "" {
		return "files"
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

// decodeField names the key of a strict-decode type error, from the image
// body keys and, under spec, the ImageSpec keys. Anything else, including an
// unknown key and a map key, gives "": only names this agent defines leave
// the host.
func decodeField(err error) string {
	var te *json.UnmarshalTypeError
	if !errors.As(err, &te) || te.Field == "" {
		return ""
	}
	parts := strings.SplitN(te.Field, ".", 3)
	if !bodyKeys[parts[0]] {
		return ""
	}
	if parts[0] != "spec" || len(parts) < 2 || !specKeys[parts[1]] {
		return parts[0]
	}
	// A key of a nested spec object, named as Validate names it.
	if len(parts) == 3 {
		if keys := nestedKeys[parts[1]]; keys != nil {
			if key, _, _ := strings.Cut(parts[2], "."); keys[key] {
				return "spec." + parts[1] + "." + key
			}
		}
	}
	return "spec." + parts[1]
}

var (
	bodyKeys   = jsonKeys(rollbackBody{}, logsBody{})
	specKeys   = jsonKeys(images.ImageSpec{})
	nestedKeys = map[string]map[string]bool{
		"healthcheck":   jsonKeys(images.Healthcheck{}),
		"health_policy": jsonKeys(images.HealthPolicy{}),
		"logging":       jsonKeys(images.Logging{}),
	}
)

// jsonKeys is the set of json tag names of the structs' fields, embedded
// structs included.
func jsonKeys(values ...any) map[string]bool {
	keys := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				keys[name] = true
			}
		}
	}
	for _, v := range values {
		walk(reflect.TypeOf(v))
	}
	return keys
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
