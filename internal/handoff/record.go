package handoff

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const RecordVersion = 1

type SourceState string

const (
	SourceQueued          SourceState = "queued"
	SourcePreparingRemote SourceState = "preparing_remote"
	SourceRemotePrepared  SourceState = "remote_prepared"
	SourceLaunchingRemote SourceState = "launching_remote"
	SourceAccepted        SourceState = "accepted"
	SourceRetryWait       SourceState = "retry_wait"
	SourceLaunchRetryWait SourceState = "launch_retry_wait"
	SourceFailed          SourceState = "failed"
)

type TargetState string

const (
	TargetReceived            TargetState = "received"
	TargetValidating          TargetState = "validating"
	TargetPrepared            TargetState = "prepared"
	TargetWaitingAssets       TargetState = "waiting_assets"
	TargetLaunching           TargetState = "launching"
	TargetLaunchWaitingAssets TargetState = "launch_waiting_assets"
	TargetAccepted            TargetState = "accepted"
	TargetRejected            TargetState = "rejected"
)

type FailureCode string

const (
	FailureUnavailable     FailureCode = "unavailable"
	FailureUnauthorized    FailureCode = "unauthorized"
	FailureInvalidManifest FailureCode = "invalid_manifest"
	FailureConflict        FailureCode = "conflict"
	FailureMissingAsset    FailureCode = "missing_asset"
	FailureVerification    FailureCode = "verification_failed"
	FailureLaunch          FailureCode = "launch_failed"
	FailureInternal        FailureCode = "internal"
)

// Failure is deliberately typed and bounded so records do not become an
// unbounded log or an accidental sink for remote responses.
type Failure struct {
	Code      FailureCode `json:"code"`
	Message   string      `json:"message"`
	Retryable bool        `json:"retryable"`
	At        time.Time   `json:"at"`
}

type TargetLocator struct {
	DeviceID     string `json:"device_id"`
	ProfileScope string `json:"profile_scope"`
	SessionID    string `json:"session_id"`
}

type SourceRecord struct {
	Version       int            `json:"version"`
	Manifest      Manifest       `json:"manifest"`
	Digest        string         `json:"digest"`
	State         SourceState    `json:"state"`
	Attempts      uint32         `json:"attempts"`
	NextRetry     *time.Time     `json:"next_retry"`
	Failure       *Failure       `json:"failure"`
	TargetLocator *TargetLocator `json:"target_locator"`
	Revision      uint64         `json:"revision"`
	Updated       time.Time      `json:"updated"`
}

type TargetRecord struct {
	Version       int            `json:"version"`
	Manifest      Manifest       `json:"manifest"`
	Digest        string         `json:"digest"`
	State         TargetState    `json:"state"`
	Attempts      uint32         `json:"attempts"`
	NextRetry     *time.Time     `json:"next_retry"`
	Failure       *Failure       `json:"failure"`
	TargetLocator *TargetLocator `json:"target_locator"`
	Revision      uint64         `json:"revision"`
	Updated       time.Time      `json:"updated"`
}

// Transition carries local state produced by one state-machine step. At may be
// zero, in which case Store uses its clock. Other fields replace the record's
// prior retry, failure, and target-locator values according to the next state.
type Transition struct {
	At            time.Time
	NextRetry     *time.Time
	Failure       *Failure
	TargetLocator *TargetLocator
}

func (r SourceRecord) validate() error {
	if r.Version != RecordVersion {
		return fmt.Errorf("source record version %d is unsupported", r.Version)
	}
	if err := validateStoredManifest(r.Manifest, r.Digest); err != nil {
		return err
	}
	if !validSourceState(r.State) {
		return fmt.Errorf("invalid source state %q", r.State)
	}
	return validateRecordFields(r.Manifest.Target, string(r.State), r.NextRetry, r.Failure, r.TargetLocator, r.Revision, r.Updated,
		r.State == SourceRetryWait || r.State == SourceLaunchRetryWait,
		r.State == SourceFailed, r.State == SourceAccepted, r.State == SourceAccepted)
}

func (r TargetRecord) validate() error {
	if r.Version != RecordVersion {
		return fmt.Errorf("target record version %d is unsupported", r.Version)
	}
	if err := validateStoredManifest(r.Manifest, r.Digest); err != nil {
		return err
	}
	if !validTargetState(r.State) {
		return fmt.Errorf("invalid target state %q", r.State)
	}
	return validateRecordFields(r.Manifest.Target, string(r.State), r.NextRetry, r.Failure, r.TargetLocator, r.Revision, r.Updated,
		r.State == TargetWaitingAssets || r.State == TargetLaunchWaitingAssets, r.State == TargetRejected,
		r.State == TargetLaunching || r.State == TargetAccepted, r.State == TargetAccepted)
}

