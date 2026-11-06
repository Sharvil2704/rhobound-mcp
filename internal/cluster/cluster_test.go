// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

var (
	t0  = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	win = &Window{Start: t0, End: t0.Add(10 * time.Minute)}
)

func list(items ...string) string {
	return `{"apiVersion":"v1","kind":"List","items":[` + strings.Join(items, ",") + `]}`
}

func service(name, ip string, ports ...int) string {
	var ps []string
	for _, p := range ports {
		ps = append(ps, fmt.Sprintf(`{"port":%d}`, p))
	}
	return fmt.Sprintf(`{"kind":"Service","metadata":{"name":%q,"namespace":"shop"},"spec":{"clusterIP":%q,"ports":[%s]}}`,
		name, ip, strings.Join(ps, ","))
}

func pod(name, created, node, imageID string, restarts int) string {
	return fmt.Sprintf(`{"kind":"Pod","metadata":{"name":%q,"namespace":"shop","creationTimestamp":%q},"spec":{"nodeName":%q,`+
		`"containers":[{"name":"app","env":[{"name":"OTEL_SERVICE_NAME","value":"b"},{"name":"DB_PASSWORD","valueFrom":{"secretKeyRef":{"name":"x","key":"y"}}}]}]},`+
		`"status":{"containerStatuses":[{"name":"app","imageID":%q,"restartCount":%d}]}}`, name, created, node, imageID, restarts)
}

const node = `{"kind":"Node","metadata":{"name":"n1","labels":{"node.kubernetes.io/instance-type":"m7g.xlarge","kubernetes.io/arch":"arm64"}},` +
	`"status":{"nodeInfo":{"kernelVersion":"6.1.0","architecture":"arm64"}}}`

const deployment = `{"kind":"Deployment","metadata":{"name":"b","namespace":"shop"},"spec":{"replicas":2,"template":{"spec":` +
	`{"containers":[{"name":"app","image":"reg/b:1","resources":{"limits":{"cpu":"1"}}}]}}}}`

const hpa = `{"kind":"HorizontalPodAutoscaler","metadata":{"name":"b"},"status":{"currentReplicas":2,"desiredReplicas":3}}`

func digest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func codes(fs []contract.Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return strings.Join(out, ",")
}

func at(t time.Time) *time.Time { return &t }

func TestNoClusterFolder(t *testing.T) {
	d, fs, err := Read(t.TempDir(), TakenAt{}, win)
	if d != nil || fs != nil || err != nil {
		t.Errorf("%v %v %v", d, fs, err)
	}
}

func TestStartEnd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cluster", "start", "a.json"), list(service("b", "192.0.2.10", 8080, 9090), node, deployment,
		pod("b-1", "2026-10-26T08:00:00Z", "n1", "docker-pullable://reg/b@"+digest('a'), 0),
		pod("b-2", "2026-10-26T08:00:00Z", "n1", "reg/b@"+digest('a'), 0)))
	writeFile(t, filepath.Join(dir, "cluster", "end", "a.json"), list(service("b", "192.0.2.10", 8080), service("db", "None", 5432), hpa,
		pod("b-1", "2026-10-26T08:00:00Z", "n1", "reg/b@"+digest('a'), 1),
		pod("b-3", "2026-10-26T12:05:00Z", "n1", "reg/b@"+digest('b'), 0)))
	d, fs, err := Read(dir, TakenAt{Start: at(t0.Add(-20 * time.Second)), End: at(t0.Add(10*time.Minute + 15*time.Second))}, win)
	if err != nil || len(fs) != 0 {
		t.Fatalf("%v %v", err, fs)
	}
	if d.Layout != contract.LayoutStartEnd || d.Start == d.End || !strings.HasPrefix(d.Start.Digest, "sha256:") || d.Start.Digest == d.End.Digest {
		t.Fatalf("dump %+v", d)
	}
	names := d.PeerNames()
	if names["192.0.2.10:8080"] != "b" || names["192.0.2.10:9090"] != "b" || names["192.0.2.10"] != "b" || len(names) != 3 {
		t.Errorf("peer names %v (a headless Service has no address)", names)
	}
	if got := d.Churn(*win); fmt.Sprint(got) != "[b-1 b-2 b-3]" {
		t.Errorf("churn %v: b-1 restarted, b-2 went away, b-3 was created inside the window", got)
	}
	env := d.Environment()
	if fmt.Sprint(env.PodImages["b-3"]) != "["+digest('b')+"]" || env.PodNode["b-1"] != "n1" || len(env.Nodes) != 0 {
		t.Errorf("environment of the end snapshot: %+v", env)
	}
	if st := d.Start; len(st.Nodes) != 1 || st.Nodes[0].InstanceType != "m7g.xlarge" || st.Nodes[0].Arch != "arm64" ||
		len(st.Deployments) != 1 || *st.Deployments[0].Replicas != 2 || st.Deployments[0].Containers[0].Resources != `{"limits":{"cpu":"1"}}` {
		t.Errorf("start snapshot %+v", st)
	}
	if h := d.End.HPAs; len(h) != 1 || h[0].Desired != 3 {
		t.Errorf("autoscalers %+v", h)
	}
	// the spans' digests against the running ones
	got := d.CheckDigests(map[string]map[string][]string{"b": {"b-1": {"reg/b@" + digest('a')}, "b-3": {"reg/b@" + digest('a')}}})
	if codes(got) != contract.ClusterDigestDisagrees || got[0].Subject != "b" || got[0].Examples[0] != "b-3" {
		t.Errorf("digest check %+v", got)
	}
	if got := d.CheckDigests(map[string]map[string][]string{"b": {"b-3": {"reg/b@" + digest('b')}}}); len(got) != 0 {
		t.Errorf("agreeing digests: %+v", got)
	}
}

