//go:build race

package memprobe

// The race detector slows the sampler several-fold, so the overhead test
// would measure the detector.
const raceEnabled = true
