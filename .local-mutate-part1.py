#!/usr/bin/env python3
"""Mutation audit for ADR-0147 Part 1: internal/hostinstall and
internal/hostcert.

A sibling of .local-mutate.py, which audits the force-restart work.
Same rules, same reporting, and the same rejection of a mutant that
dies at build time - there were three such first-pass mutants here
and all three were the harness's fault, not the code's.

Every mutant must apply exactly once, change bytes, fail on an
assertion rather than a compile error, and leave the file byte for
byte as it was. A mutant that dies at build time is rejected as
evidence, the way the two build-failure kills in the force-restart
work were, and is reported separately rather than counted.

Run from the repository root:  python3 .local-mutate-part1.py
"""

import os
import subprocess
import sys

PKGS = "./internal/hostinstall/ ./internal/hostcert/"

# (label, file, needle, replacement)
MUTANTS = [
    # --- the preservation rules -------------------------------------
    ("dial loopback even when managerd is bound to a name",
     "internal/hostinstall/writes.go",
     "\tif isLoopbackHostPort(p.RPCAddr) {\n\t\treturn loopbackHost + \":\" + port\n\t}",
     "\tif true {\n\t\treturn loopbackHost + \":\" + port\n\t}"),

    # --- the preservation rules -------------------------------------
    ("stop refusing a half TLS pair",
     "internal/hostinstall/hostinstall.go",
     "\tcase certExists != keyExists:",
     "\tcase false:"),
    ("overwrite a field that is already set",
     "internal/hostinstall/writes.go",
     '\tif own != "" {\n\t\tp.add(ActionKeep, path, field, own, "already set in this file; left exactly as it is")\n\t\treturn false\n\t}\n\tif want == "" {\n\t\tp.add(ActionNeeds, path, field, "", reason)\n\t\treturn false\n\t}\n\t*dst = want\n\tp.add(ActionFill, path, field, want, reason)\n\treturn true\n}\n\n// needHostFact',
     '\tif want == "" {\n\t\tp.add(ActionNeeds, path, field, "", reason)\n\t\treturn false\n\t}\n\t*dst = want\n\tp.add(ActionFill, path, field, want, reason)\n\treturn true\n}\n\n// needHostFact'),
    ("ignore the node_id a file already has",
     "internal/hostinstall/hostinstall.go",
     "p.NodeID = firstNonEmpty(own[0].id, own[1].id, p.opts.FQDN, p.short)",
     "p.NodeID = firstNonEmpty(p.opts.FQDN, p.short)"),
    ("stop refusing a file with unknown fields",
     "internal/hostinstall/hostinstall.go",
     "\tif len(unknown) == 0 {\n\t\treturn nil\n\t}",
     "\treturn nil\n\tif len(unknown) == 0 {\n\t\treturn nil\n\t}"),
    ("treat a two-way node_id dispute as agreement",
     "internal/hostinstall/hostinstall.go",
     "\t\tif id != first {",
     "\t\tif false {"),
    ("report success despite a refusal",
     "internal/hostinstall/writes.go",
     "\tif r := p.Refusals(); len(r) > 0 {\n\t\tseen := map[string]bool{}",
     "\tif r := p.Refusals(); false && len(r) > 0 {\n\t\tseen := map[string]bool{}"),
    ("report a refused Comb as configured",
     "internal/hostinstall/writes.go",
     "\tif r := p.Refusals(); len(r) > 0 {\n\t\treturn fmt.Sprintf(",
     "\tif r := p.Refusals(); false && len(r) > 0 {\n\t\treturn fmt.Sprintf("),

    # --- addresses and permissions ---------------------------------
    ("bind the standalone rpc_addr to the wildcard",
     "internal/hostinstall/hostinstall.go",
     'p.RPCAddr = loopbackHost + ":" + defaultManagerPort',
     'p.RPCAddr = "0.0.0.0:" + defaultManagerPort'),
    ("stop detecting a Comb that is already a member",
     "internal/hostinstall/hostinstall.go",
     "\tif p.managerOwn.RPCAddr != \"\" && !isLoopbackHostPort(p.managerOwn.RPCAddr) {\n\t\treturn false\n\t}",
     "\tif false && p.managerOwn.RPCAddr != \"\" && !isLoopbackHostPort(p.managerOwn.RPCAddr) {\n\t\treturn false\n\t}"),
    ("write the private key world-readable",
     "internal/hostinstall/writes.go",
     "return writeFileAtomic(keyPath, p.pendingKey, 0o600)",
     "return writeFileAtomic(keyPath, p.pendingKey, 0o644)"),
    ("create directories world-readable",
     "internal/hostinstall/writes.go",
     "\t\t\t\tif err := os.MkdirAll(path, 0o700); err != nil {",
     "\t\t\t\tif err := os.MkdirAll(path, 0o755); err != nil {"),
    ("stop creating a missing directory",
     "internal/hostinstall/writes.go",
     "\t\t\t\tif err := os.MkdirAll(path, 0o700); err != nil {",
     "\t\t\t\tif err := error(nil); err != nil {"),
    ("accept a numeric LAN address from --rpc-addr",
     "internal/hostinstall/hostinstall.go",
     "\t\tif !ip.IsLoopback() {",
     "\t\tif false {"),
    ("accept a name the certificate does not carry",
     "internal/hostinstall/hostinstall.go",
     "\t} else if !p.certHasDNS(host) {",
     "\t} else if false {"),

    # --- the certificate gate --------------------------------------
    ("stop requiring the loopback SAN",
     "internal/hostcert/hostcert.go",
     "\tif opts.RequireLoopbackSAN && !hasLoopbackSAN(cert) {",
     "\tif false && opts.RequireLoopbackSAN && !hasLoopbackSAN(cert) {"),
    ("stop requiring a DNS SAN",
     "internal/hostcert/hostcert.go",
     "\tif opts.RequireDNSSAN && len(cert.DNSNames) == 0 {",
     "\tif false && opts.RequireDNSSAN && len(cert.DNSNames) == 0 {"),
    ("accept an RSA key below 2048 bits",
     "internal/hostcert/hostcert.go",
     "\t\tif bits < 2048 {",
     "\t\tif bits < 512 {"),
    ("stop verifying the self-signature",
     "internal/hostcert/hostcert.go",
     "\tif err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {",
     "\tif err := error(nil); err != nil {"),
    ("accept a certificate whose issuer is not its subject",
     "internal/hostcert/hostcert.go",
     "\tif !bytes.Equal(cert.RawIssuer, cert.RawSubject) {",
     "\tif false && !bytes.Equal(cert.RawIssuer, cert.RawSubject) {"),
    ("accept an expired certificate",
     "internal/hostcert/hostcert.go",
     "\tif now.After(cert.NotAfter) {",
     "\tif now.After(cert.NotAfter.Add(100000 * time.Hour)) {"),
    ("accept a certificate that is not yet valid",
     "internal/hostcert/hostcert.go",
     "\tif now.Before(cert.NotBefore) {",
     "\tif now.Before(cert.NotBefore.Add(-100000 * time.Hour)) {"),
    ("stop checking the key against the certificate",
     "internal/hostcert/hostcert.go",
     "\tif len(opts.KeyPEM) > 0 {",
     "\tif false && len(opts.KeyPEM) > 0 {"),
    ("change the fingerprint format",
     "internal/hostcert/hostcert.go",
     'return "SHA256:" + strings.Join(pairs, ":")',
     'return "sha256:" + strings.Join(pairs, ":")'),
    ("put the LAN address into the generated certificate",
     "internal/hostinstall/hostinstall.go",
     "\t\tIPAddresses: []net.IP{net.ParseIP(loopbackHost)},",
     "\t\tIPAddresses: []net.IP{net.ParseIP(loopbackHost), net.ParseIP(\"10.62.0.2\")},"),
    ("drop the FQDN from the generated certificate",
     "internal/hostinstall/hostinstall.go",
     "\tnames := []string{p.short}\n\tif p.opts.FQDN != \"\" {",
     "\tnames := []string{p.short}\n\tif false && p.opts.FQDN != \"\" {"),
]