// The flat folder simpler capture tools write is one snapshot for the whole window.
func TestFlatSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cluster", "services.json"), list(service("c", "192.0.2.11", 80)))
	d, fs, err := Read(dir, TakenAt{}, win)
	if err != nil || d.Layout != contract.LayoutSingleSnapshot || d.Start != d.End {
		t.Fatalf("%v %+v", err, d)
	}
	if codes(fs) != contract.ClusterTimeUnknown+","+contract.ClusterSingleSnapshot {
		t.Errorf("findings %s", codes(fs))
	}
	if d.PeerNames()["192.0.2.11:80"] != "c" {
		t.Error("the flat snapshot names no peers")
	}
}

func TestLayoutAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cluster", "services.json"), list())
	writeFile(t, filepath.Join(dir, "cluster", "start", "a.json"), list())
	_, fs, err := Read(dir, TakenAt{}, win)
	if err != nil || codes(fs) != contract.ClusterLayoutAmbiguous {
		t.Errorf("%v %s", err, codes(fs))
	}
}

func TestNotList(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":     "kind: List\n",
		"one object":   service("b", "192.0.2.10", 80),
		"no items":     `{"kind":"List"}`,
		"other kind":   `{"kind":"Status","items":[]}`,
		"service list": `{"kind":"ServiceList","items":[` + service("b", "192.0.2.10", 80) + `]}`,
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "cluster", "x.txt"), body)
		_, fs, err := Read(dir, TakenAt{Start: at(t0)}, win)
		want := contract.ClusterNotList + "," + contract.ClusterSingleSnapshot
		if name == "service list" {
			want = contract.ClusterSingleSnapshot
		}
		if err != nil || codes(fs) != want {
			t.Errorf("%s: %v %s, want %s", name, err, codes(fs), want)
		}
	}
}

func TestUnscrubbed(t *testing.T) {
	for name, item := range map[string]string{
		"secret":       `{"kind":"Secret","metadata":{"name":"s"},"data":{"k":"dg=="}}`,
		"config map":   `{"kind":"ConfigMap","metadata":{"name":"c"},"data":{"k":"v"}}`,
		"managed":      `{"kind":"Service","metadata":{"name":"b","managedFields":[{"manager":"kubectl"}]}}`,
		"last-applied": `{"kind":"Service","metadata":{"name":"b","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}"}}}`,
		"env value":    `{"kind":"Deployment","metadata":{"name":"b"},"spec":{"template":{"spec":{"containers":[{"name":"a","env":[{"name":"DB_PASSWORD","value":"hunter2"}]}]}}}}`,
		"otel headers": `{"kind":"Pod","metadata":{"name":"b"},"spec":{"containers":[{"name":"a","env":[{"name":"OTEL_EXPORTER_OTLP_HEADERS","value":"authorization=x"}]}]}}`,
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "cluster", "start", "a.json"), list(item))
		d, fs, err := Read(dir, TakenAt{Start: at(t0)}, win)
		if err != nil || codes(fs) != contract.ClusterUnscrubbed {
			t.Errorf("%s: %v %s", name, err, codes(fs))
		}
		if len(fs) == 1 && strings.Contains(strings.Join(fs[0].Examples, ""), "hunter2") {
			t.Errorf("%s: the finding leaks the value", name)
		}
		if d != nil && d.Start != nil && (len(d.Start.Services)+len(d.Start.Deployments)+len(d.Start.Pods)) != 0 {
			t.Errorf("%s: an unscrubbed object was read", name)
		}
	}
	// non-secret OTEL_* values and secret references are fine
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cluster", "start", "a.json"), list(pod("b-1", "2026-10-26T08:00:00Z", "n1", "", 0)))
	if _, fs, err := Read(dir, TakenAt{Start: at(t0)}, win); err != nil || len(fs) != 0 {
		t.Errorf("a scrubbed pod: %v %s", err, codes(fs))
	}
}