func validateStoredManifest(manifest Manifest, digest string) error {
	got, err := manifest.Digest()
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if digest != got {
		return errors.New("stored manifest digest mismatch")
	}
	return nil
}

func validateRecordFields(target TargetAgent, state string, nextRetry *time.Time, failure *Failure, locator *TargetLocator, revision uint64, updated time.Time, retry, terminalFailure, locatorAllowed, locatorRequired bool) error {
	if revision == 0 {
		return errors.New("record revision must be positive")
	}
	if updated.IsZero() || updated.Location() != time.UTC {
		return errors.New("record updated must be a non-zero UTC timestamp")
	}
	if retry {
		if nextRetry == nil || !nextRetry.After(updated) || nextRetry.Location() != time.UTC {
			return errors.New("retry state requires a later UTC next_retry")
		}
		if failure == nil || !failure.Retryable {
			return errors.New("retry state requires a retryable failure")
		}
	} else if nextRetry != nil {
		return fmt.Errorf("state %s must not have next_retry", state)
	}
	if terminalFailure && failure == nil {
		return fmt.Errorf("state %s requires a failure", state)
	}
	if !retry && !terminalFailure && failure != nil {
		return fmt.Errorf("state %s must not have a failure", state)
	}
	if failure != nil {
		if err := failure.validate(); err != nil {
			return err
		}
		if failure.At.After(updated) {
			return errors.New("failure timestamp is after record update")
		}
	}
	if locatorRequired && locator == nil {
		return fmt.Errorf("state %s requires a target locator", state)
	}
	if locator != nil {
		if !locatorAllowed {
			return fmt.Errorf("state %s must not have a target locator", state)
		}
		if err := locator.validate(); err != nil {
			return err
		}
		if locator.DeviceID != target.DeviceID {
			return errors.New("target locator device does not match manifest target")
		}
	}
	return nil
}

func (f Failure) validate() error {
	switch f.Code {
	case FailureUnavailable, FailureUnauthorized, FailureInvalidManifest, FailureConflict, FailureMissingAsset, FailureVerification, FailureLaunch, FailureInternal:
	default:
		return fmt.Errorf("invalid failure code %q", f.Code)
	}
	if len([]rune(f.Message)) > 256 || strings.ContainsAny(f.Message, "\r\n\x00") {
		return errors.New("failure detail must be at most 256 runes on one line")
	}
	if f.At.IsZero() || f.At.Location() != time.UTC {
		return errors.New("failure timestamp must be non-zero UTC")
	}
	return nil
}

func (l TargetLocator) validate() error {
	if err := validateID("target locator device_id", l.DeviceID); err != nil {
		return err
	}
	if !profileScope.MatchString(l.ProfileScope) {
		return fmt.Errorf("invalid target locator profile_scope %q", l.ProfileScope)
	}
	return validateID("target locator session_id", l.SessionID)
}

func validSourceState(state SourceState) bool {
	switch state {
	case SourceQueued, SourcePreparingRemote, SourceRemotePrepared, SourceLaunchingRemote, SourceAccepted, SourceRetryWait, SourceLaunchRetryWait, SourceFailed:
		return true
	default:
		return false
	}
}

func validTargetState(state TargetState) bool {
	switch state {
	case TargetReceived, TargetValidating, TargetPrepared, TargetWaitingAssets, TargetLaunching, TargetLaunchWaitingAssets, TargetAccepted, TargetRejected:
		return true
	default:
		return false
	}
}

func legalSourceTransition(from, to SourceState) bool {
	switch from {
	case SourceQueued:
		return to == SourcePreparingRemote || to == SourceFailed
	case SourcePreparingRemote:
		return to == SourceRemotePrepared || to == SourceRetryWait || to == SourceFailed
	case SourceRemotePrepared:
		return to == SourceLaunchingRemote || to == SourceRetryWait || to == SourceFailed
	case SourceLaunchingRemote:
		return to == SourceAccepted || to == SourceLaunchRetryWait || to == SourceFailed
	case SourceRetryWait:
		return to == SourcePreparingRemote || to == SourceFailed
	case SourceLaunchRetryWait:
		return to == SourceLaunchingRemote || to == SourceFailed
	default:
		return false
	}
}

func legalTargetTransition(from, to TargetState) bool {
	switch from {
	case TargetReceived:
		return to == TargetValidating || to == TargetRejected
	case TargetValidating:
		return to == TargetPrepared || to == TargetWaitingAssets || to == TargetRejected
	case TargetWaitingAssets:
		return to == TargetValidating || to == TargetRejected
	case TargetPrepared:
		return to == TargetLaunching || to == TargetRejected
	case TargetLaunching:
		return to == TargetAccepted || to == TargetLaunchWaitingAssets || to == TargetRejected
	case TargetLaunchWaitingAssets:
		return to == TargetLaunching || to == TargetRejected
	default:
		return false
	}
}
