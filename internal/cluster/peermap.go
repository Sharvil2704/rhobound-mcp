// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// PeerMapSchema is the schema id of an ECS peer map.
const PeerMapSchema = "rhobound.peer-map/v1"

// PeerMap is the ECS peer map (rhobound.peer-map/v1, DATA.md 5): the addresses or DNS names CLIENT spans give, mapped
// to the service that answers there; desired task counts and instance types by service.
type PeerMap struct {
	Schema       string            `json:"schema"`
	TakenAt      *time.Time        `json:"taken_at,omitempty"`
	Addresses    map[string]string `json:"addresses"`
	DesiredCount map[string]int    `json:"desired_count,omitempty"`
	InstanceType map[string]string `json:"instance_type,omitempty"`
}

// ParsePeerMap reads a peer map and checks it against rhobound.peer-map/v1.
func ParsePeerMap(b []byte) (*PeerMap, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m PeerMap
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	if m.Schema != PeerMapSchema {
		return nil, fmt.Errorf("schema %q, want %q", m.Schema, PeerMapSchema)
	}
	if len(m.Addresses) == 0 {
		return nil, errors.New("no addresses")
	}
	for a, s := range m.Addresses {
		if a == "" || s == "" {
			return nil, errors.New("an empty address or service name")
		}
	}
	for s, n := range m.DesiredCount {
		if s == "" || n < 0 {
			return nil, errors.New("a desired count must name a service and not be negative")
		}
	}
	return &m, nil
}

// ReadPeerMap reads CAPTURE/peer-map.json when there is one (nil, and no findings, when there is none). A file that
// does not match the schema is cluster.peer_map_invalid; one taken more than MaxSkew outside the window is
// cluster.window_mismatch, one with no taken_at cluster.time_unknown.
func ReadPeerMap(dir string, win *Window) (*PeerMap, []contract.Finding, error) {
	p := filepath.Join(dir, "peer-map.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	m, err := ParsePeerMap(b)
	if err != nil {
		return nil, []contract.Finding{contract.NewFinding(contract.ClusterPeerMapInvalid).WithSubject("peer-map.json")}, nil
	}
	var fs []contract.Finding
	switch at := m.TakenAt; {
	case at == nil:
		fs = append(fs, contract.NewFinding(contract.ClusterTimeUnknown).WithSubject("peer-map.json"))
	case win != nil && (at.Before(win.Start.Add(-MaxSkew)) || at.After(win.End.Add(MaxSkew))):
		fs = append(fs, contract.NewFinding(contract.ClusterWindowMismatch).WithSubject("peer-map.json"))
	}
	return m, fs, nil
}

// PeerNames returns the peer map's addresses: address -> service.
func (m *PeerMap) PeerNames() map[string]string {
	out := make(map[string]string, len(m.Addresses))
	for a, s := range m.Addresses {
		out[a] = s
	}
	return out
}
