package protocol

import (
	"strings"
	"testing"
)

func TestContainerDiscovered(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	owned := `{"v":1,"type":"services","services":[{"name":"web","kind":"container","ports":[3031],"systemd_unit":null,"systemd_scope":null,"container_name":"acme-web","process_name":null,"pid":null,"user":null,"image_digest":"` + digest + `","owner":{"kind":"image","project":"acme","entry":"web"}}]}`
	if err := ValidateAgent([]byte(owned)); err != nil {
		t.Fatal(err)
	}
	other := `{"v":1,"type":"services","services":[{"name":"redis","kind":"container","ports":[6379],"systemd_unit":null,"systemd_scope":null,"container_name":"redis","process_name":null,"pid":null,"user":null,"image_digest":null,"owner":null}]}`
	if err := ValidateAgent([]byte(other)); err != nil {
		t.Fatal(err)
	}
	suffixed := strings.Replace(owned, `"name":"web"`, `"name":"web-2"`, 1)
	if err := ValidateAgent([]byte(suffixed)); err == nil {
		t.Fatal("owned name suffix")
	}
	if err := ValidateAgent([]byte(strings.Replace(owned, `"kind":"container"`, `"kind":"process"`, 1))); err == nil {
		t.Fatal("process accepted a container name")
	}
	if err := ValidateAgent([]byte(strings.Replace(other, `"image_digest":null`, `"image_digest":"nope"`, 1))); err == nil {
		t.Fatal("bad digest")
	}
	plain := `{"v":1,"type":"services","services":[{"name":"nginx","kind":"process","ports":[80],"systemd_unit":null,"systemd_scope":null,"container_name":null,"process_name":"nginx","pid":1,"user":null,"image_digest":"sha256:` + strings.Repeat("ab", 32) + `"}]}`
	if err := ValidateAgent([]byte(plain)); err == nil {
		t.Fatal("process accepted image_digest")
	}
	if err := ValidateAgent([]byte(strings.Replace(owned, `"container_name":"acme-web"`, `"container_name":"acme/web"`, 1))); err == nil {
		t.Fatal("slash in container_name")
	}
	hello := `{"v":1,"type":"hello","host_id":"11111111-1111-4111-8111-111111111111","credential_id":"22222222-2222-4222-8222-222222222222","agent_version":"0.1.0","os":"linux","arch":"arm64","capabilities":["metrics","probe"],"programs":{"enabled":false,"run_as":[],"allow_shell":false,"policy_hash":"` + strings.Repeat("ab", 32) + `","images":true,"docker":false},"services":[],"services_complete":true}`
	if err := ValidateAgent([]byte(hello)); err != nil {
		t.Fatal(err)
	}
}

func TestImagePayloadValidators(t *testing.T) {
	digest := "sha256:" + strings.Repeat("cd", 32)
	status := `{"container":"acme-web","state":"running","exit_code":null,"restart_count":0,"started_at":null,"finished_at":null,"digest":"` + digest + `","ports":[3031]}`
	deploy := `{"outcome":"succeeded","digest":"` + digest + `","exit_code":null,"status":` + status + `,"lines":["ok"]}`
	if err := ValidateImageDeploy([]byte(deploy)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImageDeploy([]byte(strings.Replace(deploy, `"succeeded"`, `"nope"`, 1))); err == nil {
		t.Fatal("bad outcome")
	}
	if err := ValidateImageDeploy([]byte(strings.Replace(deploy, `"lines":["ok"]`, `"lines":["ok"],"extra":1`, 1))); err == nil {
		t.Fatal("extra deploy key")
	}
	if err := ValidateImageStatus([]byte(`{"status":` + status + `}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImageStatus([]byte(`{"status":` + status + `,"spec_hash":"abc"}`)); err == nil {
		t.Fatal("status kept spec_hash")
	}
	logs := `{"container":"acme-web","lines":["a"],"truncated":false}`
	if err := ValidateImageLogs([]byte(logs)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImageLogs([]byte(strings.Replace(logs, `"truncated":false`, `"truncated":1`, 1))); err == nil {
		t.Fatal("truncated type")
	}
	job := strings.Replace(deploy, `"exit_code":null`, `"exit_code":0`, 1)
	if err := ValidateImageDeploy([]byte(job)); err != nil {
		t.Fatal(err)
	}
}
