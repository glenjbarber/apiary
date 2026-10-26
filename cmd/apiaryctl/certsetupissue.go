package main

// The walkthrough's issuance call.
//
// The issuance itself is NOT reimplemented here. managerd owns the
// ACME issuer and the certificate directory, and the Machine page's
// ACME panel already issues through it, so the walkthrough calls the
// same RPC. One implementation of issuance means a certificate
// obtained from the console and one obtained from the page are
// produced by the same code, with the same checks.
//
// What the walkthrough adds is the part before it - the token, the
// zone, the suffix, the preview - and the part after it, which is
// different for the two steps: a certificate for THIS Comb becomes
// this Comb's serving certificate, while a certificate for ANOTHER
// Comb becomes a PREPARED pair that the join flow delivers.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/peerca"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

// issueThroughManagerd asks this Comb's own managerd to issue.
//
// It dials managerd over TLS when this Comb serves TLS, and in
// plaintext when it does not - the same choice
// internal/manager's own loopback dialing makes, and the same one the
// Machine page's panel makes, because it is the same managerd.
func (s *certSetupState) issueThroughManagerd(ctx context.Context, forThisComb bool) error {
	client, closeFn, err := dialLocalManagerd()
	if err != nil {
		return err
	}
	defer closeFn()

	ctx, cancel := context.WithTimeout(ctx, issueTimeout)
	defer cancel()

	if forThisComb {
		return s.issueForThisComb(ctx, client)
	}
	return s.issueForAnotherComb(ctx, client)
}

// issueForThisComb issues a certificate for this Comb's own serving
// certificate.
//
// The name it is issued for must be the name this Comb's tls_cert and
// tls_key already name, because managerd's issuance installs the pair
// at those paths and refuses otherwise. That refusal is managerd's,
// and it is a good one: a certificate installed somewhere other than
// where the service reads it would be a certificate nothing serves.
func (s *certSetupState) issueForThisComb(ctx context.Context, client rpcpb.ManagerServiceClient) error {
	resp, err := client.IssueLetsEncryptCertificate(ctx, &rpcpb.IssueLetsEncryptCertificateRequest{
		Name:      s.Name,
		Hostnames: []string{s.Name},
		AutoRenew: true,
	})
	if err != nil {
		return fmt.Errorf("asking managerd to issue the certificate: %w", err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("managerd refused the issuance: %s", resp.GetError())
	}
	return nil
}

// issueForAnotherComb prepares a certificate for a Comb that has not
// joined yet.
//
// The prepared pair is recorded as a RESERVATION (ADR-0169), and the
// join flow will bind it to the joining Comb and deliver it over the
// join channel (ADR-0171). The walkthrough does not deliver it and
// does not bind it: it prepares, and the join flow decides.
//
// The preparation needs a bootstrap key, because ACME never delivers
// one: the key is the requester's, and the CA only signs a CSR made
// with it. A certificate prepared before the joiner exists therefore
// has a private key the PREPARER generated, and that key has two
// holders until the joiner replaces it. The walkthrough says so,
// because an operator who does not know that would be surprised by it
// later.
func (s *certSetupState) issueForAnotherComb(ctx context.Context, client rpcpb.ManagerServiceClient) error {
	// The reservation ID is generated HERE, by the walkthrough, because
	// it is what the joining Comb presents and it must be unguessable.
	// It is generated from crypto/rand rather than derived from the
	// name, so a name that is already public does not make the
	// reservation guessable.
	reservationID, err := newReservationID()
	if err != nil {
		return err
	}
	resp, err := client.IssuePreparedCertificate(ctx, &rpcpb.IssuePreparedCertificateRequest{
		Fqdn:            s.Name,
		ReservationId:   reservationID,
		ValiditySeconds: int64(s.PreparedValidity / time.Second),
	})
	if err != nil {
		return fmt.Errorf("asking managerd to prepare the certificate: %w", err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("managerd refused the preparation: %s", resp.GetError())
	}
	fmt.Fprintf(s.Out, "Prepared %s. The reservation ID is %s.\n", s.Name, resp.GetReservation().GetReservationId())
	fmt.Fprintln(s.Out, "Give that reservation ID to the joining Comb. It collects the certificate during the join.")
	if warning := resp.GetBootstrapKeyWarning(); warning != "" {
		fmt.Fprintf(s.Out, "Note: %s\n", warning)
	}
	return nil
}

// dialLocalManagerd dials this Comb's own managerd.
//
// The address comes from this Comb's own managerd.json, and the
// transport follows managerd's own configuration: TLS when the config
// says managerd serves TLS, plaintext when it says otherwise. Guessing
// either way would be the wrong answer for one of the two.
func dialLocalManagerd() (rpcpb.ManagerServiceClient, func(), error) {
	manager := &nodeconfig.Manager{Path: nodeconfig.DefaultPath}
	cfg, err := manager.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("reading this Comb's managerd configuration: %w", err)
	}
	address := cfg.RPCAddr
	if address == "" {
		address = "127.0.0.1:17700"
	}
	// managerd serves TLS when it has a serving certificate, which is
	// the same rule cmd/managerd's own startup applies. Guessing
	// either way would be wrong for one of the two configurations, so
	// the certificate's presence decides.
	//
	// The CA is the derived peer-ca.pem, because this is a connection
	// to this Comb's OWN managerd and the derived file is what every
	// Comb already writes for exactly this purpose.
	caPath := peerca.DefaultPath
	if _, statErr := os.Stat(caPath); statErr != nil {
		return nil, nil, fmt.Errorf("reading the trust anchors at %s: %w. This Comb cannot reach its own managerd without them", caPath, statErr)
	}
	dialOpt, err := tlsdial.ManagerDialOption(cfg.TLSCert != "", caPath, "")
	if err != nil {
		return nil, nil, fmt.Errorf("configuring the connection to managerd: %w", err)
	}
	conn, err := grpc.NewClient(address, dialOpt)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing managerd at %s: %w", address, err)
	}
	return rpcpb.NewManagerServiceClient(conn), func() { conn.Close() }, nil
}

// certSetupNow is the clock the walkthrough stamps records with.
//
// It is a variable so a test can fix it, rather than a direct call to
// time.Now.
var certSetupNow = time.Now

// newReservationID generates an unguessable reservation identifier.
//
// It is 32 bytes of crypto/rand, base64url-encoded without padding.
// It is generated rather than derived from the name, because the name
// is public and the identifier is what the joining Comb presents.
func newReservationID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating the reservation identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
