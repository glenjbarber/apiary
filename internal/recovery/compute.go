package recovery

import "fmt"

// BuildHandbook is pure and deterministic - the same Inputs always
// produce the same Steps in the same order. Callers must pass a Quorum
// that already passed ValidQuorumFact; BuildHandbook does not re-check
// it. Step order is fixed and every branch below is a distinct,
// separately-tested rule:
//
//  1. Quorum status - always first.
//  2. One step per ReplicaBacked fact.
//  3. One step per OwnedResources fact where ReplicaConfigured.
//  4. One step per OwnedResources fact where NOT ReplicaConfigured.
//  5. One step per Images fact whose Verdict != ImageAvailable.
func BuildHandbook(in Inputs) Handbook {
	verdict := ClassifyQuorum(in.Quorum, in.IsCurrentLeader)

	var steps []Step
	order := 1

	steps = append(steps, quorumStep(order, verdict, in.IsCurrentLeader, in.Quorum))
	order++

	for _, r := range in.ReplicaBacked {
		steps = append(steps, replicaBackedStep(order, r))
		order++
	}

	for _, r := range in.OwnedResources {
		if !r.ReplicaConfigured {
			continue
		}
		steps = append(steps, migrationStep(order, r, verdict))
		order++
	}

	for _, r := range in.OwnedResources {
		if r.ReplicaConfigured {
			continue
		}
		steps = append(steps, unprotectedStep(order, r))
		order++
	}

	for _, img := range in.Images {
		if img.Verdict == ImageAvailable {
			continue
		}
		steps = append(steps, imageStep(order, img))
		order++
	}

	return Handbook{TargetNodeID: in.TargetNodeID, QuorumVerdict: verdict, Steps: steps}
}

func quorumStep(order int, verdict QuorumVerdict, isCurrentLeader bool, f QuorumFact) Step {
	switch verdict {
	case QuorumSurvives:
		return Step{
			Order: order,
			Title: "Quorum will survive",
			Detail: fmt.Sprintf(
				"%d of %d remaining voter(s) are confirmed reachable, meeting the required quorum size of %d.",
				f.RemainingReachable, f.RemainingVoters, f.QuorumSize,
			),
		}
	case QuorumUnknown:
		detail := fmt.Sprintf(
			"The confirmed-reachable count (%d of %d remaining voters, quorum requires %d) falls short, but %d voter(s) have unverified reachability - the deficit could close if they turn out reachable.",
			f.RemainingReachable, f.RemainingVoters, f.QuorumSize, f.RemainingUnknown,
		)
		if isCurrentLeader {
			detail = "This node currently holds raft leadership. The reachability data behind this quorum check was gathered by calls answered by the leader itself - it confirms the leader can reach each remaining voter, but NOT that the remaining voters can reach EACH OTHER, which is what a new election actually requires. " + detail
		}
		return Step{
			Order:  order,
			Title:  "Quorum status is UNKNOWN",
			Detail: detail,
			StopCondition: "Independently confirm remaining-voter connectivity before assuming quorum will be restored " +
				"automatically - do not perform Step 3's migration or any other raft-mutating action below until quorum reads SURVIVES.",
		}
	default: // QuorumLost
		return Step{
			Order: order,
			Title: "Quorum is LOST",
			Detail: fmt.Sprintf(
				"Even crediting every voter with unknown reachability as reachable (%d confirmed + %d unknown = %d of %d remaining voters), a majority cannot be reached - quorum requires %d.",
				f.RemainingReachable, f.RemainingUnknown, f.RemainingReachable+f.RemainingUnknown, f.RemainingVoters, f.QuorumSize,
			),
			StopCondition: "Do not proceed with any other recovery action below, including Step 3's migration, until quorum is restored.",
		}
	}
}

func replicaBackedStep(order int, r ReplicaBackedFact) Step {
	return Step{
		Order: order,
		Title: fmt.Sprintf("%s %q is replica-backed here, owned elsewhere", r.Kind, r.Name),
		Detail: fmt.Sprintf(
			"This %s keeps running unaffected on %s, its real owner. It loses its HAST redundancy until a new replica is configured elsewhere - no immediate action is required unless %s also fails.",
			r.Kind, r.OwnerNodeID, r.OwnerNodeID,
		),
	}
}

