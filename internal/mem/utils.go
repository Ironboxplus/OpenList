package mem

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/pkg/singleflight"
)

var ErrNotEnoughMemory = errors.New("not enough memory")

func MemoryGrowCheck(growSize uint64) error {
	if conf.MinFreeMemory == 0 {
		return ErrNotEnoughMemory
	}
	m, err, _ := singleflight.AnyGroup.Do("MemoryGrowCheck", func() (any, error) {
		_, available, err := EffectiveMemory()
		if err != nil {
			return nil, err
		}
		if available < conf.MinFreeMemory {
			return nil, ErrNotEnoughMemory
		}
		return &available, nil
	})
	if err != nil {
		return err
	}
	avail := m.(*uint64)
	for {
		available := atomic.LoadUint64(avail)
		if available < growSize || available-growSize < conf.MinFreeMemory {
			return ErrNotEnoughMemory
		}
		if atomic.CompareAndSwapUint64(avail, available, available-growSize) {
			return nil
		}
	}
}

func NewGuardedMemory(cap, max uint64) (m LinearMemory, err error) {
	if err := MemoryGrowCheck(cap); err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrNotEnoughMemory, r)
		}
	}()
	m, err = NewMemory(cap, max)
	if err != nil {
		return nil, err
	}
	if s, ok := m.(interface{ SetGrowCheck(GrowCheck) }); ok {
		s.SetGrowCheck(MemoryGrowCheck)
	}
	gm := &guardedMemory{LinearMemory: m}
	gm.cleanup = runtime.AddCleanup(gm, func(m LinearMemory) {
		m.Free()
	}, m)
	return gm, nil
}

type guardedMemory struct {
	LinearMemory
	cleanup runtime.Cleanup
}

func (s *guardedMemory) Reallocate(size uint64) (all []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrNotEnoughMemory, r)
		}
	}()
	return s.LinearMemory.Reallocate(size)
}

func (s *guardedMemory) Free() error {
	s.cleanup.Stop()
	return s.LinearMemory.Free()
}
