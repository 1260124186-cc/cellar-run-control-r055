package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

const (
	stateFileName       = "state.json"
	reservationFileName = "reservations.json"
)

type Store struct {
	mu              sync.RWMutex
	path            string
	reservationPath string
	snapshot        domain.Snapshot
	reservations    domain.ReservationLedger
	now             func() time.Time
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	store := &Store{
		path:            filepath.Join(dataDir, stateFileName),
		reservationPath: filepath.Join(dataDir, reservationFileName),
		snapshot:        domain.EmptySnapshot(),
		reservations:    domain.EmptyReservationLedger(),
		now:             func() time.Time { return time.Now().UTC() },
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	if err := store.loadReservations(); err != nil {
		return nil, err
	}
	return store, nil
}

// readReservations loads the durable unique-key reservation ledger. A missing
// file is a normal first start.
func readReservations(path string) (domain.ReservationLedger, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.EmptyReservationLedger(), nil
	}
	if err != nil {
		return domain.ReservationLedger{}, fmt.Errorf("read reservations: %w", err)
	}
	var loaded domain.ReservationLedger
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return domain.ReservationLedger{}, fmt.Errorf("decode reservations: %w", err)
	}
	if loaded.Entries == nil {
		loaded.Entries = map[string]domain.Reservation{}
	}
	return loaded, nil
}

func writeReservations(path string, ledger domain.ReservationLedger) error {
	return writeSnapshot(path, ledger)
}

func (s *Store) loadReservations() error {
	ledger, err := readReservations(s.reservationPath)
	if err != nil {
		return err
	}
	s.reservations = ledger
	return nil
}

// Claim reserves a unique business key before a create request is validated.
// The reservation is written through before the caller continues so that a
// concurrent duplicate create cannot slip past the check.
func (s *Store) Claim(kind domain.ReservationKind, key, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		return nil
	}
	updated := s.reservations.Clone()
	if err := updated.Claim(kind, key, owner, s.now()); err != nil {
		return err
	}
	if err := writeReservations(s.reservationPath, updated); err != nil {
		return domain.NewError(domain.CodeStorage, err.Error())
	}
	s.reservations = updated
	return nil
}

// Commit confirms a reservation once the caller intends to keep it.
func (s *Store) Commit(kind domain.ReservationKind, key, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := s.reservations.Clone()
	updated.Commit(kind, key, owner)
	if err := writeReservations(s.reservationPath, updated); err != nil {
		return domain.NewError(domain.CodeStorage, err.Error())
	}
	s.reservations = updated
	return nil
}

// Release applies the ledger transition for an owner that is giving the key
// back. The returned ledger is kept in memory for the rest of this process.
func (s *Store) Release(kind domain.ReservationKind, key, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservations.Release(kind, key, owner)
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	var loaded domain.Snapshot
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	normalized, err := normalizeSnapshot(loaded)
	if err != nil {
		return err
	}
	s.snapshot = normalized
	return nil
}

func normalizeSnapshot(value domain.Snapshot) (domain.Snapshot, error) {
	if value.Revision < 0 {
		return domain.Snapshot{}, fmt.Errorf("state revision cannot be negative")
	}
	if value.Formulas == nil {
		value.Formulas = map[string]domain.Formula{}
	}
	if value.Vessels == nil {
		value.Vessels = map[string]domain.Vessel{}
	}
	if value.Runs == nil {
		value.Runs = map[string]domain.FermentationRun{}
	}
	if value.NameIndex == nil {
		value.NameIndex = map[string]string{}
	}
	if value.VesselIndex == nil {
		value.VesselIndex = map[string]string{}
	}
	if value.RunIndex == nil {
		value.RunIndex = map[string]string{}
	}
	if err := validateIndexes(value); err != nil {
		return domain.Snapshot{}, err
	}
	return value, nil
}

func validateIndexes(value domain.Snapshot) error {
	expectedNames := make(map[string]string, len(value.Formulas))
	for id, formula := range value.Formulas {
		key := domain.NameKey(formula.Name)
		if key == "" {
			return fmt.Errorf("formula %s has an empty normalized name", id)
		}
		if existing, found := expectedNames[key]; found && existing != id {
			return fmt.Errorf("formula name index contains a duplicate key")
		}
		expectedNames[key] = id
	}
	if err := compareIndexes("formula name", value.NameIndex, expectedNames); err != nil {
		return err
	}

	expectedVessels := make(map[string]string, len(value.Vessels))
	for id, vessel := range value.Vessels {
		if existing, found := expectedVessels[vessel.Code]; found && existing != id {
			return fmt.Errorf("vessel index contains a duplicate code")
		}
		expectedVessels[vessel.Code] = id
	}
	if err := compareIndexes("vessel code", value.VesselIndex, expectedVessels); err != nil {
		return err
	}

	expectedRuns := make(map[string]string, len(value.Runs))
	for id, run := range value.Runs {
		if existing, found := expectedRuns[run.Code]; found && existing != id {
			return fmt.Errorf("run index contains a duplicate code")
		}
		expectedRuns[run.Code] = id
	}
	return compareIndexes("run code", value.RunIndex, expectedRuns)
}

func compareIndexes(label string, actual, expected map[string]string) error {
	if len(actual) != len(expected) {
		return fmt.Errorf("%s index does not match its collection", label)
	}
	for key, expectedID := range expected {
		actualID, found := actual[key]
		if !found || actualID != expectedID {
			return fmt.Errorf("%s index does not match its collection", label)
		}
	}
	return nil
}

func (s *Store) Revision() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot.Revision
}

func (s *Store) View(visitor func(domain.Snapshot) error) error {
	s.mu.RLock()
	copied := s.snapshot.Clone()
	s.mu.RUnlock()
	return visitor(copied)
}

func (s *Store) Update(expected *int64, mutate func(*domain.Snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if expected != nil && *expected != s.snapshot.Revision {
		return domain.NewError(domain.CodeRevisionConflict,
			"the supplied revision does not match the current snapshot")
	}
	candidate := s.snapshot.Clone()
	if err := mutate(&candidate); err != nil {
		return err
	}
	candidate.Revision = s.snapshot.Revision + 1
	candidate.UpdatedAt = s.now()
	if err := writeSnapshot(s.path, candidate); err != nil {
		return domain.NewError(domain.CodeStorage, err.Error())
	}
	s.snapshot = candidate
	return nil
}

func (s *Store) Execute(mutator func(*domain.Snapshot) error) error {
	return s.Update(nil, mutator)
}
