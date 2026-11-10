package stats

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTypicalIsTheMedianOfTheRecordedPeaks(t *testing.T) {
	b := Load(filepath.Join(t.TempDir(), "stats.json"))
	if _, ok := b.Typical("ts.types"); ok {
		t.Fatal("a step that never ran has a typical peak")
	}
	// One cold run among warm ones must not set the expectation.
	for _, mb := range []int{2329, 516, 520, 510, 512} {
		b.Record("ts.types", mb, 1000)
	}
	if got, ok := b.Typical("ts.types"); !ok || got != 516 {
		t.Errorf("Typical = %d, %v; want the median 516", got, ok)
	}
}

func TestOnlyTheMostRecentRunsCount(t *testing.T) {
	b := Load(filepath.Join(t.TempDir(), "stats.json"))
	for i := 0; i < 20; i++ {
		b.Record("go.test", 100, 10)
	}
	for i := 0; i < 8; i++ {
		b.Record("go.test", 900, 10)
	}
	if got, _ := b.Typical("go.test"); got != 900 {
		t.Errorf("Typical = %d, want 900: the step changed and the old runs are history", got)
	}
}

func TestChunksOfAStepShareOneRecordAndSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache", "stats.json") // the directory does not exist yet
	b := Load(path)
	b.Record("ts.lint@apps/web#1", 80, 5)
	b.Record("ts.lint@apps/web#2", 120, 5)
	b.Record("py.types", 0, 5) // nothing measured: not a sample
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	again := Load(path)
	if got, ok := again.Typical("ts.lint@apps/web#7"); !ok || got != 120 {
		t.Errorf("Typical after reload = %d, %v; want 120 (upper of the two samples)", got, ok)
	}
	if _, ok := again.Typical("py.types"); ok {
		t.Error("a run without a measurement became a sample")
	}
	if _, ok := again.Typical("ts.lint@apps/api"); ok {
		t.Error("projects must not share a record")
	}
}

func TestAMissingCorruptOrAbsentBookIsEmptyNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := Load(path)
	if _, ok := b.Typical("x"); ok {
		t.Error("corrupt file produced data")
	}
	b.Record("x", 10, 1)
	if err := b.Save(); err != nil {
		t.Errorf("Save over a corrupt file: %v", err)
	}
	var none *Book // no state directory: learning is simply off
	none.Record("x", 10, 1)
	if _, ok := none.Typical("x"); ok {
		t.Error("nil book has data")
	}
	if err := none.Save(); err != nil {
		t.Errorf("nil Save: %v", err)
	}
	untouched := filepath.Join(t.TempDir(), "untouched.json")
	if err := Load(untouched).Save(); err != nil {
		t.Errorf("Save with nothing recorded: %v", err)
	}
	if _, err := os.Stat(untouched); err == nil {
		t.Error("an unchanged book was written")
	}
}
