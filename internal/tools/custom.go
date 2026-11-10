package tools

import (
	"path/filepath"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

// custom adds the project-specific steps declared in aiguard.json.
func (b *builder) custom(all []string, byLang map[lang.Lang][]string) {
	for _, cs := range b.cfg.Steps {
		files := all
		if len(cs.Langs) > 0 {
			files = nil
			for _, l := range cs.Langs {
				files = append(files, byLang[lang.Lang(l)]...)
			}
			if len(files) == 0 {
				continue // nothing of that family changed
			}
		}
		dir := filepath.ToSlash(filepath.Clean(cs.Dir))
		if cs.Dir == "" {
			dir = "."
		}
		var rel []string
		for _, f := range files {
			if dir == "." || strings.HasPrefix(f, dir+"/") {
				rel = append(rel, relTo(dir, f))
			}
		}
		s := &Step{ID: cs.ID, Tier: cs.Tier, Phase: 1, Dir: b.abs(dir), Parser: diag.Generic, Files: len(rel)}
		if cs.Tier == 0 {
			s.Phase = 0
		} else if cs.Phase != nil {
			s.Phase = *cs.Phase
		}
		if p, ok := diag.ByName(cs.Parser); ok {
			s.Parser = p
		}
		if cs.Shell != "" {
			quoted := make([]string, len(rel))
			for i, f := range rel {
				quoted[i] = f
				if strings.ContainsAny(f, " \t") {
					quoted[i] = `"` + f + `"`
				}
			}
			s.Shell = strings.ReplaceAll(cs.Shell, "{files}", strings.Join(quoted, " "))
		} else {
			needsFiles := false
			for _, a := range cs.Cmd {
				if a == "{files}" {
					needsFiles = true
					s.Argv = append(s.Argv, rel...)
				} else {
					s.Argv = append(s.Argv, a)
				}
			}
			if needsFiles && len(rel) == 0 {
				continue
			}
		}
		b.add(s)
	}
}
