package app

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The lease covers at most two bounded HTTP requests plus persistence. Both
// the deadline and ownership live in the DB, including across process restarts.
const rotationLeaseDuration = 2*rotateRequestTimeout + 60*time.Second

type rotationState struct {
	LastAttempt int64  `json:"last_attempt"`
	LastSuccess int64  `json:"last_success"`
	NextAttempt int64  `json:"next_attempt"`
	LeaseUntil  int64  `json:"lease_until"`
	Failures    int    `json:"failures"`
	LastError   string `json:"last_error"`
}

func loadRotationState(id int64) (rotationState, error) {
	var s rotationState
	err := getDB().QueryRow(`SELECT last_attempt,last_success,next_attempt,lease_until,failures,last_error FROM account_rotations WHERE account_id=?`, id).Scan(&s.LastAttempt, &s.LastSuccess, &s.NextAttempt, &s.LeaseUntil, &s.Failures, &s.LastError)
	return s, err
}

type rotationDeferredError struct{ Wait time.Duration }

func (e *rotationDeferredError) Error() string {
	return fmt.Sprintf("保活已在执行或等待下次刷新，%s 后再检查", e.Wait.Round(time.Second))
}

func rotationWait(s rotationState, now time.Time) time.Duration {
	next := s.NextAttempt
	if s.LeaseUntil > next {
		next = s.LeaseUntil
	}
	d := time.Unix(next, 0).Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return d
}

func claimRotation(id int64) (rotationState, string, error) {
	s, err := loadRotationState(id)
	if errors.Is(err, sql.ErrNoRows) {
		// A competing process may insert first. Re-read rather than overwrite it.
		_, insertErr := getDB().Exec(`INSERT INTO account_rotations(account_id) VALUES(?)`, id)
		s, err = loadRotationState(id)
		if err != nil {
			return s, "", errors.Join(err, insertErr)
		}
	} else if err != nil {
		return s, "", err
	}
	now := time.Now()
	if s.NextAttempt > now.Unix() || s.LeaseUntil > now.Unix() {
		return s, "", &rotationDeferredError{rotationWait(s, now)}
	}
	var tokenBytes [16]byte
	if _, err = rand.Read(tokenBytes[:]); err != nil {
		return s, "", err
	}
	token := fmt.Sprintf("%x", tokenBytes)
	result, err := getDB().Exec(`UPDATE account_rotations SET last_attempt=?,lease_until=?,lease_token=? WHERE account_id=? AND next_attempt<=? AND lease_until<=?`, now.Unix(), now.Add(rotationLeaseDuration).Unix(), token, id, now.Unix(), now.Unix())
	if err != nil {
		return s, "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return s, "", err
	}
	if n != 1 {
		s, err = loadRotationState(id)
		if err != nil {
			return s, "", err
		}
		return s, "", &rotationDeferredError{rotationWait(s, time.Now())}
	}
	return s, token, nil
}

func finishRotation(id int64, token string, previous rotationState, wait time.Duration, refreshErr error) error {
	now := time.Now()
	success := previous.LastSuccess
	failures := 0
	detail := ""
	if refreshErr == nil {
		success = now.Unix()
	} else {
		failures = previous.Failures + 1
		detail = truncate(refreshErr.Error(), 450)
	}
	// Stored deadlines have second precision. Round up so Retry-After is never
	// shortened by truncating the fractional part of the response timestamp.
	result, err := getDB().Exec(`UPDATE account_rotations SET last_success=?,next_attempt=?,lease_until=0,lease_token='',failures=?,last_error=? WHERE account_id=? AND lease_token=?`, success, rotationDeadline(now, wait), failures, detail, id, token)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("account #%d rotation lease lost", id)
	}
	return nil
}

func rotationDeadline(now time.Time, wait time.Duration) int64 {
	return now.Add(wait).Add(time.Second - time.Nanosecond).Unix()
}

func rotationFailureWait(previous rotationState, err error) time.Duration {
	var limited *rotationHTTPError
	if errors.As(err, &limited) {
		if limited.RetryAfter > 0 {
			return limited.RetryAfter
		}
		return defaultRotateInterval
	}
	wait := min1PSIDTSInterval
	for i := 0; i < previous.Failures && wait < defaultRotateInterval; i++ {
		wait *= 2
	}
	if wait > defaultRotateInterval {
		wait = defaultRotateInterval
	}
	return wait
}
