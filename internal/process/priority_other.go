//go:build !windows

package process

func BoostCurrent() error {
	return nil
}

func BoostPID(pid int) error {
	return nil
}
