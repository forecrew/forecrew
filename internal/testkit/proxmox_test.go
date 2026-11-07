package testkit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// proxmoxCall makes one call to the fake as its token and returns the status and
// the data.
func proxmoxCall(t *testing.T, f *FakeProxmox, method, path string, form url.Values) (int, any) {
	t.Helper()
	return proxmoxCallAs(t, f, f.Token, method, path, form)
}

func proxmoxCallAs(t *testing.T, f *FakeProxmox, token, method, path string, form url.Values) (int, any) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(f.CAPEM())
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"}}}
	u := f.URL() + "/api2/json" + path
	var req *http.Request
	var err error
	if method == http.MethodGet || method == http.MethodDelete {
		req, err = http.NewRequestWithContext(context.Background(), method, u+"?"+form.Encode(), nil)
	} else {
		req, err = http.NewRequestWithContext(context.Background(), method, u, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "PVEAPIToken="+token)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data any `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Data
}

func TestFakeProxmoxLifecycle(t *testing.T) {
	f := NewFakeProxmox(t)
	f.AddTemplate(9000, "pve", "forecrew-image-x", nil)
	f.AddVM(500, "pve", "plain", "mine")
	if f.Count() != 1 || f.VM(9000) == nil || f.VM(1) != nil {
		t.Fatalf("count %d", f.Count())
	}
	st, data := proxmoxCall(t, f, "GET", "/cluster/nextid", nil)
	if st != 200 {
		t.Fatalf("nextid %d", st)
	}
	id := data.(string)
	st, upid := proxmoxCall(t, f, "POST", "/nodes/pve/qemu/9000/clone", url.Values{"newid": {id}, "name": {"forecrew-a"}, "description": {"notes"}})
	if st != 200 || upid == nil {
		t.Fatalf("clone %d", st)
	}
	if st, d := proxmoxCall(t, f, "GET", "/nodes/pve/tasks/"+upid.(string)+"/status", nil); st != 200 || d.(map[string]any)["exitstatus"] != "OK" {
		t.Errorf("task %d %v", st, d)
	}
	if st, _ := proxmoxCall(t, f, "POST", "/nodes/pve/qemu/9000/clone", url.Values{"newid": {id}}); st != 500 {
		t.Errorf("clone onto a taken ID: %d", st)
	}
	if st, _ := proxmoxCall(t, f, "POST", "/nodes/pve/qemu/9000/clone", url.Values{"newid": {"x"}}); st != 400 {
		t.Errorf("clone with a bad ID: %d", st)
	}
	base := "/nodes/pve/qemu/" + id
	for _, c := range []struct {
		method, path string
		form         url.Values
		want         int
	}{
		{"PUT", base + "/config", url.Values{"cores": {"2"}}, 200},
		{"GET", base + "/config", nil, 200},
		{"PUT", base + "/resize", url.Values{"disk": {"scsi0"}, "size": {"40G"}}, 200},
		{"PUT", base + "/resize", url.Values{"disk": {"scsi9"}, "size": {"40G"}}, 500},
		{"GET", base + "/status/current", nil, 200},
		{"POST", base + "/agent/ping", nil, 500}, // not running yet
		{"POST", base + "/agent/file-write", url.Values{"file": {"/x"}, "content": {"y"}}, 500},
		{"POST", base + "/status/start", nil, 200},
		{"POST", base + "/agent/ping", nil, 200},
		{"POST", base + "/agent/file-write", url.Values{"file": {"/x"}, "content": {"y"}}, 200},
		{"POST", base + "/agent/file-write", url.Values{"file": {"/x"}, "content": {strings.Repeat("y", 61441)}}, 400},
		{"PUT", base + "/firewall/options", url.Values{"enable": {"1"}}, 200},
		{"POST", base + "/firewall/rules", url.Values{"type": {"out"}, "action": {"DROP"}, "dest": {"10.0.0.0/8"}}, 200},
		{"POST", base + "/firewall/rules", url.Values{"type": {"out"}}, 400},
		{"GET", base + "/firewall/rules", nil, 200},
		{"DELETE", base + "/firewall/rules/0", nil, 200},
		{"DELETE", base + "/firewall/rules/0", nil, 500},
		{"DELETE", base, nil, 500}, // running
		{"POST", base + "/status/stop", nil, 200},
		{"DELETE", base, nil, 200},
		{"GET", base + "/config", nil, 500},
		{"POST", "/nodes/pve/qemu/9000/status/start", nil, 500}, // a template
		{"PUT", "/nodes/pve/qemu/9000/config", url.Values{"cores": {"2"}}, 500},
		{"GET", "/nodes/pve/status", nil, 200},
		{"GET", "/nodes/pve/storage", url.Values{"content": {"images"}}, 200},
		{"GET", "/nodes/nowhere/status", nil, 500},
		{"GET", "/nodes/pve/tasks/UPID:none/status", nil, 500},
		{"GET", "/cluster/resources", url.Values{"type": {"vm"}}, 200},
		{"GET", "/cluster/firewall/options", nil, 200},
		{"GET", "/version", nil, 200},
		{"GET", "/nodes/pve/unknown", nil, 501},
		{"GET", "/nothing", nil, 501},
	} {
		if st, _ := proxmoxCall(t, f, c.method, c.path, c.form); st != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, st, c.want)
		}
	}
	if f.Count() != 1 {
		t.Errorf("count %d", f.Count())
	}
	if len(f.CallsTo("/clone")) != 3 || len(f.Forms("/clone")) != 3 || len(f.Forms("/version")) != 0 {
		t.Errorf("calls %v forms %v", f.CallsTo("/clone"), f.Forms("/clone"))
	}
}

func TestFakeProxmoxFailuresAndAuth(t *testing.T) {
	f := NewFakeProxmox(t)
	f.FailOn = func(call string) (int, string) {
		if strings.HasSuffix(call, "/version") {
			return 503, "busy"
		}
		return 0, ""
	}
	if st, _ := proxmoxCall(t, f, "GET", "/version", nil); st != 503 {
		t.Errorf("injected failure: %d", st)
	}
	f.FailOn = nil
	f.AddTemplate(9000, "pve", "", nil)
	f.FailTask["/clone"] = "clone failed"
	_, upid := proxmoxCall(t, f, "POST", "/nodes/pve/qemu/9000/clone", url.Values{"newid": {"700"}, "name": {"x"}})
	if _, d := proxmoxCall(t, f, "GET", "/nodes/pve/tasks/"+upid.(string)+"/status", nil); d.(map[string]any)["exitstatus"] != "clone failed" {
		t.Errorf("task = %v", d)
	}
	f.FirewallInsertsAtTop = true
	f.AddVM(701, "pve", "forecrew-x", "")
	for _, a := range []string{"A", "B"} {
		proxmoxCall(t, f, "POST", "/nodes/pve/qemu/701/firewall/rules", url.Values{"type": {"out"}, "action": {"ACCEPT"}, "dest": {a}})
	}
	if r := f.VM(701).Rules; r[0].Dest != "B" {
		t.Errorf("rules = %+v", r)
	}
	if st, _ := proxmoxCallAs(t, f, "someone@pve!else=00000000", "GET", "/version", nil); st != 401 {
		t.Errorf("a token the fake does not accept: %d", st)
	}
}
