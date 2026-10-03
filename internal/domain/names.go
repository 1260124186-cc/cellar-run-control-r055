package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	vesselCodePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{2,31}$`)
	runCodePattern    = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{3,47}$`)
)

func NewID(prefix string) (string, error) {
	buffer := make([]byte, 10)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(buffer), nil
}

func NormalizeName(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	return strings.Join(fields, " ")
}

func NameKey(value string) string {
	var builder strings.Builder
	for _, r := range NormalizeName(value) {
		builder.WriteRune(unicode.ToLower(r))
	}
	return builder.String()
}

func ValidateIdentifier(value, field string) error {
	if !identifierPattern.MatchString(value) {
		return FieldError(CodeInvalidInput, field,
			"must start with a lowercase letter and contain 3 to 64 lowercase letters, digits, or hyphens")
	}
	return nil
}

func NormalizeVesselCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func ValidateVesselCode(value, field string) error {
	if !vesselCodePattern.MatchString(value) {
		return FieldError(CodeInvalidInput, field,
			"must contain 3 to 32 uppercase letters, digits, or hyphens")
	}
	return nil
}

func NormalizeRunCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func ValidateRunCode(value, field string) error {
	if !runCodePattern.MatchString(value) {
		return FieldError(CodeInvalidInput, field,
			"must contain 4 to 48 uppercase letters, digits, or hyphens")
	}
	return nil
}

// ReservationKind identifies the business namespace a unique key belongs to.
type ReservationKind string

const (
	ReservationFormulaName ReservationKind = "formula_name"
	ReservationVesselCode  ReservationKind = "vessel_code"
	ReservationRunCode     ReservationKind = "run_code"
)

// ErrReservationHeld reports that a unique business key is already reserved.
var ErrReservationHeld = errors.New("unique business key is already reserved")

// Reservation is a claim on a unique name or code while a create request is
// being validated. Confirmed reservations are meant to survive a restart so a
// later request can reconcile an interrupted create.
type Reservation struct {
	Owner     string    `json:"owner"`
	Committed bool      `json:"committed"`
	CreatedAt time.Time `json:"created_at"`
}

type ReservationLedger struct {
	Entries map[string]Reservation `json:"entries"`
}

func EmptyReservationLedger() ReservationLedger {
	return ReservationLedger{Entries: map[string]Reservation{}}
}

func (l ReservationLedger) Clone() ReservationLedger {
	cloned := EmptyReservationLedger()
	for slot, entry := range l.Entries {
		cloned.Entries[slot] = entry
	}
	return cloned
}

func reservationSlot(kind ReservationKind, key string) string {
	return string(kind) + "|" + key
}

// Claim records a reservation for one entity namespace. A key that is already
// reserved, in any stage of its lifecycle, cannot be claimed again.
func (l *ReservationLedger) Claim(
	kind ReservationKind,
	key string,
	owner string,
	now time.Time,
) error {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	if l.Entries == nil {
		l.Entries = map[string]Reservation{}
	}
	slot := reservationSlot(kind, key)
	if _, held := l.Entries[slot]; held {
		return ErrReservationHeld
	}
	l.Entries[slot] = Reservation{Owner: owner, CreatedAt: now.UTC()}
	return nil
}

// Commit confirms a reservation that is owned by the requesting owner.
func (l *ReservationLedger) Commit(kind ReservationKind, key, owner string) {
	slot := reservationSlot(kind, key)
	entry, held := l.Entries[slot]
	if !held || entry.Owner != owner {
		return
	}
	entry.Committed = true
	l.Entries[slot] = entry
}

// Held reports whether any entity namespace currently reserves the key.
func (l ReservationLedger) Held(kind ReservationKind, key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	_, held := l.Entries[reservationSlot(kind, key)]
	return held
}

// Release drops an unconfirmed reservation. Confirmed reservations are kept as
// durable evidence of an interrupted create so recovery can reconcile them.
func (l *ReservationLedger) Release(kind ReservationKind, key, owner string) {
	slot := reservationSlot(kind, key)
	entry, held := l.Entries[slot]
	if !held || entry.Owner != owner || entry.Committed {
		return
	}
	delete(l.Entries, slot)
}
