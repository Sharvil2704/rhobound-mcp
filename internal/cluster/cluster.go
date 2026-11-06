// SPDX-License-Identifier: Apache-2.0

// Package cluster reads the optional cluster dump of a capture (docs/mcp/DATA.md 5): Kubernetes List objects in
// CAPTURE/cluster/start/ and CAPTURE/cluster/end/, or one flat snapshot in CAPTURE/cluster/, and the ECS peer map
// CAPTURE/peer-map.json. It names CLIENT peers given as a Service's ClusterIP, gives the environment record (instance
// types, architectures, running image digests), and checks the dump itself: that every file is a List, that it was
// scrubbed, and when it was taken. A capture without a cluster folder reads exactly as before.
package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// MaxSkew is how far outside the window a dump may have been taken [D] (cluster.window_mismatch).
const MaxSkew = time.Minute

// MaxFileBytes bounds one dump file; a larger file is refused as not a List.
const MaxFileBytes = 256 << 20

// Snapshot is one cluster dump: what rhobound reads of its Services, Deployments, Pods, Nodes and autoscalers.
type Snapshot struct {
	Files   []string   // the file names, relative to the capture folder, in the order read
	Digest  string     // sha256:<hex> over the files' names and contents
	TakenAt *time.Time // from the capture manifest; nil when it was not recorded

	Services    []Service
	Deployments []Deployment
	Pods        []Pod
	Nodes       []Node
	HPAs        []HPA
	Ignored     map[string]int // objects of kinds nothing reads, by kind
}

// Service is a Kubernetes Service: where it answers.
type Service struct {
	Name, Namespace string
	ClusterIP       string // "" for a headless Service
	Ports           []int
}

// Deployment is a Kubernetes Deployment: its replica count and container spec.
type Deployment struct {
	Name, Namespace string
	Replicas        *int
	Containers      []Container
}

// Container is one container of a pod template: its name, image and resources (as canonical JSON).
type Container struct {
	Name, Image, Resources string
}

// Pod is a running pod: when it was created, its node, and each container's running image and restarts.
type Pod struct {
	Name, Namespace string
	Created         time.Time
	Node            string
	Containers      []ContainerStatus
}

// ContainerStatus is one container's running image (its digest, sha256:<hex>, when imageID names one) and restarts.
type ContainerStatus struct {
	Name, ImageID, Digest string
	Restarts              int
}

// Node is a node's instance type, architecture and kernel.
type Node struct {
	Name, InstanceType, Arch, Kernel string
}

// HPA is an autoscaler's current and desired replica counts.
type HPA struct {
	Name, Namespace  string
	Current, Desired int
}

// Dump is the cluster dump of a capture: its layout, and the snapshots at the window's start and end. A flat folder is
// one snapshot, read as both.
type Dump struct {
	Layout     string // contract.LayoutStartEnd or contract.LayoutSingleSnapshot
	Start, End *Snapshot
}

// Window is the capture's window, against which the dumps' times are checked.
type Window struct{ Start, End time.Time }

// TakenAt holds the times the dumps were taken, as the capture manifest records them (either may be nil).
type TakenAt struct{ Start, End *time.Time }

// Read reads the cluster dump of the capture in dir. It returns nil, and no findings, when the capture has no cluster
// folder. Findings are the dump's own: cluster.layout_ambiguous, cluster.not_list, cluster.unscrubbed,
// cluster.window_mismatch, cluster.time_unknown and cluster.single_snapshot; with a refusal among them the dump is
// still returned as far as it was read. The error is an I/O error.
func Read(dir string, taken TakenAt, win *Window) (*Dump, []contract.Finding, error) {
	root := filepath.Join(dir, "cluster")
	if st, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	} else if !st.IsDir() {
		return nil, []contract.Finding{contract.NewFinding(contract.ClusterNotList).WithSubject("cluster")}, nil
	}
	flat, err := jsonFiles(root)
	if err != nil {
		return nil, nil, err
	}
	startDir, endDir := filepath.Join(root, "start"), filepath.Join(root, "end")
	hasStart, hasEnd := isDir(startDir), isDir(endDir)
	var fs []contract.Finding
	d := &Dump{}
	switch {
	case (hasStart || hasEnd) && len(flat) > 0:
		fs = append(fs, contract.NewFinding(contract.ClusterLayoutAmbiguous).WithNumber("flat_files", float64(len(flat))))
		return nil, fs, nil
	case hasStart || hasEnd:
		d.Layout = contract.LayoutStartEnd
		for _, side := range []struct {
			dir  string
			at   *time.Time
			into **Snapshot
		}{{startDir, taken.Start, &d.Start}, {endDir, taken.End, &d.End}} {
			if !isDir(side.dir) {
				continue
			}
			names, err := jsonFiles(side.dir)
			if err != nil {
				return nil, nil, err
			}
			s, f, err := readSnapshot(dir, side.dir, names, side.at, win)
			if err != nil {
				return nil, nil, err
			}
			*side.into = s
			fs = append(fs, f...)
		}
	case len(flat) > 0:
		d.Layout = contract.LayoutSingleSnapshot
		at := taken.Start
		if at == nil {
			at = taken.End
		}
		s, f, err := readSnapshot(dir, root, flat, at, win)
		if err != nil {
			return nil, nil, err
		}
		d.Start, d.End = s, s
		fs = append(fs, f...)
		fs = append(fs, contract.NewFinding(contract.ClusterSingleSnapshot))
	default:
		return nil, nil, nil // an empty folder: no dump
	}
	return d, fs, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// jsonFiles lists the files of a folder in name order; every file is a dump file (a file that is not JSON is refused
