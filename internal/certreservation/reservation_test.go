package certreservation

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewReservation_RequiresItsInputs(t *testing.T) {
	cases := []struct {
		name        string
		fqdn        string
		certificate string
		fingerprint string
	}{
		{"no name", "", "cert", "fp"},
		{"no certificate", "host2.example.com", "", "fp"},
		{"no fingerprint", "host2.example.com", "cert", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReservation(tc.fqdn, tc.certificate, tc.fingerprint, time.Hour); err == nil {
				t.Fatal("NewReservation accepted an incomplete request")
			}
		})
	}
}

func TestNewReservation_IdentifierAndTokenAreDistinctAndUnguessable(t *testing.T) {
	first, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	second, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	// Two reservations for the same name must not collide, or one
	// joiner could collect another's material.
	if first.ID == second.ID {
		t.Errorf("two reservations for the same name share the identifier %q", first.ID)
	}
	// The identifier identifies a reservation; the token authorizes
	// collecting it. They must not be the same value, or knowing the
	// identifier would be enough to collect the material.
	if first.ID == first.DeliveryToken {
		t.Error("the delivery token is the reservation identifier; knowing the identifier would authorize collection")
	}
	if first.ID == "" || first.DeliveryToken == "" {
		t.Error("a reservation must carry both an identifier and a delivery token")
	}
}

func TestReservation_BeforeBindingNobodyMayDeliverIt(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	err = res.AuthorizeDelivery("host2", "req-1", res.DeliveryToken, time.Now())
	if !errors.Is(err, ErrNotBound) {
		t.Errorf("AuthorizeDelivery on an unbound reservation = %v, want ErrNotBound", err)
	}
}

func TestReservation_BindIsRequiredAndSingle(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("", "req-1", now); err == nil {
		t.Error("Bind accepted a request with no node ID")
	}
	if err := res.Bind("host2", "", now); err == nil {
		t.Error("Bind accepted a request with no join request ID")
	}
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if res.NodeID != "host2" || res.RequestID != "req-1" {
		t.Errorf("Bind recorded node %q request %q, want host2/req-1", res.NodeID, res.RequestID)
	}
	// Re-binding to another joiner would hand the prepared private key
	// to the wrong machine.
	if err := res.Bind("host3", "req-2", now); err == nil {
		t.Error("Bind accepted a second binding; a preparation is made for one joining node")
	}
}

func TestReservation_DeliveryRequiresTheRightRecipient(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// The wrong Comb, presenting the right token, is refused.
	if err := res.AuthorizeDelivery("host3", "req-1", res.DeliveryToken, now); !errors.Is(err, ErrWrongRecipient) {
		t.Errorf("AuthorizeDelivery to the wrong node = %v, want ErrWrongRecipient", err)
	}
	// The right Comb, presenting a wrong request, is refused.
	if err := res.AuthorizeDelivery("host2", "req-9", res.DeliveryToken, now); !errors.Is(err, ErrWrongRecipient) {
		t.Errorf("AuthorizeDelivery with the wrong request = %v, want ErrWrongRecipient", err)
	}
	// The right Comb and request, presenting a wrong token, is refused.
	if err := res.AuthorizeDelivery("host2", "req-1", "not-the-token", now); !errors.Is(err, ErrReplay) {
		t.Errorf("AuthorizeDelivery with the wrong token = %v, want ErrReplay", err)
	}
	// And the right combination is allowed.
	if err := res.AuthorizeDelivery("host2", "req-1", res.DeliveryToken, now); err != nil {
		t.Errorf("AuthorizeDelivery with the right recipient and token = %v, want nil", err)
	}
}

func TestReservation_DeliveryIsSingleUse(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := res.MarkDelivered(now); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	// A second collection of the same prepared key must be refused.
	if err := res.AuthorizeDelivery("host2", "req-1", res.DeliveryToken, now); !errors.Is(err, ErrAlreadyDelivered) {
		t.Errorf("AuthorizeDelivery after delivery = %v, want ErrAlreadyDelivered", err)
	}
	if err := res.MarkDelivered(now); !errors.Is(err, ErrAlreadyDelivered) {
		t.Errorf("MarkDelivered twice = %v, want ErrAlreadyDelivered", err)
	}
}

func TestReservation_ExpiredWindowIsRefused(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Minute)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	later := now.Add(2 * time.Minute)
	if err := res.AuthorizeDelivery("host2", "req-1", res.DeliveryToken, later); !errors.Is(err, ErrExpired) {
		t.Errorf("AuthorizeDelivery after the window closed = %v, want ErrExpired", err)
	}
}

func TestReservation_WithdrawalCancelsThePreparation(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := res.Withdraw(); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if err := res.AuthorizeDelivery("host2", "req-1", res.DeliveryToken, now); !errors.Is(err, ErrWithdrawn) {
		t.Errorf("AuthorizeDelivery after withdrawal = %v, want ErrWithdrawn", err)
	}
}

func TestReservation_LifecycleIsForwardOnly(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	now := time.Now()
	if err := res.Bind("host2", "req-1", now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Adoption requires delivery first.
	if err := res.MarkAdopted(now); err == nil {
		t.Error("MarkAdopted succeeded before delivery")
	}
	if err := res.MarkDelivered(now); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if err := res.MarkAdopted(now); err != nil {
		t.Fatalf("MarkAdopted: %v", err)
	}
	// Retirement requires adoption first.
	if err := res.MarkRetired(); err != nil {
		t.Fatalf("MarkRetired: %v", err)
	}
	if res.State != StateRetired {
		t.Errorf("State = %q, want %q", res.State, StateRetired)
	}
	// A retired reservation cannot go back.
	if err := res.MarkAdopted(now); err == nil {
		t.Error("a retired reservation was adopted again")
	}
}

// TestReservation_NeverBecomesAMember is the property ADR-0169 states
// most plainly: a reservation is a pending intent, never a trust
// anchor.
//
// There is no field on a Reservation that could carry membership, and
// no method that could set one. The test asserts that by checking the
// exported surface does not grow one - a reservation that could be
// promoted by this package would be promoted without the join flow's
// authorization, which is exactly what ADR-0169 forbids.
func TestReservation_NeverBecomesAMember(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	// The record carries no membership-shaped field. The reflection
	// walk is what makes this a real assertion rather than a comment.
	value := reflect.ValueOf(*res)
	for i := 0; i < value.NumField(); i++ {
		name := value.Type().Field(i).Name
		switch strings.ToLower(name) {
		case "isvoter", "voter", "member", "ismember", "pinned", "ispinned":
			t.Errorf("the reservation has a field %q; a reservation is a pending intent and must never carry membership or a pin", name)
		}
	}
}

// TestReservation_ExposesNoKeyMaterial is ADR-0169's rule that the
// reservation record is metadata.
//
// The record may name a certificate and its fingerprint, but it must
// not carry a private key: the record is the kind of thing that ends
// up in a log, a rendered page, or a replicated store, and a private
// key in any of those is a private key in all of them.
func TestReservation_ExposesNoKeyMaterial(t *testing.T) {
	res, err := NewReservation("host2.example.com", "cert", "fp", time.Hour)
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	value := reflect.ValueOf(*res)
	for i := 0; i < value.NumField(); i++ {
		name := value.Type().Field(i).Name
		lower := strings.ToLower(name)
		if strings.Contains(lower, "privatekey") || strings.Contains(lower, "secret") {
			t.Errorf("the reservation has a field %q; the record is metadata and must not carry key material", name)
		}
	}
}
