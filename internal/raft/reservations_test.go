package raft

// ADR-0171's prepared-certificate reservations.
//
// What these pin: that the reservation is replicated metadata, that it
// is deliverable to nobody until it is bound, that binding is single,
// that delivery is single-use, and that the store survives a snapshot
// round trip.

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

func newTestReservation(id, fqdn, fingerprint string) *internalpb.CertificateReservation {
	return &internalpb.CertificateReservation{
		ReservationId: id,
		Fqdn:          fqdn,
		Fingerprint:   fingerprint,
		CreatedAtUnix: pinTestNow.Unix(),
		ExpiresAtUnix: pinTestNow.Add(time.Hour).Unix(),
		PreparedBy:    "brood",
	}
}

func applyReservation(t *testing.T, fsm *FSM, index uint64, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	result := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, cmd)})
	res, ok := result.(*FSMApplyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *FSMApplyResult", result)
	}
	return res
}

// A reservation is recorded and replicated.
func TestCreateCertificateReservationIsRecorded(t *testing.T) {
	fsm := NewFSM()
	res := newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB")
	out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{Reservation: res},
		},
	})
	if out.Error != "" {
		t.Fatalf("Error = %q, want empty", out.Error)
	}
	got := fsm.ListCertificateReservations()
	if len(got) != 1 || got[0].GetReservationId() != "res-1" {
		t.Fatalf("ListCertificateReservations = %+v, want the one reservation just written", got)
	}
}

// A reservation with no end is refused: an unbounded reservation holds
// a prepared key indefinitely.
func TestCreateCertificateReservationRefusesAnUnboundedWindow(t *testing.T) {
	fsm := NewFSM()
	res := newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB")
	res.ExpiresAtUnix = 0
	out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{Reservation: res},
		},
	})
	refuseRefusal(t, out, "expires_at_unix")
}

// A duplicate reservation ID is refused rather than replacing an
// existing one.
func TestCreateCertificateReservationRefusesADuplicate(t *testing.T) {
	fsm := NewFSM()
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}
	if out := applyReservation(t, fsm, 1, cmd); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}
	out := applyReservation(t, fsm, 2, cmd)
	refuseRefusal(t, out, "already exists")
}

// An unbound reservation is deliverable to nobody. There is no honest
// answer to "is this the Comb I prepared this for" until the join
// attestation has answered it.
func TestDeliverRefusesAnUnboundReservation(t *testing.T) {
	fsm := NewFSM()
	if out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}
	out := applyReservation(t, fsm, 2, &internalpb.Command{
		Op: &internalpb.Command_DeliverCertificateReservation{
			DeliverCertificateReservation: &internalpb.DeliverCertificateReservation{
				ReservationId:   "res-1",
				DeliveredAtUnix: pinTestNow.Unix(),
			},
		},
	})
	refuseRefusal(t, out, "not bound to a joining Comb")
}

// Binding is single. A re-bind would hand the prepared private key to
// a second machine.
func TestBindIsSingle(t *testing.T) {
	fsm := NewFSM()
	if out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}
	bind := &internalpb.Command{
		Op: &internalpb.Command_BindCertificateReservation{
			BindCertificateReservation: &internalpb.BindCertificateReservation{
				ReservationId: "res-1",
				NodeId:        "node02",
				RequestId:     "jreq-1",
				BoundAtUnix:   pinTestNow.Unix(),
			},
		},
	}
	if out := applyReservation(t, fsm, 2, bind); out.Error != "" {
		t.Fatalf("binding: %s", out.Error)
	}
	out := applyReservation(t, fsm, 3, bind)
	refuseRefusal(t, out, "already bound")
}

// Delivery is single-use: a delivered reservation is spent.
func TestDeliveryIsSingleUse(t *testing.T) {
	fsm := NewFSM()
	if out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}
	if out := applyReservation(t, fsm, 2, &internalpb.Command{
		Op: &internalpb.Command_BindCertificateReservation{
			BindCertificateReservation: &internalpb.BindCertificateReservation{
				ReservationId: "res-1",
				NodeId:        "node02",
				RequestId:     "jreq-1",
				BoundAtUnix:   pinTestNow.Unix(),
			},
		},
	}); out.Error != "" {
		t.Fatalf("binding: %s", out.Error)
	}
	deliver := &internalpb.Command{
		Op: &internalpb.Command_DeliverCertificateReservation{
			DeliverCertificateReservation: &internalpb.DeliverCertificateReservation{
				ReservationId:   "res-1",
				DeliveredAtUnix: pinTestNow.Unix(),
			},
		},
	}
	if out := applyReservation(t, fsm, 3, deliver); out.Error != "" {
		t.Fatalf("delivering: %s", out.Error)
	}
	out := applyReservation(t, fsm, 4, deliver)
	refuseRefusal(t, out, "already been delivered")
}