// by name, never skipped), folders are left for the caller.
func jsonFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// listObject is a Kubernetes List as kubectl get -o json writes it.
type listObject struct {
	Kind  string            `json:"kind"`
	Items []json.RawMessage `json:"items"`
}

func readSnapshot(capture, dir string, names []string, at *time.Time, win *Window) (*Snapshot, []contract.Finding, error) {
	s := &Snapshot{TakenAt: at, Ignored: map[string]int{}}
	var fs []contract.Finding
	var digestIn bytes.Buffer
	for _, n := range names {
		p := filepath.Join(dir, n)
		rel, _ := filepath.Rel(capture, p)
		rel = filepath.ToSlash(rel)
		s.Files = append(s.Files, rel)
		b, err := readCapped(p)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&digestIn, "%s\x00%s\n", rel, contract.SHA256Hex(b))
		var l listObject
		if b == nil || json.Unmarshal(b, &l) != nil || !(l.Kind == "List" || strings.HasSuffix(l.Kind, "List")) || l.Items == nil {
			fs = append(fs, contract.NewFinding(contract.ClusterNotList).WithSubject(rel))
			continue
		}
		for i, raw := range l.Items {
			if why := unscrubbed(raw); why != "" {
				fs = append(fs, contract.NewFinding(contract.ClusterUnscrubbed).WithSubject(rel).
					WithNumber("item", float64(i)).WithExamples(why))
				continue
			}
			s.add(raw)
		}
	}
	s.Digest = contract.Digest(digestIn.Bytes())
	switch {
	case at == nil:
		fs = append(fs, contract.NewFinding(contract.ClusterTimeUnknown).WithSubject(filepath.ToSlash(relDir(capture, dir))))
	case win != nil && (at.Before(win.Start.Add(-MaxSkew)) || at.After(win.End.Add(MaxSkew))):
		off := at.Sub(win.Start).Seconds()
		if at.After(win.End) {
			off = at.Sub(win.End).Seconds()
		}
		fs = append(fs, contract.NewFinding(contract.ClusterWindowMismatch).WithSubject(filepath.ToSlash(relDir(capture, dir))).
			WithNumber("seconds_outside", off))
	}
	return s, fs, nil
}

func relDir(capture, dir string) string {
	r, err := filepath.Rel(capture, dir)
	if err != nil {
		return "cluster"
	}
	return r
}

// readCapped reads a file of at most MaxFileBytes; a larger one reads as nil (not a List).
func readCapped(p string) ([]byte, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxFileBytes {
		return nil, nil
	}
	return os.ReadFile(p)
}

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// unscrubbed says why an object must not be in a dump: a Secret or a ConfigMap, managed fields, the last-applied
// annotation, or a container env value other than a non-secret OTEL_* key. It returns "" for a scrubbed object.
func unscrubbed(raw json.RawMessage) string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	switch k, _ := obj["kind"].(string); k {
	case "Secret", "ConfigMap":
		return "kind:" + k
	}
	var why string
	var walk func(v any, key string)
	walk = func(v any, key string) {
		if why != "" {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				switch {
				case k == "managedFields":
					if arr, ok := c.([]any); ok && len(arr) > 0 {
						why = "managedFields"
						return
					}
				case k == "annotations":
					if m, ok := c.(map[string]any); ok && m[lastApplied] != nil {
						why = "last-applied-configuration"
						return
					}
				case k == "env":
					if arr, ok := c.([]any); ok {
						for _, e := range arr {
							em, _ := e.(map[string]any)
							name, _ := em["name"].(string)
							if val, has := em["value"]; has && val != "" && !nonSecretOTEL(name) {
								why = "env:" + safeEnvName(name)
								return
							}
						}
					}
				}
				walk(c, k)
			}
		case []any:
			for _, c := range x {
				walk(c, key)
			}
		}
	}
	walk(obj, "")
	return why
}

