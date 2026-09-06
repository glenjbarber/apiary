package hostconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactMasterPasswd(t *testing.T) {
	input := `# comment line
root:$6$abcdefghijklmnopqrstuvwxyz0123456789:0:0:Charlie &:/root:/bin/csh
apiary:*:1001:1001:Apiary:/home/apiary:/bin/sh

alice:$2b$10$somehash.that.looks.like.bcrypt:1002:1002:Alice Operator:/home/alice:/bin/tcsh
`
	got, err := RedactMasterPasswd(strings.NewReader(input))
	if err != nil {
		t.Fatalf("RedactMasterPasswd() error: %v", err)
	}
	out := string(got)

	if strings.Contains(out, "$6$abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("root's real hash leaked into output: %s", out)
	}
	if strings.Contains(out, "$2b$10$somehash") {
		t.Errorf("alice's real hash leaked into output: %s", out)
	}
	if !strings.Contains(out, "root:*:0:0:Charlie &:/root:/bin/csh") {
		t.Errorf("root's account structure not preserved with a redacted hash, got: %s", out)
	}
	if !strings.Contains(out, "alice:*:1002:1002:Alice Operator:/home/alice:/bin/tcsh") {
		t.Errorf("alice's account structure not preserved with a redacted hash, got: %s", out)
	}
	// apiary's field is already "*" (a locked account) - must stay "*",
	// never become an empty field, which FreeBSD treats as "no password
	// required."
	if !strings.Contains(out, "apiary:*:1001:1001:Apiary:/home/apiary:/bin/sh") {
		t.Errorf("already-locked account not preserved correctly, got: %s", out)
	}
	if !strings.Contains(out, "# comment line") {
		t.Errorf("comment line not preserved, got: %s", out)
	}
}

func TestRedactRcConf(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "key in the middle",
			input: `apiary_managerd_args="-raftd-socket /var/run/apiary/raftd.sock -peer-api-key apk_Cmd1QjIELyOJR6Ga3262TilBVycyzgl6wkGWPdfJftA -peer-tls -vlan-uplink re0"`,
		},
		{
			name:  "key at the end of the quoted string",
			input: `apiary_managerd_args="-vlan-uplink re0 -peer-api-key apk_Cmd1QjIELyOJR6Ga3262TilBVycyzgl6wkGWPdfJftA"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RedactRcConf(strings.NewReader(c.input))
			if err != nil {
				t.Fatalf("RedactRcConf() error: %v", err)
			}
			out := string(got)
			if strings.Contains(out, "apk_Cmd1QjIELyOJR6Ga3262TilBVycyzgl6wkGWPdfJftA") {
				t.Errorf("real peer-api-key leaked into output: %s", out)
			}
			if !strings.Contains(out, "-peer-api-key REDACTED") {
				t.Errorf("expected -peer-api-key REDACTED, got: %s", out)
			}
			if !strings.Contains(out, "-vlan-uplink re0") {
				t.Errorf("unrelated flags must survive untouched, got: %s", out)
			}
			if strings.HasSuffix(strings.TrimSpace(out), `"REDACTED`) && !strings.HasSuffix(strings.TrimSpace(out), `REDACTED"`) {
				t.Errorf("redaction must not consume the closing quote, got: %s", out)
			}
		})
	}
}

func TestRedactRcConf_NoPeerAPIKeyIsNoOp(t *testing.T) {
	input := `apiary_frontend_args="-tls-cert /path/fullchain.pem -tls-key /path/key.pem"`
	got, err := RedactRcConf(strings.NewReader(input))
	if err != nil {
		t.Fatalf("RedactRcConf() error: %v", err)
	}
	if string(got) != input {
		t.Errorf("RedactRcConf() = %q, want unchanged %q", got, input)
	}
}

func TestExport_WritesThreeRedactedReadableFiles(t *testing.T) {
	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "export")

	rcConfPath := filepath.Join(srcDir, "rc.conf")
	pfConfPath := filepath.Join(srcDir, "pf.conf")
	masterPasswdPath := filepath.Join(srcDir, "master.passwd")

	mustWrite(t, rcConfPath, `apiary_managerd_args="-peer-api-key apk_secretvalue -vlan-uplink re0"`)
	mustWrite(t, pfConfPath, `anchor "apiary/*"`)
	mustWrite(t, masterPasswdPath, "root:$6$realhash:0:0:Charlie &:/root:/bin/csh\n")

	files := Files{RcConfPath: rcConfPath, PfConfPath: pfConfPath, MasterPasswdPath: masterPasswdPath}
	if err := Export(files, outDir); err != nil {
		t.Fatalf("Export() error: %v", err)
	}

	rcOut := mustRead(t, filepath.Join(outDir, "rc.conf.redacted"))
	if strings.Contains(rcOut, "apk_secretvalue") {
		t.Errorf("exported rc.conf still contains the real secret: %s", rcOut)
	}

	pfOut := mustRead(t, filepath.Join(outDir, "pf.conf"))
	if pfOut != `anchor "apiary/*"` {
		t.Errorf("pf.conf should be copied verbatim (no secrets expected in it), got: %s", pfOut)
	}

	passwdOut := mustRead(t, filepath.Join(outDir, "master.passwd.redacted"))
	if strings.Contains(passwdOut, "realhash") {
		t.Errorf("exported master.passwd still contains the real hash: %s", passwdOut)
	}

	for _, name := range []string{"rc.conf.redacted", "pf.conf", "master.passwd.redacted"} {
		info, err := os.Stat(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("Stat(%s) error: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestExport_MissingSourceFileReturnsError(t *testing.T) {
	files := Files{
		RcConfPath:       filepath.Join(t.TempDir(), "does-not-exist"),
		PfConfPath:       filepath.Join(t.TempDir(), "does-not-exist"),
		MasterPasswdPath: filepath.Join(t.TempDir(), "does-not-exist"),
	}
	if err := Export(files, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("Export() with a missing source file = nil error, want one")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}
