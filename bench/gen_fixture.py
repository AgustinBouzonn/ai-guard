#!/usr/bin/env python3
"""Generate a synthetic polyglot repository (TypeScript, Python, Go, Rust) to try
AI-Guard and its benchmark without touching a real project.

    python bench/gen_fixture.py DEST [--ts 300] [--py 300] [--go 60] [--rs 80]

Modules form chains of ten that hang from one shared module per language, so an
edit to a mid-chain module has a small blast radius and an edit to the shared
module touches everything. Dependencies are declared, not installed: run
`npm install` and `uv sync` in DEST afterwards.
"""
import argparse
import json
import pathlib
import subprocess
import textwrap

CHAIN = 10


def write(root: pathlib.Path, rel: str, text: str) -> None:
    path = root / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(textwrap.dedent(text).lstrip("\n"), encoding="utf-8", newline="\n")


def typescript(root: pathlib.Path, n: int) -> None:
    write(root, "package.json", json.dumps({
        "name": "aiguard-fixture", "private": True, "type": "module",
        "scripts": {"lint": "eslint .", "typecheck": "tsc --noEmit", "test": "vitest run"},
        "devDependencies": {
            "@biomejs/biome": "^2.6.0", "@eslint/js": "^10.0.0", "@typescript/native-preview": "latest",
            "eslint": "^10.0.0", "typescript": "~6.0.3", "typescript-eslint": "^8.72.0", "vitest": "^5.0.0",
        },
    }, indent=2) + "\n")
    write(root, "tsconfig.json", """
        {
          "compilerOptions": {
            "target": "es2022", "module": "nodenext", "moduleResolution": "nodenext",
            "strict": true, "noEmit": true, "skipLibCheck": true, "types": []
          },
          "include": ["web"]
        }
        """)
    write(root, "tsconfig.fast-check.json", """
        {
          "extends": "./tsconfig.json",
          "compilerOptions": {
            "noEmit": true, "incremental": true, "tsBuildInfoFile": "./.aiguard/cache/fast-check.tsbuildinfo",
            "skipLibCheck": true, "isolatedModules": true
          }
        }
        """)
    write(root, "eslint.config.js", """
        import js from "@eslint/js";
        import tseslint from "typescript-eslint";

        export default tseslint.config(
          { ignores: ["node_modules/**", "target/**", ".venv/**", ".aiguard/**"] },
          js.configs.recommended,
          ...tseslint.configs.recommended,
        );
        """)
    write(root, "biome.json", """
        {
          "vcs": { "enabled": true, "clientKind": "git", "useIgnoreFile": true },
          "files": { "ignoreUnknown": true },
          "formatter": { "enabled": false },
          "linter": { "enabled": true, "rules": { "preset": "recommended" } }
        }
        """)
    write(root, "web/src/shared.ts", """
        export interface Item { id: number; name: string; tags: string[]; score: number }

        export const seed: number = 7;

        export function build(n: number, factor: number): Item[] {
          return Array.from({ length: n }, (_, k) => ({ id: k, name: `item-${k}`, tags: [String(k % 3)], score: k * factor }));
        }

        export function total(items: Item[]): number {
          return items.reduce((acc, item) => acc + item.score, 0);
        }
        """)
    for i in range(n):
        prev = "shared" if i % CHAIN == 0 else f"mod_{i - 1:03d}"
        base = "seed" if i % CHAIN == 0 else f"value_{i - 1:03d}"
        write(root, f"web/src/mod_{i:03d}.ts", f"""
            import {{ build, total, type Item }} from "./shared.js";
            import {{ {base} }} from "./{prev}.js";

            export const value_{i:03d}: number = {base} + {i};

            export interface Report_{i:03d} {{ label: string; items: Item[]; sum: number }}

            export function report_{i:03d}(n: number): Report_{i:03d} {{
              const items = build(n, value_{i:03d});
              return {{ label: "mod_{i:03d}", items, sum: total(items) }};
            }}

            export class Service_{i:03d} {{
              constructor(private readonly base: number = value_{i:03d}) {{}}

              run(n: number): number {{
                return report_{i:03d}(n).sum + this.base;
              }}

              describe(): string {{
                return `service {i:03d} with base ${{this.base}}`;
              }}
            }}
            """)
        if i % 5 == 0:
            write(root, f"web/src/mod_{i:03d}.test.ts", f"""
                import {{ expect, test }} from "vitest";
                import {{ Service_{i:03d}, report_{i:03d}, value_{i:03d} }} from "./mod_{i:03d}.js";

                test("report_{i:03d} sums scores", () => {{
                  const r = report_{i:03d}(4);
                  expect(r.sum).toBe((0 + 1 + 2 + 3) * value_{i:03d});
                  expect(r.items).toHaveLength(4);
                }});

                test("Service_{i:03d} adds its base", () => {{
                  expect(new Service_{i:03d}(10).run(3)).toBe((0 + 1 + 2) * value_{i:03d} + 10);
                }});
                """)