// nonSecretOTEL reports whether an environment variable is an OpenTelemetry setting that carries no secret: OTEL_*
// except the header variables, which carry credentials.
func nonSecretOTEL(name string) bool {
	return strings.HasPrefix(name, "OTEL_") && !strings.Contains(name, "HEADERS")
}

func safeEnvName(n string) string {
	if s, untrusted := contract.SafeName(n); !untrusted {
		return s
	}
	return "(name)"
}

// The Kubernetes objects, as far as rhobound reads them.
type k8sMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
}

type k8sContainer struct {
	Name      string          `json:"name"`
	Image     string          `json:"image"`
	Resources json.RawMessage `json:"resources"`
}

type k8sObject struct {
	Kind     string  `json:"kind"`
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		ClusterIP string `json:"clusterIP"`
		Ports     []struct {
			Port int `json:"port"`
		} `json:"ports"`
		Replicas *int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []k8sContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses []struct {
			Name         string `json:"name"`
			ImageID      string `json:"imageID"`
			RestartCount int    `json:"restartCount"`
		} `json:"containerStatuses"`
		NodeInfo struct {
			KernelVersion string `json:"kernelVersion"`
			Architecture  string `json:"architecture"`
		} `json:"nodeInfo"`
		CurrentReplicas int `json:"currentReplicas"`
		DesiredReplicas int `json:"desiredReplicas"`
	} `json:"status"`
}

func (s *Snapshot) add(raw json.RawMessage) {
	var o k8sObject
	if json.Unmarshal(raw, &o) != nil {
		s.Ignored["unreadable"]++
		return
	}
	m := o.Metadata
	switch o.Kind {
	case "Service":
		sv := Service{Name: m.Name, Namespace: m.Namespace}
		if ip := o.Spec.ClusterIP; ip != "None" {
			sv.ClusterIP = ip
		}
		for _, p := range o.Spec.Ports {
			sv.Ports = append(sv.Ports, p.Port)
		}
		s.Services = append(s.Services, sv)
	case "Deployment":
		dp := Deployment{Name: m.Name, Namespace: m.Namespace, Replicas: o.Spec.Replicas}
		for _, c := range o.Spec.Template.Spec.Containers {
			res := ""
			if len(c.Resources) > 0 {
				if b, err := contract.CanonicalizeJSON(c.Resources); err == nil {
					res = string(b)
				}
			}
			dp.Containers = append(dp.Containers, Container{Name: c.Name, Image: c.Image, Resources: res})
		}
		s.Deployments = append(s.Deployments, dp)
	case "Pod":
		p := Pod{Name: m.Name, Namespace: m.Namespace, Node: o.Spec.NodeName}
		if t, err := time.Parse(time.RFC3339, m.CreationTimestamp); err == nil {
			p.Created = t
		}
		for _, c := range o.Status.ContainerStatuses {
			p.Containers = append(p.Containers, ContainerStatus{Name: c.Name, ImageID: c.ImageID, Digest: ImageDigest(c.ImageID),
				Restarts: c.RestartCount})
		}
		s.Pods = append(s.Pods, p)
	case "Node":
		n := Node{Name: m.Name, InstanceType: m.Labels["node.kubernetes.io/instance-type"], Arch: m.Labels["kubernetes.io/arch"],
			Kernel: o.Status.NodeInfo.KernelVersion}
		if n.Arch == "" {
			n.Arch = o.Status.NodeInfo.Architecture
		}
		s.Nodes = append(s.Nodes, n)
	case "HorizontalPodAutoscaler":
		s.HPAs = append(s.HPAs, HPA{Name: m.Name, Namespace: m.Namespace, Current: o.Status.CurrentReplicas,
			Desired: o.Status.DesiredReplicas})
	default:
		k := o.Kind
		if n, untrusted := contract.SafeName(k); untrusted || k == "" {
			k = "(other)"
		} else {
			k = n
		}
		s.Ignored[k]++
	}
}

var digestRE = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// ImageDigest returns the sha256:<hex> digest an image reference names (a pod's imageID such as
// docker-pullable://reg/repo@sha256:<hex>, or a repo_digests entry reg/repo@sha256:<hex>), or "".
func ImageDigest(ref string) string { return digestRE.FindString(ref) }

// snapshots returns the dump's distinct snapshots: start, then end.
func (d *Dump) snapshots() []*Snapshot {
	var out []*Snapshot
	if d == nil {
		return nil
	}
	if d.Start != nil {
		out = append(out, d.Start)
	}
	if d.End != nil && d.End != d.Start {
		out = append(out, d.End)
	}
	return out
}

