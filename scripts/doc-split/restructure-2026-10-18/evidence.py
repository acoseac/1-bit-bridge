"""Which tracked files an entry names: the evidence split.py --coverage checks the rules files'
globs against.

An entry names a file directly (`internal/manifest/scanner.go`, `app.js`) or through an
identifier defined there: a Go func, method, type, var or const, or a JavaScript function. An
identifier counts only when it is specific (camelCase, an underscore, or twelve characters or
more, never under six) and is defined in at most three non-test files: `Open` or `size` would
point everywhere.
"""
import collections
import os
import re

SOURCE = (".go", ".js", ".mjs", ".html", ".css", ".sh", ".ps1", ".yml", ".yaml", ".tmpl")
PATH_RE = re.compile(r"(?<![\w/.-])((?:\.github/|cmd/|internal/|deploy/|config/|docs/|ops/|testdata/|tools/)?"
                     r"[\w.-]+(?:/[\w.-]+)*\.(?:go|js|mjs|html|css|sh|ps1|yml|yaml|tmpl))\b")
GO_DEF = re.compile(r"^(?:func\s+(?:\([^)]*\)\s*)?|type\s+|var\s+|const\s+)([A-Za-z_]\w*)", re.M)
GO_GROUP = re.compile(r"^(?:var|const|type)\s*\(\n((?:.*\n)*?)\)", re.M)
JS_DEF = re.compile(r"\bfunction\s+([A-Za-z_$][\w$]*)")


def specific(name):
    if len(name) < 6:
        return False
    return bool(re.search(r"[a-z][A-Z]", name)) or "_" in name.strip("_") or len(name) >= 12


class Index:
    def __init__(self, repo, tracked):
        self.files = set(tracked)
        self.by_base = collections.defaultdict(set)
        self.defs = collections.defaultdict(set)
        for rel in tracked:
            self.by_base[os.path.basename(rel)].add(rel)
            if not rel.endswith((".go", ".js")) or rel.endswith("_test.go"):
                continue
            path = os.path.join(repo, rel)
            if not os.path.isfile(path):
                continue
            with open(path, encoding="utf-8", errors="replace") as f:
                text = f.read()
            if rel.endswith(".go"):
                for m in GO_DEF.finditer(text):
                    self.defs[m.group(1)].add(rel)
                for g in GO_GROUP.finditer(text):
                    for m in re.finditer(r"^\t([A-Za-z_]\w*)", g.group(1), re.M):
                        self.defs[m.group(1)].add(rel)
            else:
                for m in JS_DEF.finditer(text):
                    self.defs[m.group(1)].add(rel)

    def files_named(self, text):
        """{tracked file: the names in TEXT that point at it}."""
        found = collections.defaultdict(set)
        for m in PATH_RE.finditer(text):
            p = m.group(1)
            if p in self.files:
                found[p].add(os.path.basename(p))
                continue
            for f in self.by_base.get(os.path.basename(p), ()):
                if f.endswith(p):
                    found[f].add(os.path.basename(p))
        names = set(re.findall(r"`\.?([A-Za-z_][A-Za-z0-9_]*)", text))
        names |= set(re.findall(r"`[A-Za-z_][A-Za-z0-9_]*\.([A-Za-z_][A-Za-z0-9_]*)", text))
        names |= set(re.findall(r"\b([A-Z][a-z0-9]+(?:[A-Z][A-Za-z0-9]*)+)\b", text))
        names |= set(re.findall(r"\b([a-z]+[A-Z][A-Za-z0-9]*)\b", text))
        for n in names:
            where = self.defs.get(n, ())
            if specific(n) and 0 < len(where) <= 3:
                for f in where:
                    found[f].add(n)
        return found
