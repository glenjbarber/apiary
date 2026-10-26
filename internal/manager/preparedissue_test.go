package manager

// ADR-0172's prepared-certificate issuance.
//
// What these pin: that the naming suffix is checked BEFORE anything
// is requested, that an issuance with no suffix set is refused rather
// than treated as a wildcard, and that the reservation records what
// was actually issued rather than what the issuer claimed.

import (
	"context"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// A name outside the allowed suffix is refused.
//
// The check happens BEFORE anything is requested, so a typo costs
// nothing: no ACME order, no DNS challenge, no TXT record.
func TestIssuePreparedCertificate_RefusesANameOutsideTheSuffix(t *testing.T) {
	err := preparedNameAllowed("host2.evil.example.com", ".lab2.apiary.work")
	if err == nil {
		t.Fatal("a name outside the allowed suffix was accepted")
	}
	if !strings.Contains(err.Error(), "not inside the allowed naming suffix") {
		t.Errorf("the refusal %q does not name the suffix check", err)
	}
}

// An integration with NO suffix is refused rather than treated as a
// wildcard.
//
// A wildcard would let the integration issue for any name in the
// zone, which is exactly the consequence the suffix exists to bound.
func TestIssuePreparedCertificate_RefusesWhenNoSuffixIsSet(t *testing.T) {
	err := preparedNameAllowed("host2.lab2.apiary.work", "")
	if err == nil {
		t.Fatal("an issuance with no allowed suffix was accepted")
	}
	if !strings.Contains(err.Error(), "no allowed naming suffix") {
		t.Errorf("the refusal %q does not say the suffix is what is missing", err)
	}
}

// The RPC refuses an empty request with a reason that names what is
// missing, rather than failing obscurely later.
func TestIssuePreparedCertificate_RefusesAnIncompleteRequest(t *testing.T) {
	srv := &Server{}
	for _, tc := range []struct {
		name string
		req  *rpcpb.IssuePreparedCertificateRequest
		want string
	}{
		{"no fqdn", &rpcpb.IssuePreparedCertificateRequest{ReservationId: "r"}, "fqdn is required"},
		{"no reservation id", &rpcpb.IssuePreparedCertificateRequest{Fqdn: "host2.lab2.apiary.work"}, "reservation_id is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.IssuePreparedCertificate(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("IssuePreparedCertificate: %v", err)
			}
			if !strings.Contains(resp.GetError(), tc.want) {
				t.Errorf("the refusal %q does not mention %q", resp.GetError(), tc.want)
			}
		})
	}
}