// PeerNames maps the addresses a CLIENT span may give for a Service, "<ClusterIP>:<port>" and "<ClusterIP>", to the
// Service's name, from every snapshot (headless Services have no ClusterIP and are left out). When two Services claim
// one address, the first by name wins.
func (d *Dump) PeerNames() map[string]string {
	out := map[string]string{}
	for _, s := range d.snapshots() {
		svcs := append([]Service(nil), s.Services...)
		sort.SliceStable(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })
		for _, sv := range svcs {
			if sv.ClusterIP == "" {
				continue
			}
			if _, ok := out[sv.ClusterIP]; !ok {
				out[sv.ClusterIP] = sv.Name
			}
			for _, p := range sv.Ports {
				a := sv.ClusterIP + ":" + strconv.Itoa(p)
				if _, ok := out[a]; !ok {
					out[a] = sv.Name
				}
			}
		}
	}
	return out
}

// Environment is the environment record a dump gives: the nodes' instance types, architectures and kernels, and the
// running image digests by pod.
type Environment struct {
	Nodes     []Node              // by name
	PodNode   map[string]string   // pod name -> node name
	PodImages map[string][]string // pod name -> running image digests, sorted
}

// Environment returns the environment record of the dump's end snapshot (the start one when there is no end).
func (d *Dump) Environment() Environment {
	env := Environment{PodNode: map[string]string{}, PodImages: map[string][]string{}}
	s := d.End
	if s == nil {
		s = d.Start
	}
	if s == nil {
		return env
	}
	env.Nodes = append(env.Nodes, s.Nodes...)
	sort.Slice(env.Nodes, func(i, j int) bool { return env.Nodes[i].Name < env.Nodes[j].Name })
	for _, p := range s.Pods {
		env.PodNode[p.Name] = p.Node
		for _, c := range p.Containers {
			if c.Digest != "" && !contains(env.PodImages[p.Name], c.Digest) {
				env.PodImages[p.Name] = append(env.PodImages[p.Name], c.Digest)
			}
		}
		sort.Strings(env.PodImages[p.Name])
	}
	return env
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// CheckDigests compares the running image digests of the dump's pods with the image digests the spans name for the same
// pods (spanDigests: service -> pod name -> repo_digests entries). A pod whose running digests share none with the
// spans' is cluster.digest_disagrees, one finding per service.
func (d *Dump) CheckDigests(spanDigests map[string]map[string][]string) []contract.Finding {
	env := d.Environment()
	var out []contract.Finding
	svcs := make([]string, 0, len(spanDigests))
	for s := range spanDigests {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	for _, svc := range svcs {
		pods := spanDigests[svc]
		names := make([]string, 0, len(pods))
		for p := range pods {
			names = append(names, p)
		}
		sort.Strings(names)
		var bad []string
		for _, pod := range names {
			running := env.PodImages[pod]
			if len(running) == 0 || len(pods[pod]) == 0 {
				continue
			}
			match := false
			for _, ref := range pods[pod] {
				if contains(running, ImageDigest(ref)) {
					match = true
				}
			}
			if !match {
				bad = append(bad, pod)
			}
		}
		if len(bad) > 0 {
			out = append(out, contract.NewFinding(contract.ClusterDigestDisagrees).WithSubject(svc).
				WithNumber("pods", float64(len(bad))).WithExamples(bad...))
		}
	}
	return out
}

// Churn returns the pods that changed during the window by the dumps: present in only one of the start and end
// snapshots, created inside the window, or restarted between the snapshots. With a single snapshot only the creation
// times can tell.
func (d *Dump) Churn(win Window) []string {
	set := map[string]bool{}
	var start, end map[string]Pod
	index := func(s *Snapshot) map[string]Pod {
		m := map[string]Pod{}
		if s != nil {
			for _, p := range s.Pods {
				m[p.Namespace+"/"+p.Name] = p
			}
		}
		return m
	}
	start, end = index(d.Start), index(d.End)
	for k, p := range end {
		if !p.Created.IsZero() && p.Created.After(win.Start) && p.Created.Before(win.End) {
			set[p.Name] = true
		}
		if d.Start != d.End {
			q, ok := start[k]
			if !ok || restarts(q) != restarts(p) {
				set[p.Name] = true
			}
		}
	}
	if d.Start != d.End {
		for k, p := range start {
			if _, ok := end[k]; !ok {
				set[p.Name] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func restarts(p Pod) int {
	n := 0
	for _, c := range p.Containers {
		n += c.Restarts
	}
	return n
}
