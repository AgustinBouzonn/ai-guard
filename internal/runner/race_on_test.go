//go:build race

package runner

// The helper child is this same test binary, so under -race its memory is
// inflated by the detector's shadow state.
const raceBuild = true
