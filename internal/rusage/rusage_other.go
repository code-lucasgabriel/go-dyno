//go:build !unix

package rusage

// CPUSeconds is not available on this platform.
func CPUSeconds() float64 { return 0 }
