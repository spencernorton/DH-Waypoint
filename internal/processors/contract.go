package processors

import (
	"encoding/hex"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	v2 "github.com/spencernorton/DH-Waypoint/processorapi/v2"
)

func mustDecodeHash(s string) []byte { b, _ := hex.DecodeString(s); return b }

func validateManifest(m v1.Manifest) error {
	if m.Contract == v2.Contract {
		return v2.Manifest(m).Validate()
	}
	return m.Validate()
}
func validateStatus(s v1.ServiceStatus) error {
	if s.Contract == v2.Contract {
		return v2.ServiceStatus(s).Validate()
	}
	return s.Validate()
}
func validateJob(j v1.Job) error {
	if j.Contract == v2.Contract {
		return v2.Job(j).Validate()
	}
	return j.Validate()
}
func validateResult(r v1.Result, j v1.Job) error {
	if j.Contract == v2.Contract {
		return v2.Result(r).Validate(v2.Job(j))
	}
	return r.Validate(j)
}

// Compatibility is an explicit property of a registered manifest. Legacy
// services never receive a full v2 identifier or a new JSON field by accident.
func planForIdentity(p Plan, hash []byte) Plan {
	if len(hash) == 20 {
		return p
	}
	out := p
	out.Steps = nil
	for _, step := range p.Steps {
		if step.Entry.Manifest.Contract == v2.Contract {
			out.Steps = append(out.Steps, step)
		}
	}
	return out
}