func migrationStep(order int, r ResourceFact, verdict QuorumVerdict) Step {
	hastName := HASTResourceName(r.Kind, r.ID)
	migrateCmd := "MigrateVM"
	if r.Kind == "jail" {
		migrateCmd = "MigrateJail"
	}

	var quorumNote string
	if verdict != QuorumSurvives {
		quorumNote = fmt.Sprintf(
			"Quorum is %s (see Step 1) - do not perform the migration below until quorum is confirmed SURVIVES; "+
				"a raft write attempted without real quorum can silently fail, be rejected, or apply against a leader "+
				"that does not actually have legitimate quorum support. ",
			verdictWord(verdict),
		)
	}

	detail := quorumNote + fmt.Sprintf(
		"Recover %q via Apiary's %s operation - do not manually run `hastctl role`. "+
			"(a) Fence the old owner - specific, enumerated evidence only, narrowed to what Apiary's own "+
			"host-local-disk HAST architecture makes meaningful (there is no shared storage array to revoke "+
			"access to, and a pulled network cable proves nothing about whether the old owner can still write "+
			"to its own local disk). Acceptable: confirmed power-off via independent out-of-band management "+
			"(IPMI/BMC or equivalent), or physical removal of the old owner's storage device confirmed on-site. "+
			"NOT acceptable: a failed ping, a failed SSH attempt, managerd/raftd being unreachable from this "+
			"report, or a disconnected network cable alone - none of these prove the host cannot still write "+
			"locally, and an unreachable-but-still-writing primary plus a promoted replica is a split-brain. "+
			"Record the fencing method, evidence, operator, and timestamp before proceeding. "+
			"(b) Verify secondary sync: run `hastctl list %s` on %s and confirm it shows `status: complete` "+
			"with zero dirty extents (Apiary's own tooling does not surface this over RPC - internal/hast.Manager.Status "+
			"only ever runs hastctl locally, invoked only by the reconciler itself). If the resource is "+
			"disconnected, uninitialized, or unable to identify its peer, that is NOT sufficient proof - report "+
			"the last known-successful reconciliation timestamp instead and stop. "+
			"(c) Only once (a), (b), and a SURVIVES quorum verdict are all independently confirmed, call %s "+
			"with target_node_id=%s - never a direct `hastctl role primary` call, since %s (ADR-0028) is the "+
			"reviewed path that lets the existing reconciler handle old-owner teardown/demotion and new-owner "+
			"promotion/startup safely, rather than a manual role change racing whatever the reconciler is doing. "+
			"(d) After migration, verify ownership swapped as expected, the cell actually started, its network "+
			"is up, and any declared health check passes.",
		r.Name, migrateCmd, hastName, r.ReplicaNodeID, migrateCmd, r.ReplicaNodeID, migrateCmd,
	)

	return Step{
		Order:        order,
		Title:        fmt.Sprintf("Recover %s %q via %s", r.Kind, r.Name, migrateCmd),
		Detail:       detail,
		Irreversible: true,
		StopCondition: fmt.Sprintf(
			"Do not invoke %s unless ALL THREE hold: quorum reads SURVIVES (Step 1), fencing evidence above is "+
				"independently confirmed, and secondary sync is independently confirmed per (b) above. Apiary "+
				"verifies none of these three automatically. Never run `hastctl role primary` directly - it "+
				"bypasses the reconciler and risks a state Apiary's own tracking no longer matches reality.",
			migrateCmd,
		),
	}
}

func verdictWord(v QuorumVerdict) string {
	switch v {
	case QuorumLost:
		return "LOST"
	case QuorumUnknown:
		return "UNKNOWN"
	default:
		return string(v)
	}
}

func unprotectedStep(order int, r ResourceFact) Step {
	return Step{
		Order: order,
		Title: fmt.Sprintf("No redundancy for %s %q", r.Kind, r.Name),
		Detail: fmt.Sprintf(
			"No HAST replica is configured for this %s. It cannot be automatically recovered by Apiary if the "+
				"target hive is gone for good - rebuilding requires its base image/ISO (see the image availability "+
				"findings below) or an out-of-band backup outside Apiary's own tracking. This is a real data-loss risk.",
			r.Kind,
		),
	}
}

func imageStep(order int, img ImageFact) Step {
	if img.Verdict == ImageUnknown {
		return Step{
			Order: order,
			Title: fmt.Sprintf("Verify image %q before relying on it for %q", img.ImageName, img.ResourceName),
			Detail: fmt.Sprintf(
				"No remaining source for image %q (needed to rebuild %q) was confirmed, but at least one "+
					"remaining hive's inventory could not be read. Verify a source is actually reachable before "+
					"relying on this image for a rebuild - absence of proof is not proof of absence.",
				img.ImageName, img.ResourceName,
			),
		}
	}
	return Step{
		Order: order,
		Title: fmt.Sprintf("Image %q is unavailable for %q", img.ImageName, img.ResourceName),
		Detail: fmt.Sprintf(
			"No remaining hive reports image %q. A rebuild of %q needing this image is blocked until a source "+
				"is restored.",
			img.ImageName, img.ResourceName,
		),
	}
}
