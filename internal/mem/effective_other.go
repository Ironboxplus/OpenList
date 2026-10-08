//go:build !linux

package mem

func containerLimit() *cgroupLimit { return nil }
