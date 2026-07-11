//go:build !darwin && !linux

package monitor

import "errors"

// errUnsupported is returned by the process lister on platforms without a
// scanning implementation (e.g. windows). The daemon still runs; it simply
// reports no observed processes and continues to accept hook reports and
// spawn requests.
var errUnsupported = errors.New("process scan unsupported on this platform")

func listProcesses() ([]rawProc, error) { return nil, errUnsupported }

func processCWD(int) (string, error) { return "", errUnsupported }