def python(root: pathlib.Path, n: int) -> None:
    write(root, "pyproject.toml", """
        [project]
        name = "aiguard-fixture"
        version = "0.1.0"
        requires-python = ">=3.10"
        dependencies = []

        [dependency-groups]
        dev = ["ruff", "ty", "pytest", "pytest-testmon", "flake8", "black"]

        [tool.ruff]
        line-length = 120
        extend-exclude = ["node_modules", "target", ".aiguard"]

        [tool.black]
        line-length = 120

        [tool.pytest.ini_options]
        testpaths = ["api/tests"]

        [tool.ty.src]
        include = ["api"]
        """)
    write(root, ".flake8", """
        [flake8]
        max-line-length = 120
        exclude = .git,.venv,node_modules,target,.aiguard
        """)
    write(root, "api/__init__.py", "")
    write(root, "api/pkg/__init__.py", "")
    write(root, "api/tests/__init__.py", "")
    write(root, "api/pkg/shared.py", '''
        from dataclasses import dataclass

        SEED: int = 7


        @dataclass(frozen=True)
        class Item:
            id: int
            name: str
            score: int


        def build(n: int, factor: int) -> list[Item]:
            return [Item(id=k, name=f"item-{k}", score=k * factor) for k in range(n)]


        def total(items: list[Item]) -> int:
            return sum(item.score for item in items)
        ''')
    for i in range(n):
        prev = "shared" if i % CHAIN == 0 else f"m_{i - 1:03d}"
        base = "SEED" if i % CHAIN == 0 else f"VALUE_{i - 1:03d}"
        write(root, f"api/pkg/m_{i:03d}.py", f'''
            from api.pkg.{prev} import {base}
            from api.pkg.shared import Item, build, total

            VALUE_{i:03d}: int = {base} + {i}


            def report_{i:03d}(n: int) -> tuple[list[Item], int]:
                items = build(n, VALUE_{i:03d})
                return items, total(items)


            class Service{i:03d}:
                def __init__(self, base: int = VALUE_{i:03d}) -> None:
                    self.base = base

                def run(self, n: int) -> int:
                    return report_{i:03d}(n)[1] + self.base

                def describe(self) -> str:
                    return f"service {i:03d} with base {{self.base}}"
            ''')
        if i % 3 == 0:
            write(root, f"api/tests/test_m_{i:03d}.py", f'''
                from api.pkg.m_{i:03d} import VALUE_{i:03d}, Service{i:03d}, report_{i:03d}


                def test_report_sums_scores():
                    items, total = report_{i:03d}(4)
                    assert total == (0 + 1 + 2 + 3) * VALUE_{i:03d}
                    assert len(items) == 4


                def test_service_adds_base():
                    assert Service{i:03d}(10).run(3) == (0 + 1 + 2) * VALUE_{i:03d} + 10
                ''')


def golang(root: pathlib.Path, n: int) -> None:
    write(root, "go.mod", "module fixture\n\ngo 1.22\n")
    write(root, "svc/shared/shared.go", """
        // Package shared holds the helpers every service package uses.
        package shared

        // Seed is the base value of every chain.
        const Seed = 7

        // Item is one scored record.
        type Item struct {
        	ID    int
        	Name  string
        	Score int
        }

        // Build makes n items scored by factor.
        func Build(n, factor int) []Item {
        	items := make([]Item, n)
        	for k := range items {
        		items[k] = Item{ID: k, Name: "item", Score: k * factor}
        	}
        	return items
        }

        // Total adds the scores.
        func Total(items []Item) int {
        	sum := 0
        	for _, it := range items {
        		sum += it.Score
        	}
        	return sum
        }
        """)
    imports = []
    for i in range(n):
        name = f"p{i:03d}"
        if i % CHAIN == 0:
            dep, base = "", "shared.Seed"
        else:
            dep, base = f'\t"fixture/svc/p{i - 1:03d}"\n', f"p{i - 1:03d}.Value"
        write(root, f"svc/{name}/{name}.go", f"""
            // Package {name} is service {i} of the fixture.
            package {name}

            import (
            {dep}\t"fixture/svc/shared"
            )

            // Value is this package's factor.
            const Value = {base} + {i}

            // Report builds n items and returns them with their total.
            func Report(n int) ([]shared.Item, int) {{
            \titems := shared.Build(n, Value)
            \treturn items, shared.Total(items)
            }}

            // Service runs reports on top of a base.
            type Service struct{{ Base int }}

            // Run returns the report total plus the base.
            func (s Service) Run(n int) int {{
            \t_, sum := Report(n)
            \treturn sum + s.Base
            }}
            """)
        write(root, f"svc/{name}/{name}_test.go", f"""
            package {name}

            import "testing"

            func TestReportSumsScores(t *testing.T) {{
            \titems, sum := Report(4)
            \tif sum != (0+1+2+3)*Value || len(items) != 4 {{
            \t\tt.Errorf("Report(4) = %d items, sum %d", len(items), sum)
            \t}}
            }}

            func TestServiceAddsBase(t *testing.T) {{
            \tif got := (Service{{Base: 10}}).Run(3); got != (0+1+2)*Value+10 {{
            \t\tt.Errorf("Run(3) = %d", got)
            \t}}
            }}
            """)
        if i % CHAIN == CHAIN - 1 or i == n - 1:
            imports.append(name)
    body = "\n".join(f'\t"fixture/svc/{p}"' for p in imports)
    calls = "\n".join(f"\tsum += ({p}.Service{{Base: 1}}).Run(10)" for p in imports)
    write(root, "cmd/server/main.go", f"""
        // Command server links every service chain into one binary.
        package main

        import (
        \t"fmt"

        {body}
        )

        func main() {{
        \tsum := 0
        {calls}
        \tfmt.Println(sum)
        }}
        """)