def run_tests():
    proc = subprocess.run(
        ["go", "test", "-count=1"] + PKGS.split(),
        capture_output=True, text=True)
    return proc.returncode, proc.stdout + proc.stderr


def main():
    backups = {}
    for _, path, _, _ in MUTANTS:
        if path not in backups:
            with open(path, "rb") as fh:
                backups[path] = fh.read()
    results = []
    try:
        for label, path, needle, replacement in MUTANTS:
            original = backups[path]
            count = original.count(needle.encode())
            if count != 1:
                results.append((label, "ANCHOR-FAIL(%d)" % count, ""))
                with open(path, "wb") as fh:
                    fh.write(original)
                continue
            mutated = original.replace(needle.encode(), replacement.encode(), 1)
            if mutated == original:
                results.append((label, "NOCHANGE", ""))
                continue
            with open(path, "wb") as fh:
                fh.write(mutated)
            code, out = run_tests()
            if "build failed" in out or "cannot use" in out or "undefined:" in out:
                verdict = "COMPILE-FAIL"
            elif code == 0:
                verdict = "SURVIVED"
            else:
                verdict = "KILLED"
            detail = ""
            for line in out.splitlines():
                if line.startswith("--- FAIL") or line.startswith("    --- FAIL"):
                    detail = line.strip()
                    break
            results.append((label, verdict, detail))
            with open(path, "wb") as fh:
                fh.write(original)
            same = open(path, "rb").read() == original
            if not same:
                print("RESTORE FAILED for %s" % path)
                sys.exit(1)
            sys.stdout.write("%-9s %-52s %s\n" % (verdict, label, detail))
            sys.stdout.flush()
    finally:
        for path, body in backups.items():
            with open(path, "wb") as fh:
                fh.write(body)

    killed = sum(1 for _, v, _ in results if v == "KILLED")
    survived = [l for l, v, _ in results if v == "SURVIVED"]
    compile_fail = [l for l, v, _ in results if v == "COMPILE-FAIL"]
    anchor = [l for l, v, _ in results if v.startswith("ANCHOR") or v == "NOCHANGE"]
    print("")
    print("mutations: %d  killed: %d  survived: %d  compile-fail: %d  anchor-fail: %d"
          % (len(results), killed, len(survived), len(compile_fail), len(anchor)))
    for group, name in ((survived, "SURVIVED"), (compile_fail, "COMPILE-FAIL"),
                        (anchor, "ANCHOR-FAIL")):
        for label in group:
            print("  %s %s" % (name, label))


if __name__ == "__main__":
    main()
