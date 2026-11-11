// Package configbundle defines the sealed envelope used for trusted native
// configuration packages. It never executes the contents.
package configbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
)

var LegacyMarker = strings.ToUpper(brand.ID()) + "-LEGACY.json"
var legacyEnvelope = brand.ID() + "-legacy-v1:"

// Capability is announced only by agents that protect native bundle files and
// verify their content during approved rollback.
const Capability = "native-config-v1"

// LegacyPolicy is the part of the frozen marker file that agents enforce. It is
// covered by the bundle digest, so it carries the two administrators' approval.
type LegacyPolicy struct {
	// AllowRootMaster must be declared at submission before a native bundle may
	// be loaded by a master process that runs as root: native scripts are not
	// sandboxed and would otherwise silently gain full control of the node.
	AllowRootMaster bool `json:"allow_root_master"`
}

// ParseLegacyPolicy reads the policy from the marker file contents. Unknown
// fields are ignored; a malformed marker is an error, never a permissive default.
func ParseLegacyPolicy(marker []byte) (LegacyPolicy, error) {
	var p LegacyPolicy
	if err := json.Unmarshal(marker, &p); err != nil {
		return LegacyPolicy{}, errors.New("native configuration marker is malformed")
	}
	return p, nil
}

func IsLegacy(data []byte) bool { return bytes.HasPrefix(data, []byte(legacyEnvelope)) }

func SealLegacy(box *secretbox.Box, data []byte) ([]byte, error) {
	if box == nil {
		return nil, errors.New("legacy configuration encryption key is unavailable")
	}
	sealed, err := box.Seal(string(data))
	if err != nil {
		return nil, err
	}
	return []byte(legacyEnvelope + sealed), nil
}

func Open(box *secretbox.Box, data []byte) ([]byte, error) {
	if !IsLegacy(data) {
		return data, nil
	}
	if box == nil {
		return nil, errors.New("legacy configuration encryption key is unavailable")
	}
	plain, err := box.Open(string(data[len(legacyEnvelope):]))
	if err != nil {
		return nil, err
	}
	return []byte(plain), nil
}
