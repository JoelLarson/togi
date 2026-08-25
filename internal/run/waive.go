package run

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/joellarson/togi/internal/waiver"
)

// waiverTimeLayout keeps an approval time readable in the confirmation the
// operator sees; the persisted record keeps full precision.
const waiverTimeLayout = "2006-01-02T15:04:05Z"

// Waive records an operator's approval of one fingerprint for the repository
// containing root. The approval is durable and shared by every checkout of
// that repository, because it is keyed by repository identity rather than by
// working directory.
func (service Service) Waive(ctx context.Context, root, fingerprint, reason string) (waiver.Record, error) {
	if err := service.checkPlatform(); err != nil {
		return waiver.Record{}, err
	}
	if err := service.validateStateAccess(); err != nil {
		return waiver.Record{}, err
	}
	if ctx == nil {
		return waiver.Record{}, errors.New("waive context is required")
	}
	repository, err := service.resolveRepository(ctx, root)
	if err != nil {
		return waiver.Record{}, err
	}
	repoState := service.Paths.RepoState(repository)
	if err := validateExternalRepoState(repository.Root(), repoState); err != nil {
		return waiver.Record{}, err
	}
	record, created, err := (waiver.Store{Dir: repoState, Now: service.Now}).Approve(fingerprint, reason)
	if err != nil {
		return waiver.Record{}, fmt.Errorf("record waiver: %w", err)
	}
	if err := renderWaiver(service.Stdout, record, created); err != nil {
		return waiver.Record{}, err
	}
	return record, nil
}

func renderWaiver(output io.Writer, record waiver.Record, created bool) error {
	if output == nil {
		return errors.New("waiver output is required")
	}
	headline := "waived"
	if !created {
		headline = "already waived"
	}
	for _, line := range []string{
		headline + " " + record.Fingerprint,
		"reason: " + safeText(record.Reason),
		"approved: " + record.ApprovedAt.Format(waiverTimeLayout),
	} {
		if _, err := fmt.Fprintln(output, line); err != nil {
			return fmt.Errorf("render waiver: %w", err)
		}
	}
	return nil
}
