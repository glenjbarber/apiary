#!/usr/bin/env python3
"""Mutation harness for this branch.

Deliberately crude and deliberately loud. The two ways a mutation harness
lies are (a) reporting a build error as a clean pass, and (b) reporting a
test failure as a build error or vice versa, so this script classifies
every run into exactly one of three buckets and prints which:

  BUILD-ERROR   the mutated tree does not compile. Reported separately
                and counted as CAUGHT, because a mutation that does not
                compile is a load-bearing anchor, but never reported as a
                test failure.
  TEST-FAIL     the tree compiles and at least one test failed. The
                failing test names are printed.
  PASS          the tree compiles and the tests all pass. THIS IS A
                SURVIVOR and it is a test gap, and it is reported as one.

No grep filters the output. The exit status of the test command is read
directly, and `grep -c` is never used for a count (it exits 1 on zero
matches, which has already produced one false "tests failed" and one
false "tests passed" report in this project).

Usage:
    python3 .local-mutate.py <manifest.py>

The manifest is a list of (label, path, old, new) tuples. Source is
restored with git checkout after every mutation and the tree is verified
byte-identical to HEAD at the end.
"""

import subprocess
import sys
import os

REPO = os.path.dirname(os.path.abspath(__file__))


def sh(cmd, **kw):
    return subprocess.run(cmd, shell=True, cwd=REPO, capture_output=True, text=True, **kw)


def tree_is_clean():
    r = sh("git status --porcelain")
    return r.stdout.strip() == ""


BUILD_MARKERS = (
    "[build failed]",
    "build constraints exclude all Go files",
    "cannot use",
    "undefined:",
    "syntax error",
    "expected declaration",
)


def run(pkg):
    """Classify one run. Never conflates a build error with a test failure."""
    r = sh(f"go test -count=1 {pkg}")
    out = r.stdout + r.stderr
    if any(m in out for m in BUILD_MARKERS) and "--- FAIL:" not in out:
        return "BUILD-ERROR", out
    if r.returncode != 0:
        return "TEST-FAIL", out
    return "PASS", out


def main():
    manifest = sys.argv[1]
    ns = {}
    with open(manifest) as f:
        exec(compile(f.read(), manifest, "exec"), ns)
    mutations = ns["MUTATIONS"]
    default_pkg = ns.get("DEFAULT_PKG", "./...")

    if not tree_is_clean():
        print("REFUSING TO RUN: the worktree is not clean.")
        print(sh("git status --porcelain").stdout)
        return 2

    # Pre-flight: every anchor must appear exactly once before anything is
    # applied. A mutation whose anchor is wrong would otherwise be counted
    # as a survivor, and a survivor is a claim about the tests.
    broken = []
    for entry in mutations:
        label, path, old = entry[0], entry[1], entry[2]
        with open(os.path.join(REPO, path)) as f:
            n = f.read().count(old)
        if n != 1:
            broken.append(f"{label}: anchor matches {n} times in {path}, need exactly 1")
    if broken:
        print("PRE-FLIGHT FAILED - the manifest is wrong and proves nothing as written:")
        for b in broken:
            print("  " + b)
        return 2
    print(f"  pre-flight: all {len(mutations)} anchors match exactly once")

    tally = {"BUILD-ERROR": 0, "TEST-FAIL": 0, "PASS": 0}
    survivors = []
    rows = []
    for entry in mutations:
        label, path, old, new = entry[:4]
        pkg = entry[4] if len(entry) > 4 else default_pkg
        full = os.path.join(REPO, path)
        with open(full) as f:
            src = f.read()
        with open(full, "w") as f:
            f.write(src.replace(old, new, 1))
        bucket, out = run(pkg)
        sh(f"git checkout -- {path}")
        tally[bucket] = tally.get(bucket, 0) + 1
        detail = ""
        if bucket == "TEST-FAIL":
            names = sorted({l.split()[2] for l in out.splitlines() if l.startswith("--- FAIL:")})
            detail = ", ".join(n.replace("Test", "") for n in names[:4])
        elif bucket == "BUILD-ERROR":
            detail = "does not compile"
        elif bucket == "PASS":
            survivors.append(label)
        rows.append((label, bucket, detail))

    w = max(len(r[0]) for r in rows)
    print()
    for label, bucket, detail in rows:
        print(f"  {label:<{w}}  {bucket:<12} {detail}")
    print()
    print(f"  applied   : {len(rows)}")
    print(f"  caught    : {tally['BUILD-ERROR']} build-error + {tally['TEST-FAIL']} test-fail "
          f"= {tally['BUILD-ERROR'] + tally['TEST-FAIL']}")
    print(f"  survived  : {tally['PASS']}")
    if tally.get("ANCHOR-MISS"):
        print(f"  ANCHOR MISS: {tally['ANCHOR-MISS']} - the mutation was not applied at all, so it proves nothing")
    for s in survivors:
        print(f"  SURVIVOR : {s}")
    print()
    print(f"  tree clean after the run: {tree_is_clean()}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
