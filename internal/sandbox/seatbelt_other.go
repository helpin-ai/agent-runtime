//go:build !darwin

package sandbox

import "errors"

// SeatbeltAvailable fails off macOS: Seatbelt is a macOS mechanism.
func SeatbeltAvailable() error { return errors.New("seatbelt is only available on macOS") }
