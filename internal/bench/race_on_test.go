//go:build race

package bench

// The fake pipelines are this same test binary: under -race each start costs
// about a second, which swamps the 400 ms vs 40 ms of simulated work.
const raceBuild = true
