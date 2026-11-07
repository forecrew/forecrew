package vmcommon

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

// markerV1 starts the notes of every VM Forecrew made. List returns only VMs
// whose notes parse as a Meta, so nothing else on the hypervisor is ever listed,
// destroyed or changed.
const markerV1 = "forecrew-machine/1"

// Bounds on what a hypervisor hands back: its notes may have been edited by
// anyone who can reach it, so they are data, read strictly.
const (
	maxMetaBytes = 8192
	maxTags      = 32
	maxTagText   = 256
	maxList      = 256
)

// Meta is what a provider keeps with a VM, in the hypervisor's notes field. It
// holds no secrets: the bootstrap token never goes here.
type Meta struct {
	Marker    string            `json:"forecrew"`
	MachineID string            `json:"machine"`
	Tags      map[string]string `json:"tags,omitempty"`
	// Allow is the location's network_allow as given; Server and Port say where
	// forecrewd is, as the host resolved it, so a changed allowlist can be
	// applied later without resolving again.
	Allow  []string `json:"allow,omitempty"`
	Server []string `json:"server,omitempty"`
	Port   int      `json:"port,omitempty"`
	// Bridge is the host's network interface the machine is on (a Proxmox
	// bridge, a Hyper-V switch's vEthernet adapter), so that the networks that
	// hold the other machines are blocked again when the rules are rebuilt.
	Bridge   string    `json:"bridge,omitempty"`
	Deadline time.Time `json:"deadline,omitzero"`
}

// NewMeta returns the meta of a machine.
func NewMeta(machineID uuid.UUID) Meta {
	return Meta{Marker: markerV1, MachineID: machineID.String()}
}

// Encode returns the text for the notes field.
func (m Meta) Encode() string {
	m.Marker = markerV1
	b, _ := json.Marshal(m) // plain strings and numbers: cannot fail
	return string(b)
}

// ParseMeta reads notes written by Encode. It reports false for anything else:
// a VM an operator made, or notes that were changed to something unexpected.
func ParseMeta(notes string) (Meta, bool) {
	notes = strings.TrimSpace(notes)
	if len(notes) == 0 || len(notes) > maxMetaBytes || !utf8.ValidString(notes) {
		return Meta{}, false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(notes)))
	dec.DisallowUnknownFields()
	var m Meta
	if err := dec.Decode(&m); err != nil || dec.More() {
		return Meta{}, false
	}
	if m.Marker != markerV1 {
		return Meta{}, false
	}
	if _, err := uuid.Parse(m.MachineID); err != nil {
		return Meta{}, false
	}
	if len(m.Tags) > maxTags || len(m.Allow) > maxList || len(m.Server) > maxList {
		return Meta{}, false
	}
	for k, v := range m.Tags {
		if len(k) > maxTagText || len(v) > maxTagText {
			return Meta{}, false
		}
	}
	return m, true
}

// ID returns the machine's ID; ParseMeta checked it.
func (m Meta) ID() uuid.UUID {
	id, _ := uuid.Parse(m.MachineID)
	return id
}
