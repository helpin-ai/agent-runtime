//go:build !linux

package main

import "fmt"

func hardenExecutionProcess() error {
	return fmt.Errorf("execution workers require Linux process hardening; use the trusted local CLI on this platform")
}
