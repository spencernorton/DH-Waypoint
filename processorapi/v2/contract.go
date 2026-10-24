// Package v2 opts processors into complete SHA-256 torrent identifiers. The
// job/result envelope and field names remain those of v1; input.info_hash is a
// canonical complete 40-hex SHA-1 or 64-hex SHA-256. A hybrid retains its SHA-1
// canonical key. Its full identity pair is available from the catalogue v2 API.
// Existing v1 validators and strict decoders retain their original semantics.
package v2

import (
	"errors"
	"regexp"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

const Contract = "dh-waypoint.processor.v2"
const FeedbackContract = "dh-waypoint.processors.feedback.v2"
const JobExportContract = "dh-waypoint.processors.job-export.v2"

var fullHash = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

type Job v1.Job
type Result v1.Result
type Manifest v1.Manifest
type ServiceStatus v1.ServiceStatus

type Identity = v1.Identity
type Stage = v1.Stage
type Input = v1.Input
type Proposal = v1.Proposal
type FeedbackBatch = v1.FeedbackBatch
type FeedbackAck = v1.FeedbackAck
type FeedbackEvent = v1.FeedbackEvent
type FeedbackPage = v1.FeedbackPage
type JobExport = v1.JobExport

const Classify = v1.Classify
const Extract = v1.Extract
const Match = v1.Match
const Language = v1.Language
const Policy = v1.Policy
const Embed = v1.Embed

func (m Manifest) Validate() error           { return ValidateManifest(m) }
func (s ServiceStatus) Validate() error      { return ValidateStatus(s) }
func (j Job) Validate() error                { return ValidateJob(j) }
func (r Result) Validate(j Job) error        { return ValidateResult(r, j) }
func (j Job) EffectiveCapabilities() []Stage { return v1.Job(j).EffectiveCapabilities() }
func (r *Result) FillDefaultBinding(j Job) {
	out := v1.Result(*r)
	out.FillDefaultBinding(v1.Job(j))
	*r = Result(out)
}

func ValidateManifest(m Manifest) error {
	if m.Contract != Contract {
		return errors.New("unsupported_contract")
	}
	m.Contract = v1.Contract
	return v1.Manifest(m).Validate()
}

func ValidateStatus(s ServiceStatus) error {
	if s.Contract != Contract {
		return errors.New("unsupported_contract")
	}
	s.Contract = v1.Contract
	return v1.ServiceStatus(s).Validate()
}

func ValidateJob(j Job) error {
	if j.Contract != Contract || !fullHash.MatchString(j.Input.InfoHash) {
		return errors.New("invalid_v2_identity_or_contract")
	}
	// Reuse every v1 resource, proposal and binding check. This normalization
	// is validation-only and never leaves this function or reaches the wire.
	j.Contract = v1.Contract
	j.Input.InfoHash = j.Input.InfoHash[:40]
	return v1.Job(j).Validate()
}

func ValidateResult(r Result, j Job) error {
	if err := ValidateJob(j); err != nil {
		return err
	}
	if r.Contract != Contract {
		return errors.New("invalid_v2_result_contract")
	}
	j.Contract, r.Contract = v1.Contract, v1.Contract
	return v1.Result(r).Validate(v1.Job(j))
}
