package atomic2

import "sync/atomic"

type Int32[T ~int32] atomic.Int32

func (i *Int32[T]) Load() T {
	return T((*atomic.Int32)(i).Load())
}

func (i *Int32[T]) Store(val T) {
	(*atomic.Int32)(i).Store(int32(val))
}

func (i *Int32[T]) Swap(new T) T {
	return T((*atomic.Int32)(i).Swap(int32(new)))
}

func (i *Int32[T]) CompareAndSwap(old, new T) bool {
	return (*atomic.Int32)(i).CompareAndSwap(int32(old), int32(new))
}

func (i *Int32[T]) Add(delta T) T {
	return T((*atomic.Int32)(i).Add(int32(delta)))
}