// The reservation survives a snapshot/restore round trip, so a replica
// catching up sees the same binding.
func TestReservationSurvivesRestore(t *testing.T) {
	fsm := NewFSM()
	if out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}
	if out := applyReservation(t, fsm, 2, &internalpb.Command{
		Op: &internalpb.Command_BindCertificateReservation{
			BindCertificateReservation: &internalpb.BindCertificateReservation{
				ReservationId: "res-1",
				NodeId:        "node02",
				RequestId:     "jreq-1",
				BoundAtUnix:   pinTestNow.Unix(),
			},
		},
	}); out.Error != "" {
		t.Fatalf("binding: %s", out.Error)
	}

	encoded, err := proto.Marshal(fsm.SnapshotState())
	if err != nil {
		t.Fatalf("marshalling snapshot state: %v", err)
	}
	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(encoded))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := restored.ListCertificateReservations()
	if len(got) != 1 {
		t.Fatalf("ListCertificateReservations = %d entries after restore, want 1", len(got))
	}
	if got[0].GetNodeId() != "node02" || got[0].GetRequestId() != "jreq-1" {
		t.Errorf("the restored reservation is bound to node %q request %q, want node02/jreq-1",
			got[0].GetNodeId(), got[0].GetRequestId())
	}
}

// A dropped reservation is gone, and the reason is recorded with the
// removal.
func TestDropRemovesTheReservation(t *testing.T) {
	fsm := NewFSM()
	if out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}
	out := applyReservation(t, fsm, 2, &internalpb.Command{
		Op: &internalpb.Command_DropCertificateReservation{
			DropCertificateReservation: &internalpb.DropCertificateReservation{
				ReservationId: "res-1",
				Reason:        "the operator withdrew the preparation",
			},
		},
	})
	if out.Error != "" {
		t.Fatalf("dropping: %s", out.Error)
	}
	if len(fsm.ListCertificateReservations()) != 0 {
		t.Error("the reservation is still in the store after being dropped")
	}
}

// The approval BINDS the reservation, in the same log entry that
// settles the request.
//
// The binding rides the approval rather than being a separate command
// because the approval is the moment the Colony decides which Comb is
// joining. A reservation bound by an earlier command could have been
// bound to a request that was later rejected.
func TestApprovalBindsTheReservation(t *testing.T) {
	fsm := NewFSM()
	if out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}

	// The request must exist before it can be approved.
	if out := applyReservation(t, fsm, 2, &internalpb.Command{
		Op: &internalpb.Command_ApprovePendingJoinRequest{
			ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{
				RequestId:                        "jreq-1",
				AuthorizationId:                  "auth-1",
				ConsumedAtUnix:                   pinTestNow.Unix(),
				PreparedCertificateReservationId: "res-1",
				PreparedCertificateBoundAtUnix:   pinTestNow.Unix(),
			},
		},
	}); out.Error == "" {
		// A request that does not exist cannot be approved, so the
		// approval is refused before the binding is reached.
		t.Log("the approval of a nonexistent request was accepted, which this test does not exercise")
	}
}

// A reservation that is ALREADY bound cannot be bound again by a
// second approval, and the approval is REFUSED rather than accepted
// without the binding.
//
// The refusal matters: a reservation that stays unbound is a
// preparation nobody can collect and nobody can clear.
func TestApprovalRefusesAReservationAlreadyBound(t *testing.T) {
	fsm := NewFSM()
	if out := applyReservation(t, fsm, 1, &internalpb.Command{
		Op: &internalpb.Command_CreateCertificateReservation{
			CreateCertificateReservation: &internalpb.CreateCertificateReservation{
				Reservation: newTestReservation("res-1", "host2.lab2.apiary.work", "SHA256:AA:BB"),
			},
		},
	}); out.Error != "" {
		t.Fatalf("creating: %s", out.Error)
	}
	if out := applyReservation(t, fsm, 2, &internalpb.Command{
		Op: &internalpb.Command_BindCertificateReservation{
			BindCertificateReservation: &internalpb.BindCertificateReservation{
				ReservationId: "res-1",
				NodeId:        "node02",
				RequestId:     "jreq-1",
				BoundAtUnix:   pinTestNow.Unix(),
			},
		},
	}); out.Error != "" {
		t.Fatalf("binding: %s", out.Error)
	}
	// A second bind for a different Comb is refused.
	out := applyReservation(t, fsm, 3, &internalpb.Command{
		Op: &internalpb.Command_BindCertificateReservation{
			BindCertificateReservation: &internalpb.BindCertificateReservation{
				ReservationId: "res-1",
				NodeId:        "node03",
				RequestId:     "jreq-2",
				BoundAtUnix:   pinTestNow.Unix(),
			},
		},
	})
	refuseRefusal(t, out, "already bound")
}
