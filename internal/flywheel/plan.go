package flywheel

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"

	"github.com/joellarson/togi/internal/finding"
)

// BatchStatus records a batch's progress through the serial flywheel.
type BatchStatus string

const (
	BatchPending BatchStatus = "pending"
	BatchRunning BatchStatus = "running"
	BatchDone    BatchStatus = "done"
	BatchStuck   BatchStatus = "stuck"
)

// Attempt records the durable outcome of one agent invocation.
type Attempt struct {
	Number       int      `json:"number"`
	Status       string   `json:"status"`
	Failure      string   `json:"failure,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Commit       string   `json:"commit,omitempty"`
}

// Batch is one primary-file fix unit.
type Batch struct {
	ID          string            `json:"id"`
	PrimaryFile string            `json:"primary_file"`
	Findings    []finding.Finding `json:"findings"`
	Status      BatchStatus       `json:"status"`
	Attempts    []Attempt         `json:"attempts"`
	identity    batchIdentity
	proof       BatchProof
}

type batchIdentity struct {
	waveDigest string
	proof      [sha256.Size]byte
}

// Plan is the deterministic, persisted action plan consumed by the flywheel.
type Plan struct {
	SchemaVersion int     `json:"schema_version"`
	Batches       []Batch `json:"batches"`
}

// NewPlan groups canonical blocking findings by their primary file.
func NewPlan(findings []finding.Finding) (Plan, error) {
	ordered, err := canonicalFindings(findings)
	if err != nil {
		return Plan{}, err
	}
	waveDigest := blockerWaveDigest(ordered)

	plan := Plan{SchemaVersion: 1, Batches: make([]Batch, 0)}
	for _, item := range ordered {
		last := len(plan.Batches) - 1
		if last < 0 || plan.Batches[last].PrimaryFile != item.File {
			plan.Batches = append(plan.Batches, Batch{
				ID:          batchID(item.File, waveDigest),
				PrimaryFile: item.File,
				Findings:    make([]finding.Finding, 0, 1),
				Status:      BatchPending,
				Attempts:    make([]Attempt, 0),
			})
			last++
		}
		plan.Batches[last].Findings = append(plan.Batches[last].Findings, cloneFinding(item))
	}
	for index := range plan.Batches {
		batch := &plan.Batches[index]
		batch.identity = batchIdentity{
			waveDigest: waveDigest,
			proof:      batchIdentityProof(batch.ID, batch.PrimaryFile, waveDigest, batch.Findings),
		}
	}
	return plan, nil
}

func canonicalFindings(findings []finding.Finding) ([]finding.Finding, error) {
	ordered := make([]finding.Finding, len(findings))
	seen := make(map[string]struct{}, len(findings))
	for index, item := range findings {
		if item.Fingerprint == "" {
			return nil, fmt.Errorf("finding %d: canonical fingerprint is required", index)
		}
		grouped, err := finding.Group([]finding.Finding{item})
		if err != nil {
			return nil, fmt.Errorf("finding %d: %w", index, err)
		}
		if len(grouped) == 1 && len(grouped[0].Occurrences) == 0 && len(item.Occurrences) == 0 {
			grouped[0].Occurrences = nil
			item.Occurrences = nil
		}
		if len(grouped) != 1 || !reflect.DeepEqual(grouped[0], item) {
			return nil, fmt.Errorf("finding %d is not canonically grouped", index)
		}
		if _, exists := seen[item.Fingerprint]; exists {
			return nil, fmt.Errorf("finding %d duplicates grouped fingerprint %q", index, item.Fingerprint)
		}
		seen[item.Fingerprint] = struct{}{}
		ordered[index] = cloneFinding(item)
	}

	sort.Slice(ordered, func(left, right int) bool {
		return lessPlanFinding(ordered[left], ordered[right])
	})

	return ordered, nil
}

// BlockingMultiset counts blocking occurrences by stable fingerprint.
func BlockingMultiset(findings []finding.Finding) (map[string]int, error) {
	canonical, err := canonicalFindings(findings)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int, len(canonical))
	for _, item := range canonical {
		result[item.Fingerprint] += 1 + len(item.Occurrences)
	}
	return result, nil
}

// StrictlyShrinks reports whether after is a strict multiset subset of before.
func StrictlyShrinks(after, before map[string]int) bool {
	strict := false
	for fingerprint, count := range after {
		if count <= 0 || count > before[fingerprint] {
			return false
		}
		if count < before[fingerprint] {
			strict = true
		}
	}
	for fingerprint, count := range before {
		if count <= 0 {
			return false
		}
		if _, exists := after[fingerprint]; !exists {
			strict = true
		}
	}
	return strict
}

func cloneFinding(item finding.Finding) finding.Finding {
	item.Occurrences = append([]finding.Occurrence(nil), item.Occurrences...)
	return item
}

func cloneFindings(items []finding.Finding) []finding.Finding {
	if items == nil {
		return nil
	}
	cloned := make([]finding.Finding, len(items))
	for index, item := range items {
		cloned[index] = cloneFinding(item)
	}
	return cloned
}

func clonePlan(plan Plan) Plan {
	cloned := Plan{SchemaVersion: plan.SchemaVersion}
	if plan.Batches == nil {
		return cloned
	}
	cloned.Batches = make([]Batch, len(plan.Batches))
	for index, batch := range plan.Batches {
		batch.Findings = cloneFindings(batch.Findings)
		batch.Attempts = append([]Attempt(nil), batch.Attempts...)
		for attempt := range batch.Attempts {
			batch.Attempts[attempt].ChangedFiles = append([]string(nil), batch.Attempts[attempt].ChangedFiles...)
		}
		cloned.Batches[index] = batch
	}
	return cloned
}

func cloneBatch(batch Batch) Batch {
	batch.Findings = cloneFindings(batch.Findings)
	batch.Attempts = append([]Attempt(nil), batch.Attempts...)
	for attempt := range batch.Attempts {
		batch.Attempts[attempt].ChangedFiles = append([]string(nil), batch.Attempts[attempt].ChangedFiles...)
	}
	batch.proof = cloneBatchProof(batch.proof)
	return batch
}

func lessPlanFinding(left, right finding.Finding) bool {
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	if left.Gate != right.Gate {
		return left.Gate < right.Gate
	}
	if left.RuleID != right.RuleID {
		return left.RuleID < right.RuleID
	}
	return left.Fingerprint < right.Fingerprint
}

func blockerWaveDigest(findings []finding.Finding) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("togi/fix-wave-identity/v1\x00"))
	var encoded [8]byte
	for _, item := range findings {
		binary.BigEndian.PutUint64(encoded[:], uint64(len(item.Fingerprint)))
		_, _ = hash.Write(encoded[:])
		_, _ = hash.Write([]byte(item.Fingerprint))
		binary.BigEndian.PutUint64(encoded[:], uint64(1+len(item.Occurrences)))
		_, _ = hash.Write(encoded[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func batchID(primaryFile, waveDigest string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("togi/fix-batch-identity/v1\x00"))
	var length [8]byte
	for _, value := range []string{primaryFile, waveDigest} {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return "batch-" + hex.EncodeToString(hash.Sum(nil))
}

func batchIdentityProof(id, primaryFile, waveDigest string, findings []finding.Finding) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("togi/validated-fix-batch/v1\x00"))
	writeString := func(value string) {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(len(value)))
		_, _ = hash.Write(encoded[:])
		_, _ = hash.Write([]byte(value))
	}
	writeInt := func(value int) {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(value))
		_, _ = hash.Write(encoded[:])
	}
	writeString(id)
	writeString(primaryFile)
	writeString(waveDigest)
	writeInt(len(findings))
	for _, item := range findings {
		for _, value := range []string{
			item.Gate, item.Language, item.RuleID, string(item.Severity), item.File,
			item.Snippet, item.Message, item.Fingerprint,
		} {
			writeString(value)
		}
		writeInt(item.Line)
		writeInt(item.EndLine)
		writeInt(len(item.Occurrences))
		for _, occurrence := range item.Occurrences {
			writeInt(occurrence.Line)
			writeInt(occurrence.EndLine)
		}
	}
	var proof [sha256.Size]byte
	copy(proof[:], hash.Sum(nil))
	return proof
}
