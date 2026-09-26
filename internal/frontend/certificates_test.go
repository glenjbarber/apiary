package frontend

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"html"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/certmgr"
	"github.com/glenjbarber/apiary/internal/origincert"
)

// writeTestPair writes a real certificate/key pair into dir with
// origincert.WritePair - the same transaction production uses - and
// returns the two paths. Everything the page renders about a
// certificate is read back out of the resulting file, so a test that
// passed against a hand-written fake PEM would prove nothing about the
// actual read path.
func writeTestPair(t *testing.T, dir, name string, notBefore, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "api.example.com"},
		DNSNames:     []string{"api.example.com"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encoding key: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	certPath, keyPath, err = origincert.WritePair(dir, name, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("WritePair: %v", err)
	}
	return certPath, keyPath
}

// renderCertificatesPage drives the real handler end to end and returns
// the rendered body, so every assertion below is about what an operator
// would actually see.
func renderCertificatesPage(t *testing.T, client *fakeClient) string {
	t.Helper()
	rec := httptest.NewRecorder()
	newTestServer(t, client).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/certificates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// certificateRows returns just the verdict table's rows. The page's own
// legend deliberately shows each badge class once, so a scan over the
// whole body cannot distinguish "a certificate rendered as ok" from "the
// page explains what ok means". Scoping to the rows is what makes the
// negative assertions below mean what they say.
func certificateRows(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<tbody id="certificate-rows">`)
	if start < 0 {
		t.Fatalf("certificates page has no rows table:\n%s", body)
	}
	rest := body[start:]
	end := strings.Index(rest, "</tbody>")
	if end < 0 {
		t.Fatalf("certificates page rows table is unterminated:\n%s", body)
	}
	return rest[:end]
}

func TestServer_CertificatesPage_RendersEveryVerdictFromTheRealFile(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	// The extra hour on each expiry is slack for the remaining-days
	// rendering, which truncates toward zero on purpose - see
	// describeCertificateRemaining's own boundary test for why it
	// rounds the direction it does. Without the slack a certificate
	// written for "200 days out" would render 199 the moment the
	// handler's clock ticks forward, which is correct and useless as
	// a fixture.
	okPath, _ := writeTestPair(t, dir, "healthy", now.Add(-300*24*time.Hour), now.Add(200*24*time.Hour+time.Hour))
	soonPath, _ := writeTestPair(t, dir, "soon", now.Add(-300*24*time.Hour), now.Add(9*24*time.Hour+time.Hour))
	expiredPath, _ := writeTestPair(t, dir, "expired", now.Add(-400*24*time.Hour), now.Add(-3*24*time.Hour))

	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"}, listOriginCertificatesResp: &rpcpb.ListOriginCertificatesResponse{
		Certificates: []*rpcpb.OriginCertificateInfo{
			{Name: "healthy", Service: "apiary_managerd", Hostnames: []string{"api.example.com"},
				CertPath: okPath, ExpiresAtUnix: now.Add(200 * 24 * time.Hour).Unix(), AutoRenew: true},
			{Name: "soon", Service: "apiary_managerd", Hostnames: []string{"old.example.com"},
				CertPath: soonPath, ExpiresAtUnix: now.Add(9 * 24 * time.Hour).Unix()},
			{Name: "expired", Service: "apiary_frontend", Hostnames: []string{"gone.example.com"},
				CertPath: expiredPath, ExpiresAtUnix: now.Add(-3 * 24 * time.Hour).Unix()},
		},
	}}
	body := renderCertificatesPage(t, client)

	for _, want := range []string{
		`class="badge ok"`, `class="badge soon"`, `class="badge expired"`,
		"node-a", "healthy", "soon", "expired",
		"apiary_frontend", "gone.example.com",
		"200 days left", "9 days left", "expired 3 days ago",
		// The lifetime read out of the file, not the recorded copy.
		"valid for 500 days",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("certificates page missing %q\nbody:\n%s", want, body)
		}
	}
	// The scope statement is part of the deliverable, not decoration:
	// this page is one Comb's view and must never imply otherwise.
	if !strings.Contains(body, "not a Colony-wide view") {
		t.Errorf("certificates page does not disclose its single-Comb scope:\n%s", body)
	}
}

// TestServer_CertificatesPage_UnreadableFileIsUnknownNotExpired is the
// headline case. The inventory still holds a recorded expiry a year out,
// so anything that fell back to the recorded copy - or that treated a
// failed read as a failure of the certificate - would render a green or
// a red badge. Both are wrong; the honest answer is unknown, carrying the
// reason.
func TestServer_CertificatesPage_UnreadableFileIsUnknownNotExpired(t *testing.T) {
	now := time.Now()
	missing := filepath.Join(t.TempDir(), "deleted.crt")
	client := &fakeClient{listOriginCertificatesResp: &rpcpb.ListOriginCertificatesResponse{
		Certificates: []*rpcpb.OriginCertificateInfo{{
			Name: "managerd", Service: "apiary_managerd", Hostnames: []string{"api.example.com"},
			CertPath: missing, ExpiresAtUnix: now.Add(365 * 24 * time.Hour).Unix(),
		}},
	}}
	body := renderCertificatesPage(t, client)

	rows := certificateRows(t, body)
	if !strings.Contains(rows, `class="badge unknown"`) {
		t.Errorf("a missing certificate file did not render an unknown badge:\n%s", rows)
	}
	if strings.Contains(rows, `class="badge ok"`) {
		t.Errorf("a check that could not be made rendered as ok - the exact false negative this page exists to prevent:\n%s", rows)
	}
	if strings.Contains(rows, `class="badge expired"`) {
		t.Errorf("an unreadable certificate rendered as expired - a file Apiary cannot read is not an expired certificate:\n%s", rows)
	}
	// The recorded value is still shown - an operator debugging a
	// missing file wants to know what was last believed - but as
	// context, and the reason for the unknown has to be on the page.
	if !strings.Contains(body, "Expiry could not be determined") {
		t.Errorf("unknown row did not say the check could not be made:\n%s", body)
	}
	if !strings.Contains(html.UnescapeString(body), "no such file or directory") {
		t.Errorf("unknown row did not carry the read error verbatim:\n%s", body)
	}
}

// TestServer_CertificatesPage_UnparseableFileIsUnknown covers the other
// direction: the file exists and is readable but is not a certificate.
// Still a failed check, still not a verdict about the certificate.
func TestServer_CertificatesPage_UnparseableFileIsUnknown(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.crt")
	// Valid base64 inside a valid PEM header, so the failure is a DER
	// parse failure rather than a "there is no PEM here" one - the
	// realistic shape of a truncated or corrupted certificate file.
	body := "-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString([]byte("this is not a DER certificate")) + "\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(garbage, []byte(body), 0o644); err != nil {
		t.Fatalf("writing garbage certificate: %v", err)
	}
	client := &fakeClient{listOriginCertificatesResp: &rpcpb.ListOriginCertificatesResponse{
		Certificates: []*rpcpb.OriginCertificateInfo{{
			Name: "managerd", Service: "apiary_managerd", CertPath: garbage,
			ExpiresAtUnix: time.Now().Add(200 * 24 * time.Hour).Unix(),
		}},
	}}
	rendered := renderCertificatesPage(t, client)
	rows := certificateRows(t, rendered)
	if !strings.Contains(rows, `class="badge unknown"`) {
		t.Errorf("unparseable certificate did not render an unknown badge:\n%s", rows)
	}
	if strings.Contains(rows, `class="badge ok"`) || strings.Contains(rows, `class="badge expired"`) {
		t.Errorf("unparseable certificate rendered a definite verdict:\n%s", rows)
	}
	if !strings.Contains(html.UnescapeString(rendered), "parsing certificate") {
		t.Errorf("unknown row did not carry the parse error verbatim:\n%s", rendered)
	}
}

// TestServer_CertificatesPage_NeverObservedIsUnknown covers the third
// failure mode: an inventory entry that never recorded a certificate
// path at all. There is nothing to read, so nothing is known - and
// "never recorded" is rendered rather than left as a blank cell, so an
// operator can see the gap.
func TestServer_CertificatesPage_NeverObservedIsUnknown(t *testing.T) {
	client := &fakeClient{listOriginCertificatesResp: &rpcpb.ListOriginCertificatesResponse{
		Certificates: []*rpcpb.OriginCertificateInfo{{
			Name: "orphan", Service: "apiary_managerd", CertPath: "",
		}},
	}}
	body := renderCertificatesPage(t, client)
	rows := certificateRows(t, body)
	if !strings.Contains(rows, `class="badge unknown"`) {
		t.Errorf("an entry with no certificate path did not render as unknown:\n%s", rows)
	}
	if strings.Contains(rows, `class="badge expired"`) {
		t.Errorf("an entry with no recorded expiry rendered as expired:\n%s", rows)
	}
	if got := strings.Count(rows, "never recorded"); got < 2 {
		t.Errorf("want both the recorded expiry and the recorded timestamp rendered as 'never recorded', got %d:\n%s", got, rows)
	}
	if !strings.Contains(html.UnescapeString(body), "no certificate file path is recorded") {
		t.Errorf("never-observed row did not carry its reason:\n%s", body)
	}
}

// TestServer_CertificatesPage_FetchFailureFailsSoft is the house
// convention (currentClusterISOs, currentNetworks, currentPlacementHives):
// a failed fetch yields an error message and a page that still renders,
// never a 500. The message has to say that nothing is known here - an
// operator who sees an empty table must not conclude the Comb holds no
// certificates.
func TestServer_CertificatesPage_FetchFailureFailsSoft(t *testing.T) {
	client := &fakeClient{listOriginCertificatesResp: &rpcpb.ListOriginCertificatesResponse{
		Error: "reading certificate inventory: permission denied",
	}}
	body := renderCertificatesPage(t, client)
	if !strings.Contains(body, "reading certificate inventory: permission denied") {
		t.Errorf("failed fetch did not surface its own error:\n%s", body)
	}
	if !strings.Contains(body, "no certificate state is known here") {
		t.Errorf("failed fetch did not say that nothing is known, rather than implying an empty inventory:\n%s", body)
	}
	if !strings.Contains(body, "Certificates") || !strings.Contains(body, "/certificates") {
		t.Errorf("page did not render at all on a failed fetch:\n%s", body)
	}
}

// TestServer_CertificatesPage_NeverRendersPrivateKeyMaterial is the key
// custody proof at the surface. A real private key file sits next to the
// certificate, the inventory reports its path, and the rendered page is
// scanned for: the key's own PEM, its distinctive base64 body, its
// path, and the PEM type marker. Nothing about the key may reach an
// operator's browser, because a key stays local to the Comb that serves
// the certificate (ADR-0133 D3) and is never replicated or displayed.
func TestServer_CertificatesPage_NeverRendersPrivateKeyMaterial(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	certPath, keyPath := writeTestPair(t, dir, "managerd", now.Add(-100*24*time.Hour), now.Add(200*24*time.Hour))
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading key: %v", err)
	}
	client := &fakeClient{listOriginCertificatesResp: &rpcpb.ListOriginCertificatesResponse{
		Certificates: []*rpcpb.OriginCertificateInfo{{
			Name: "managerd", Service: "apiary_managerd", Hostnames: []string{"api.example.com"},
			CertPath: certPath, KeyPath: keyPath,
			ExpiresAtUnix: now.Add(200 * 24 * time.Hour).Unix(), AutoRenew: true,
		}},
	}}
	body := renderCertificatesPage(t, client)
	unescaped := html.UnescapeString(body)

	if !strings.Contains(body, `class="badge ok"`) {
		t.Fatalf("expected a definite ok verdict from a real, readable certificate:\n%s", body)
	}
	if !strings.Contains(body, certPath) {
		t.Errorf("page did not show the certificate path it read:\n%s", body)
	}
	for _, forbidden := range []string{"PRIVATE KEY", "BEGIN EC PRIVATE KEY", "PRIVATE KEY-----", keyPath, strings.TrimSpace(base64Body(t, keyBytes))} {
		if forbidden == "" {
			continue
		}
		if strings.Contains(unescaped, forbidden) {
			t.Errorf("rendered page contains key material (%q)", forbidden)
		}
	}
	// Every individual base64 line of the key, checked on its own, so a
	// partial or reformatted leak is caught too.
	for _, line := range strings.Split(string(keyBytes), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 20 || strings.HasPrefix(line, "-----") {
			continue
		}
		if strings.Contains(unescaped, line) {
			t.Errorf("rendered page contains a line of the private key: %q", line)
		}
	}
}

// base64Body returns the key PEM's payload with its line breaks removed,
// so the scan can look for it as one string as well as line by line.
func base64Body(t *testing.T, keyPEM []byte) string {
	t.Helper()
	var body strings.Builder
	for _, line := range strings.Split(string(keyPEM), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}
		body.WriteString(line)
	}
	return body.String()
}

// TestServer_CertificatesPage_ThresholdComesFromTheNamedConstant: the
// page states its own policy, and states it from certmgr.ExpiryWindow
// rather than from a number typed into a template. Changing the constant
// changes the page.
func TestServer_CertificatesPage_ThresholdComesFromTheNamedConstant(t *testing.T) {
	body := renderCertificatesPage(t, &fakeClient{})
	if !strings.Contains(html.UnescapeString(body), certmgr.ExpiryWindow.String()) {
		t.Errorf("page does not state the expiry threshold from certmgr.ExpiryWindow (%s):\n%s", certmgr.ExpiryWindow, body)
	}
	if !strings.Contains(html.UnescapeString(body), "reporting threshold, not a safety margin") {
		t.Errorf("page does not explain what the threshold means:\n%s", body)
	}
}

// TestServer_CertificatesPage_NoCertificateIsHealthyWithoutARead: a
// Comb whose certificate file cannot be read must never produce a
// healthy-looking row, in either badge class or prose. This is the
// invariant a reviewer should be able to check by reading one test.
func TestServer_CertificatesPage_NoCertificateIsHealthyWithoutARead(t *testing.T) {
	dir := t.TempDir()
	entries := []*rpcpb.OriginCertificateInfo{
		{Name: "a", CertPath: filepath.Join(dir, "a.crt"), ExpiresAtUnix: time.Now().Add(90 * 24 * time.Hour).Unix()},
		{Name: "b", CertPath: filepath.Join(dir, "b.crt"), ExpiresAtUnix: time.Now().Add(90 * 24 * time.Hour).Unix()},
		{Name: "c", ExpiresAtUnix: time.Now().Add(90 * 24 * time.Hour).Unix()},
	}
	body := renderCertificatesPage(t, &fakeClient{listOriginCertificatesResp: &rpcpb.ListOriginCertificatesResponse{Certificates: entries}})
	rows := certificateRows(t, body)
	if got := strings.Count(rows, `class="badge unknown"`); got != len(entries) {
		t.Errorf("got %d unknown verdict badges for %d unreadable certificates, want every one unknown:\n%s", got, len(entries), rows)
	}
	for _, forbidden := range []string{`class="badge ok"`, `class="badge soon"`, `class="badge expired"`} {
		if strings.Contains(rows, forbidden) {
			t.Errorf("no readable certificate produced %s; nothing here may look healthy or expired:\n%s", forbidden, rows)
		}
	}
}

// TestServer_CertificatesPage_EmptyInventoryIsNotAnError: an empty
// inventory is a fact, not a failure, and the page says so - including
// the caveat that an absent inventory.json and an empty one are
// indistinguishable, so the operator is not invited to conclude a Comb
// that has never issued anything is misconfigured.
func TestServer_CertificatesPage_EmptyInventoryIsNotAnError(t *testing.T) {
	body := renderCertificatesPage(t, &fakeClient{})
	if !strings.Contains(body, "No certificates are recorded") {
		t.Errorf("empty inventory did not render its own empty state:\n%s", body)
	}
	if strings.Contains(body, `class="banner-error"`) {
		t.Errorf("an empty inventory rendered as an error:\n%s", body)
	}
	if !strings.Contains(body, "unconfigured") {
		t.Errorf("empty state does not mention that unconfigured is the expected case here:\n%s", body)
	}
}

// TestDescribeCertificateRemaining pins the wording and the rounding
// direction of the one place a day count is produced. Rounding toward
// zero in both directions is the point: the text must never claim more
// time than a certificate actually has, and must never say "expired 0
// days ago" for something that expired this morning.
func TestDescribeCertificateRemaining(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		expires time.Time
		want    string
	}{
		{name: "years of life left", expires: now.Add(400 * 24 * time.Hour), want: "400 days left"},
		{name: "a day and a half left truncates down", expires: now.Add(36 * time.Hour), want: "1 days left"},
		{name: "just under a day left", expires: now.Add(23 * time.Hour), want: "expires in under a day"},
		{name: "exactly now", expires: now, want: "expired less than a day ago"},
		{name: "expired this morning", expires: now.Add(-6 * time.Hour), want: "expired less than a day ago"},
		{name: "expired three days ago", expires: now.Add(-3 * 24 * time.Hour), want: "expired 3 days ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeCertificateRemaining(tc.expires, now); got != tc.want {
				t.Errorf("describeCertificateRemaining() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCertificateExpiryBadgeClassNeverRendersUnknownAsHealthy is the
// class-mapping half of the same invariant, checked directly so it does
// not depend on how the template happens to spell a badge today. There
// is no fourth colour here: the mapping has three definite values and
// one grey, and unknown is the grey one by construction.
func TestCertificateExpiryBadgeClassNeverRendersUnknownAsHealthy(t *testing.T) {
	cases := map[origincert.ExpiryStatus]string{
		origincert.ExpiryOK:      "ok",
		origincert.ExpirySoon:    "soon",
		origincert.ExpiryExpired: "expired",
		origincert.ExpiryUnknown: "unknown",
	}
	for status, want := range cases {
		if got := certificateExpiryBadgeClass(status); got != want {
			t.Errorf("certificateExpiryBadgeClass(%q) = %q, want %q", status, got, want)
		}
	}
	// An unrecognised status must fall through to unknown rather than
	// to the zero value of any healthy-looking class.
	if got := certificateExpiryBadgeClass(origincert.ExpiryStatus("something-new")); got != "unknown" {
		t.Errorf("an unrecognised status rendered as %q, want unknown", got)
	}
}