func TestDumpTimes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cluster", "start", "a.json"), list())
	writeFile(t, filepath.Join(dir, "cluster", "end", "a.json"), list())
	for _, c := range []struct {
		taken TakenAt
		want  string
	}{
		{TakenAt{Start: at(t0.Add(-59 * time.Second)), End: at(t0.Add(11*time.Minute - time.Second))}, ""},
		{TakenAt{Start: at(t0.Add(-2 * time.Minute)), End: at(t0.Add(10 * time.Minute))}, contract.ClusterWindowMismatch},
		{TakenAt{Start: at(t0), End: at(t0.Add(12 * time.Minute))}, contract.ClusterWindowMismatch},
		{TakenAt{End: at(t0.Add(10 * time.Minute))}, contract.ClusterTimeUnknown},
	} {
		_, fs, err := Read(dir, c.taken, win)
		if err != nil || codes(fs) != c.want {
			t.Errorf("%+v: %v %s, want %s", c.taken, err, codes(fs), c.want)
		}
	}
}

func TestPeerMap(t *testing.T) {
	dir := t.TempDir()
	if m, fs, err := ReadPeerMap(dir, win); m != nil || fs != nil || err != nil {
		t.Errorf("no peer map: %v %v %v", m, fs, err)
	}
	good := map[string]any{"schema": PeerMapSchema, "taken_at": "2026-10-26T12:00:30Z",
		"addresses":     map[string]string{"192.0.2.20:8080": "checkout", "cart.local": "cart"},
		"desired_count": map[string]int{"checkout": 2}, "instance_type": map[string]string{"checkout": "m7g.large"}}
	b, _ := json.Marshal(good)
	writeFile(t, filepath.Join(dir, "peer-map.json"), string(b))
	m, fs, err := ReadPeerMap(dir, win)
	if err != nil || len(fs) != 0 || m.PeerNames()["192.0.2.20:8080"] != "checkout" || m.DesiredCount["checkout"] != 2 {
		t.Fatalf("%v %v %+v", err, fs, m)
	}
	for name, edit := range map[string]func(map[string]any){
		"schema":        func(m map[string]any) { m["schema"] = "rhobound.peer-map/v2" },
		"no addresses":  func(m map[string]any) { m["addresses"] = map[string]string{} },
		"unknown key":   func(m map[string]any) { m["bucket"] = "x" },
		"empty service": func(m map[string]any) { m["addresses"] = map[string]string{"192.0.2.1": ""} },
		"negative":      func(m map[string]any) { m["desired_count"] = map[string]int{"x": -1} },
		"bad time":      func(m map[string]any) { m["taken_at"] = "yesterday" },
	} {
		c := map[string]any{}
		for k, v := range good {
			c[k] = v
		}
		edit(c)
		b, _ := json.Marshal(c)
		writeFile(t, filepath.Join(dir, "peer-map.json"), string(b))
		if _, fs, err := ReadPeerMap(dir, win); err != nil || codes(fs) != contract.ClusterPeerMapInvalid {
			t.Errorf("%s: %v %s", name, err, codes(fs))
		}
	}
	writeFile(t, filepath.Join(dir, "peer-map.json"), "not json")
	if _, fs, _ := ReadPeerMap(dir, win); codes(fs) != contract.ClusterPeerMapInvalid {
		t.Errorf("not JSON: %s", codes(fs))
	}
	late := map[string]any{"schema": PeerMapSchema, "taken_at": "2026-10-26T13:00:00Z", "addresses": map[string]string{"a": "b"}}
	b, _ = json.Marshal(late)
	writeFile(t, filepath.Join(dir, "peer-map.json"), string(b))
	if _, fs, _ := ReadPeerMap(dir, win); codes(fs) != contract.ClusterWindowMismatch {
		t.Errorf("late peer map: %s", codes(fs))
	}
	untimed := map[string]any{"schema": PeerMapSchema, "addresses": map[string]string{"a": "b"}}
	b, _ = json.Marshal(untimed)
	writeFile(t, filepath.Join(dir, "peer-map.json"), string(b))
	if _, fs, _ := ReadPeerMap(dir, win); codes(fs) != contract.ClusterTimeUnknown {
		t.Errorf("untimed peer map: %s", codes(fs))
	}
}

func TestImageDigest(t *testing.T) {
	for in, want := range map[string]string{
		"docker-pullable://reg/b@" + digest('a'): digest('a'), "reg/b@" + digest('c'): digest('c'),
		digest('d'): digest('d'), "reg/b:1": "", "sha256:abc": "",
	} {
		if got := ImageDigest(in); got != want {
			t.Errorf("ImageDigest(%q) = %q", in, got)
		}
	}
}