def rust(root: pathlib.Path, n: int) -> None:
    write(root, "Cargo.toml", """
        [workspace]
        resolver = "2"
        members = ["crates/core", "crates/cli"]

        [workspace.package]
        edition = "2021"
        version = "0.1.0"
        """)
    write(root, "crates/core/Cargo.toml", """
        [package]
        name = "fixture-core"
        edition.workspace = true
        version.workspace = true
        """)
    write(root, "crates/cli/Cargo.toml", """
        [package]
        name = "fixture-cli"
        edition.workspace = true
        version.workspace = true

        [dependencies]
        fixture-core = { path = "../core" }
        """)
    mods = "\n".join(f"pub mod m_{i:03d};" for i in range(n))
    write(root, "crates/core/src/lib.rs", f"""
        //! Core library of the fixture.
        pub mod shared;
        {mods}
        """)
    write(root, "crates/core/src/shared.rs", """
        //! Helpers every module uses.

        pub const SEED: i64 = 7;

        #[derive(Debug, Clone, PartialEq)]
        pub struct Item {
            pub id: i64,
            pub name: String,
            pub score: i64,
        }

        pub fn build(n: i64, factor: i64) -> Vec<Item> {
            (0..n).map(|k| Item { id: k, name: format!("item-{k}"), score: k * factor }).collect()
        }

        pub fn total(items: &[Item]) -> i64 {
            items.iter().map(|it| it.score).sum()
        }
        """)
    for i in range(n):
        base = "crate::shared::SEED" if i % CHAIN == 0 else f"crate::m_{i - 1:03d}::VALUE"
        write(root, f"crates/core/src/m_{i:03d}.rs", f"""
            //! Module {i} of the fixture.
            use crate::shared::{{build, total, Item}};

            pub const VALUE: i64 = {base} + {i};

            pub fn report(n: i64) -> (Vec<Item>, i64) {{
                let items = build(n, VALUE);
                let sum = total(&items);
                (items, sum)
            }}

            pub struct Service {{
                pub base: i64,
            }}

            impl Service {{
                pub fn run(&self, n: i64) -> i64 {{
                    report(n).1 + self.base
                }}
            }}

            #[cfg(test)]
            mod tests {{
                use super::*;

                #[test]
                fn report_sums_scores() {{
                    let (items, sum) = report(4);
                    assert_eq!(sum, (0 + 1 + 2 + 3) * VALUE);
                    assert_eq!(items.len(), 4);
                }}

                #[test]
                fn service_adds_base() {{
                    assert_eq!(Service {{ base: 10 }}.run(3), (0 + 1 + 2) * VALUE + 10);
                }}
            }}
            """)
    last = f"m_{n - 1:03d}"
    write(root, "crates/cli/src/main.rs", f"""
        use fixture_core::{last}::Service;

        fn main() {{
            println!("{{}}", Service {{ base: 1 }}.run(10));
        }}
        """)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("dest")
    ap.add_argument("--ts", type=int, default=300)
    ap.add_argument("--py", type=int, default=300)
    ap.add_argument("--go", type=int, default=60)
    ap.add_argument("--rs", type=int, default=80)
    args = ap.parse_args()
    root = pathlib.Path(args.dest).resolve()
    if root.exists() and any(root.iterdir()):
        raise SystemExit(f"{root} is not empty")
    root.mkdir(parents=True, exist_ok=True)
    write(root, ".gitignore", "node_modules/\n.venv/\ntarget/\n__pycache__/\n.pytest_cache/\n.ruff_cache/\n.mypy_cache/\n.testmondata*\n*.tsbuildinfo\n")
    typescript(root, args.ts)
    python(root, args.py)
    golang(root, args.go)
    rust(root, args.rs)
    try:
        for cmd in (["init", "-q", "-b", "main"], ["add", "-A"],
                    ["-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-q", "-m", "fixture"]):
            subprocess.run(["git", "-C", str(root), *cmd], check=True)
    except (OSError, subprocess.CalledProcessError) as err:
        print(f"warning: git setup failed ({err}); AI-Guard needs a git repository")
    print(f"fixture ready in {root}: {args.ts} TS, {args.py} Python, {args.go} Go packages, {args.rs} Rust modules")
    print("next: npm install && uv sync   (then: ai-guard bench init)")


if __name__ == "__main__":
    main()
